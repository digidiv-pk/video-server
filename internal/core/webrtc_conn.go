package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/pkg/ringbuffer"
	"github.com/aler9/gortsplib/pkg/rtpcodecs/rtph264"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"

	"github.com/aler9/rtsp-simple-server/internal/conf"
	"github.com/aler9/rtsp-simple-server/internal/logger"
)

type webRTCConnPathManager interface {
	readerAdd(req pathReaderAddReq) pathReaderSetupPlayRes
}

type webRTCConnParent interface {
	log(logger.Level, string, ...interface{})
	connClose(*webRTCConn)
}

type webRTCConn struct {
	readBufferCount int
	pathName        string
	wsconn          *websocket.Conn
	stunServers     []string
	wg              *sync.WaitGroup
	pathManager     webRTCConnPathManager
	parent          webRTCConnParent

	ctx        context.Context
	ctxCancel  func()
	ringBuffer *ringbuffer.RingBuffer
}

func newWebRTCConn(parentCtx context.Context, readBufferCount int, pathName string, wsconn *websocket.Conn, stunServers []string, wg *sync.WaitGroup, pathManager webRTCConnPathManager, parent webRTCConnParent) *webRTCConn {
	ctx, ctxCancel := context.WithCancel(parentCtx)

	c := &webRTCConn{
		readBufferCount: readBufferCount,
		pathName:        pathName,
		wsconn:          wsconn,
		stunServers:     stunServers,
		wg:              wg,
		pathManager:     pathManager,
		parent:          parent,
		ctx:             ctx,
		ctxCancel:       ctxCancel,
	}

	c.log(logger.Info, "opened")

	wg.Add(1)
	go c.run()

	return c
}

func (c *webRTCConn) close() {
	c.ctxCancel()
}

func (c *webRTCConn) log(level logger.Level, format string, args ...interface{}) {
	c.parent.log(level, "[conn %v] "+format, append([]interface{}{c.wsconn.RemoteAddr()}, args...)...)
}

func (c *webRTCConn) run() {
	defer c.wg.Done()

	ctx, cancel := context.WithCancel(c.ctx)
	runErr := make(chan error)
	go func() {
		runErr <- c.runInner(ctx)
	}()

	var err error
	select {
	case err = <-runErr:
		cancel()

	case <-c.ctx.Done():
		cancel()
		<-runErr
		err = errors.New("terminated")
	}

	c.ctxCancel()

	c.parent.connClose(c)

	c.log(logger.Info, "closed (%v)", err)
}

func (c *webRTCConn) runInner(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		c.wsconn.Close()
	}()

	res := c.pathManager.readerAdd(pathReaderAddReq{
		author:   c,
		pathName: c.pathName,
		authenticate: func(pathIPs []fmt.Stringer, pathUser conf.Credential, pathPass conf.Credential) error {
			return nil
		},
	})
	if res.err != nil {
		return res.err
	}

	path := res.path

	defer func() {
		path.readerRemove(pathReaderRemoveReq{author: c})
	}()

	var videoTrack *gortsplib.TrackH264
	var audioTrack *gortsplib.TrackOpus
	videoTrackID := -1
	audioTrackID := -1
	for i, track := range res.stream.tracks() {
		log.Print(track)
		fmt.Printf("track = %T\n", track)
		if tt, ok := track.(*gortsplib.TrackH264); ok {
			if videoTrack != nil {
				return fmt.Errorf("can't read track %d with WebRTC: too many tracks", i+1)
			}

			videoTrack = tt
			videoTrackID = i
		} else if tt, ok := track.(*gortsplib.TrackOpus); ok {
			if audioTrack != nil {
				return fmt.Errorf("can't read track %d with WebRTC: too many tracks", i+1)
			}

			audioTrack = tt
			audioTrackID = i
		}
	}

	if videoTrack == nil {
		return fmt.Errorf("the stream doesn't contain a supported track")
	}

	iceServers := c.iceServers()

	err := c.writeICEServers(iceServers)
	if err != nil {
		return err
	}

	offer, err := c.readOffer()
	if err != nil {
		return err
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		return err
	}
	defer pc.Close()

	answerWritten := make(chan struct{})
	pcConnected := make(chan struct{})
	pcTerminated := make(chan struct{})
	defer close(pcTerminated)

	pc.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i != nil {
			select {
			case <-answerWritten:
			case <-pcTerminated:
				return
			}

			select {
			case <-pcConnected:
				return
			case <-pcTerminated:
				return
			default:
			}

			c.writeCandidate(i)
		}
	})

	trackVideo, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeH264,
		ClockRate: 90000,
	}, "video", "rtspss")
	if err != nil {
		return err
	}

	trackAudio, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 90000,
	}, "audio", "rtspsa")
	if err != nil {
		return err
	}

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		c.log(logger.Info, "WebRTC state: "+s.String())

		if s == webrtc.PeerConnectionStateConnected {
			close(pcConnected)
			go c.runWriter(path, videoTrack, videoTrackID, audioTrack, audioTrackID, trackVideo, trackAudio, pc)
		}
	})

	_, err = pc.AddTrack(trackVideo)
	if err != nil {
		return err
	}

	if audioTrackID != -1 {
		_, err = pc.AddTrack(trackAudio)
		if err != nil {
			return err
		}
	}

	err = pc.SetRemoteDescription(*offer)
	if err != nil {
		return err
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return err
	}

	err = pc.SetLocalDescription(answer)
	if err != nil {
		return err
	}

	err = c.writeAnswer(&answer)
	if err != nil {
		return err
	}
	close(answerWritten)

	for {
		candidate, err := c.readCandidate()
		if err != nil {
			return err
		}

		select {
		case <-pcConnected:
			continue
		default:
		}

		err = pc.AddICECandidate(*candidate)
		if err != nil {
			return err
		}
	}
}

func (c *webRTCConn) runWriter(path *path, videoTrack *gortsplib.TrackH264, videoTrackID int, audioTrack *gortsplib.TrackOpus, audioTrackID int, webrtcVideoTrack *webrtc.TrackLocalStaticRTP, webrtcAudioTrack *webrtc.TrackLocalStaticRTP, pc *webrtc.PeerConnection) {
	c.ringBuffer, _ = ringbuffer.New(uint64(c.readBufferCount))
	go func() {
		<-c.ctx.Done()
		c.ringBuffer.Close()
	}()

	path.readerStart(pathReaderStartReq{
		author: c,
	})

	tracks := gortsplib.Tracks{videoTrack, audioTrack}

	c.log(logger.Info, "is reading from path '%s', %s", path.Name(), sourceTrackInfo(tracks))

	encoder := &rtph264.Encoder{
		PayloadType:    96,
		PayloadMaxSize: 1200,
	}
	encoder.Init()

	for {
		item, ok := c.ringBuffer.Pull()
		if !ok {
			return
		}
		data := item.(data)

		if videoTrack != nil && data.getTrackID() == videoTrackID {
			tdata := data.(*dataH264)
			if tdata.nalus == nil {
				continue
			}

			packets, err := encoder.Encode(tdata.nalus, tdata.pts)
			if err != nil {
				continue
			}

			for _, pkt := range packets {
				webrtcVideoTrack.WriteRTP(pkt)
			}
		} else if audioTrack != nil && data.getTrackID() == audioTrackID {
			tdata := data.(*dataGeneric)
			if tdata.rtpPackets == nil {
				continue
			}
			for _, pkt := range tdata.rtpPackets {
				webrtcAudioTrack.WriteRTP(pkt)
			}

		}
	}
}

func (c *webRTCConn) iceServers() []webrtc.ICEServer {
	ret := make([]webrtc.ICEServer, len(c.stunServers))
	for i, s := range c.stunServers {
		ret[i] = webrtc.ICEServer{
			URLs: []string{"stun:" + s},
		}
	}
	return ret
}

func (c *webRTCConn) writeICEServers(iceServers []webrtc.ICEServer) error {
	enc, _ := json.Marshal(iceServers)
	return c.wsconn.WriteMessage(websocket.TextMessage, enc)
}

func (c *webRTCConn) readOffer() (*webrtc.SessionDescription, error) {
	_, enc, err := c.wsconn.ReadMessage()
	if err != nil {
		return nil, err
	}

	var offer webrtc.SessionDescription
	err = json.Unmarshal(enc, &offer)
	if err != nil {
		return nil, err
	}

	if offer.Type != webrtc.SDPTypeOffer {
		return nil, fmt.Errorf("received SDP is not an offer")
	}

	return &offer, nil
}

func (c *webRTCConn) writeAnswer(answer *webrtc.SessionDescription) error {
	enc, _ := json.Marshal(answer)
	return c.wsconn.WriteMessage(websocket.TextMessage, enc)
}

func (c *webRTCConn) writeCandidate(candidate *webrtc.ICECandidate) error {
	enc, _ := json.Marshal(candidate.ToJSON())
	return c.wsconn.WriteMessage(websocket.TextMessage, enc)
}

func (c *webRTCConn) readCandidate() (*webrtc.ICECandidateInit, error) {
	_, enc, err := c.wsconn.ReadMessage()
	if err != nil {
		return nil, err
	}

	var candidate webrtc.ICECandidateInit
	err = json.Unmarshal(enc, &candidate)
	if err != nil {
		return nil, err
	}

	return &candidate, err
}

// onReaderData implements reader.
func (c *webRTCConn) onReaderData(data data) {
	c.ringBuffer.Push(data)
}

// apiReaderDescribe implements reader.
func (c *webRTCConn) apiReaderDescribe() interface{} {
	return struct {
		Type string `json:"type"`
	}{"webRTCConn"}
}
