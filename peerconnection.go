// +build !js

// Package webrtc implements the WebRTC 1.0 as defined in W3C WebRTC specification document.
package webrtc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	mathRand "math/rand"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v2"

	"github.com/pion/webrtc/v2/internal/util"
	"github.com/pion/webrtc/v2/pkg/rtcerr"
)

// PeerConnection represents a WebRTC connection that establishes a
// peer-to-peer communications with another PeerConnection instance in a
// browser, or to another endpoint implementing the required protocols.
type PeerConnection struct {
	statsID string
	mu      sync.RWMutex

	configuration Configuration

	currentLocalDescription  *SessionDescription
	pendingLocalDescription  *SessionDescription
	currentRemoteDescription *SessionDescription
	pendingRemoteDescription *SessionDescription
	signalingState           SignalingState
	iceConnectionState       ICEConnectionState
	connectionState          PeerConnectionState

	idpLoginURL *string

	isClosed          bool
	negotiationNeeded bool

	lastOffer  string
	lastAnswer string

	rtpTransceivers []*RTPTransceiver

	// DataChannels
	dataChannels          map[uint16]*DataChannel
	dataChannelsOpened    uint32
	dataChannelsRequested uint32
	dataChannelsAccepted  uint32

	onSignalingStateChangeHandler     func(SignalingState)
	onICEConnectionStateChangeHandler func(ICEConnectionState)
	onTrackHandler                    func(*Track, *RTPReceiver)
	onDataChannelHandler              func(*DataChannel)

	iceGatherer   *ICEGatherer
	iceTransport  *ICETransport
	dtlsTransport *DTLSTransport
	sctpTransport *SCTPTransport

	// A reference to the associated API state used by this connection
	api *API
	log logging.LeveledLogger
}

// NewPeerConnection creates a peerconnection with the default
// codecs. See API.NewRTCPeerConnection for details.
func NewPeerConnection(configuration Configuration) (*PeerConnection, error) {
	m := MediaEngine{}
	m.RegisterDefaultCodecs()
	api := NewAPI(WithMediaEngine(m))
	return api.NewPeerConnection(configuration)
}

// NewPeerConnection creates a new PeerConnection with the provided configuration against the received API object
func (api *API) NewPeerConnection(configuration Configuration) (*PeerConnection, error) {
	// https://w3c.github.io/webrtc-pc/#constructor (Step #2)
	// Some variables defined explicitly despite their implicit zero values to
	// allow better readability to understand what is happening.
	pc := &PeerConnection{
		statsID: fmt.Sprintf("PeerConnection-%d", time.Now().UnixNano()),
		configuration: Configuration{
			ICEServers:           []ICEServer{},
			ICETransportPolicy:   ICETransportPolicyAll,
			BundlePolicy:         BundlePolicyBalanced,
			RTCPMuxPolicy:        RTCPMuxPolicyRequire,
			Certificates:         []Certificate{},
			ICECandidatePoolSize: 0,
		},
		isClosed:           false,
		negotiationNeeded:  false,
		lastOffer:          "",
		lastAnswer:         "",
		signalingState:     SignalingStateStable,
		iceConnectionState: ICEConnectionStateNew,
		connectionState:    PeerConnectionStateNew,
		dataChannels:       make(map[uint16]*DataChannel),

		api: api,
		log: api.settingEngine.LoggerFactory.NewLogger("pc"),
	}

	var err error
	if err = pc.initConfiguration(configuration); err != nil {
		return nil, err
	}

	pc.iceGatherer, err = pc.createICEGatherer()
	if err != nil {
		return nil, err
	}

	if !pc.iceGatherer.agentIsTrickle {
		if err = pc.iceGatherer.Gather(); err != nil {
			return nil, err
		}
	}

	// Create the ice transport
	iceTransport := pc.createICETransport()
	pc.iceTransport = iceTransport

	// Create the DTLS transport
	dtlsTransport, err := pc.api.NewDTLSTransport(pc.iceTransport, pc.configuration.Certificates)
	if err != nil {
		return nil, err
	}
	pc.dtlsTransport = dtlsTransport

	return pc, nil
}

// initConfiguration defines validation of the specified Configuration and
// its assignment to the internal configuration variable. This function differs
// from its SetConfiguration counterpart because most of the checks do not
// include verification statements related to the existing state. Thus the
// function describes only minor verification of some the struct variables.
func (pc *PeerConnection) initConfiguration(configuration Configuration) error {
	if configuration.PeerIdentity != "" {
		pc.configuration.PeerIdentity = configuration.PeerIdentity
	}

	// https://www.w3.org/TR/webrtc/#constructor (step #3)
	if len(configuration.Certificates) > 0 {
		now := time.Now()
		for _, x509Cert := range configuration.Certificates {
			if !x509Cert.Expires().IsZero() && now.After(x509Cert.Expires()) {
				return &rtcerr.InvalidAccessError{Err: ErrCertificateExpired}
			}
			pc.configuration.Certificates = append(pc.configuration.Certificates, x509Cert)
		}
	} else {
		sk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return &rtcerr.UnknownError{Err: err}
		}
		certificate, err := GenerateCertificate(sk)
		if err != nil {
			return err
		}
		pc.configuration.Certificates = []Certificate{*certificate}
	}

	if configuration.BundlePolicy != BundlePolicy(Unknown) {
		pc.configuration.BundlePolicy = configuration.BundlePolicy
	}

	if configuration.RTCPMuxPolicy != RTCPMuxPolicy(Unknown) {
		pc.configuration.RTCPMuxPolicy = configuration.RTCPMuxPolicy
	}

	if configuration.ICECandidatePoolSize != 0 {
		pc.configuration.ICECandidatePoolSize = configuration.ICECandidatePoolSize
	}

	if configuration.ICETransportPolicy != ICETransportPolicy(Unknown) {
		pc.configuration.ICETransportPolicy = configuration.ICETransportPolicy
	}

	if configuration.SDPSemantics != SDPSemantics(Unknown) {
		pc.configuration.SDPSemantics = configuration.SDPSemantics
	}

	if len(configuration.ICEServers) > 0 {
		for _, server := range configuration.ICEServers {
			if err := server.validate(); err != nil {
				return err
			}
		}
		pc.configuration.ICEServers = configuration.ICEServers
	}

	return nil
}

// OnSignalingStateChange sets an event handler which is invoked when the
// peer connection's signaling state changes
func (pc *PeerConnection) OnSignalingStateChange(f func(SignalingState)) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.onSignalingStateChangeHandler = f
}

func (pc *PeerConnection) onSignalingStateChange(newState SignalingState) (done chan struct{}) {
	pc.mu.RLock()
	hdlr := pc.onSignalingStateChangeHandler
	pc.mu.RUnlock()

	pc.log.Infof("signaling state changed to %s", newState)
	done = make(chan struct{})
	if hdlr == nil {
		close(done)
		return
	}

	go func() {
		hdlr(newState)
		close(done)
	}()

	return
}

// OnDataChannel sets an event handler which is invoked when a data
// channel message arrives from a remote peer.
func (pc *PeerConnection) OnDataChannel(f func(*DataChannel)) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.onDataChannelHandler = f
}

// OnICECandidate sets an event handler which is invoked when a new ICE
// candidate is found.
func (pc *PeerConnection) OnICECandidate(f func(*ICECandidate)) {
	pc.iceGatherer.OnLocalCandidate(f)
}

// OnICEGatheringStateChange sets an event handler which is invoked when the
// ICE candidate gathering state has changed.
func (pc *PeerConnection) OnICEGatheringStateChange(f func(ICEGathererState)) {
	pc.iceGatherer.OnStateChange(f)
}

// OnTrack sets an event handler which is called when remote track
// arrives from a remote peer.
func (pc *PeerConnection) OnTrack(f func(*Track, *RTPReceiver)) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.onTrackHandler = f
}

func (pc *PeerConnection) onTrack(t *Track, r *RTPReceiver) (done chan struct{}) {
	pc.mu.RLock()
	hdlr := pc.onTrackHandler
	pc.mu.RUnlock()

	pc.log.Debugf("got new track: %+v", t)
	done = make(chan struct{})
	if hdlr == nil || t == nil {
		close(done)
		return
	}

	go func() {
		hdlr(t, r)
		close(done)
	}()

	return
}

// OnICEConnectionStateChange sets an event handler which is called
// when an ICE connection state is changed.
func (pc *PeerConnection) OnICEConnectionStateChange(f func(ICEConnectionState)) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.onICEConnectionStateChangeHandler = f
}

func (pc *PeerConnection) onICEConnectionStateChange(cs ICEConnectionState) (done chan struct{}) {
	pc.mu.RLock()
	hdlr := pc.onICEConnectionStateChangeHandler
	pc.mu.RUnlock()

	pc.log.Infof("ICE connection state changed: %s", cs)
	done = make(chan struct{})
	if hdlr == nil {
		close(done)
		return
	}

	go func() {
		hdlr(cs)
		close(done)
	}()

	return
}

// SetConfiguration updates the configuration of this PeerConnection object.
func (pc *PeerConnection) SetConfiguration(configuration Configuration) error {
	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-setconfiguration (step #2)
	if pc.isClosed {
		return &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #3)
	if configuration.PeerIdentity != "" {
		if configuration.PeerIdentity != pc.configuration.PeerIdentity {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingPeerIdentity}
		}
		pc.configuration.PeerIdentity = configuration.PeerIdentity
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #4)
	if len(configuration.Certificates) > 0 {
		if len(configuration.Certificates) != len(pc.configuration.Certificates) {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingCertificates}
		}

		for i, certificate := range configuration.Certificates {
			if !pc.configuration.Certificates[i].Equals(certificate) {
				return &rtcerr.InvalidModificationError{Err: ErrModifyingCertificates}
			}
		}
		pc.configuration.Certificates = configuration.Certificates
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #5)
	if configuration.BundlePolicy != BundlePolicy(Unknown) {
		if configuration.BundlePolicy != pc.configuration.BundlePolicy {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingBundlePolicy}
		}
		pc.configuration.BundlePolicy = configuration.BundlePolicy
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #6)
	if configuration.RTCPMuxPolicy != RTCPMuxPolicy(Unknown) {
		if configuration.RTCPMuxPolicy != pc.configuration.RTCPMuxPolicy {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingRTCPMuxPolicy}
		}
		pc.configuration.RTCPMuxPolicy = configuration.RTCPMuxPolicy
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #7)
	if configuration.ICECandidatePoolSize != 0 {
		if pc.configuration.ICECandidatePoolSize != configuration.ICECandidatePoolSize &&
			pc.LocalDescription() != nil {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingICECandidatePoolSize}
		}
		pc.configuration.ICECandidatePoolSize = configuration.ICECandidatePoolSize
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #8)
	if configuration.ICETransportPolicy != ICETransportPolicy(Unknown) {
		pc.configuration.ICETransportPolicy = configuration.ICETransportPolicy
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #11)
	if len(configuration.ICEServers) > 0 {
		// https://www.w3.org/TR/webrtc/#set-the-configuration (step #11.3)
		for _, server := range configuration.ICEServers {
			if err := server.validate(); err != nil {
				return err
			}
		}
		pc.configuration.ICEServers = configuration.ICEServers
	}
	return nil
}

// GetConfiguration returns a Configuration object representing the current
// configuration of this PeerConnection object. The returned object is a
// copy and direct mutation on it will not take affect until SetConfiguration
// has been called with Configuration passed as its only argument.
// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-getconfiguration
func (pc *PeerConnection) GetConfiguration() Configuration {
	return pc.configuration
}

func (pc *PeerConnection) getStatsID() string {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	return pc.statsID
}

// CreateOffer starts the PeerConnection and generates the localDescription
func (pc *PeerConnection) CreateOffer(options *OfferOptions) (SessionDescription, error) {
	useIdentity := pc.idpLoginURL != nil
	switch {
	case options != nil:
		return SessionDescription{}, fmt.Errorf("TODO handle options")
	case useIdentity:
		return SessionDescription{}, fmt.Errorf("TODO handle identity provider")
	case pc.isClosed:
		return SessionDescription{}, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	d := sdp.NewJSEPSessionDescription(useIdentity)
	if err := pc.addFingerprint(d); err != nil {
		return SessionDescription{}, err
	}

	iceParams, err := pc.iceGatherer.GetLocalParameters()
	if err != nil {
		return SessionDescription{}, err
	}

	candidates, err := pc.iceGatherer.GetLocalCandidates()
	if err != nil {
		return SessionDescription{}, err
	}

	bundleValue := "BUNDLE"
	bundleCount := 0
	appendBundle := func(midValue string) {
		bundleValue += " " + midValue
		bundleCount++
	}

	if pc.configuration.SDPSemantics == SDPSemanticsPlanB {
		video := make([]*RTPTransceiver, 0)
		audio := make([]*RTPTransceiver, 0)
		for _, t := range pc.GetTransceivers() {
			switch t.kind {
			case RTPCodecTypeVideo:
				video = append(video, t)
			case RTPCodecTypeAudio:
				audio = append(audio, t)
			}
		}

		if len(video) > 0 {
			if err = pc.addTransceiverSDP(d, "video", iceParams, candidates, sdp.ConnectionRoleActpass, video...); err != nil {
				return SessionDescription{}, err
			}
			appendBundle("video")
		}
		if len(audio) > 0 {
			if err = pc.addTransceiverSDP(d, "audio", iceParams, candidates, sdp.ConnectionRoleActpass, audio...); err != nil {
				return SessionDescription{}, err
			}
			appendBundle("audio")
		}
	} else {
		for _, t := range pc.GetTransceivers() {
			midValue := strconv.Itoa(bundleCount)
			if err = pc.addTransceiverSDP(d, midValue, iceParams, candidates, sdp.ConnectionRoleActpass, t); err != nil {
				return SessionDescription{}, err
			}
			appendBundle(midValue)
		}
	}

	midValue := strconv.Itoa(bundleCount)
	if pc.configuration.SDPSemantics == SDPSemanticsPlanB {
		midValue = "data"
	}
	pc.addDataMediaSection(d, midValue, iceParams, candidates, sdp.ConnectionRoleActpass)
	appendBundle(midValue)

	d = d.WithValueAttribute(sdp.AttrKeyGroup, bundleValue)

	sdpBytes, err := d.Marshal()
	if err != nil {
		return SessionDescription{}, err
	}

	desc := SessionDescription{
		Type:   SDPTypeOffer,
		SDP:    string(sdpBytes),
		parsed: d,
	}
	pc.lastOffer = desc.SDP
	return desc, nil
}

func (pc *PeerConnection) createICEGatherer() (*ICEGatherer, error) {
	g, err := pc.api.NewICEGatherer(ICEGatherOptions{
		ICEServers:      pc.configuration.ICEServers,
		ICEGatherPolicy: pc.configuration.ICETransportPolicy,
	})
	if err != nil {
		return nil, err
	}

	return g, nil
}

func (pc *PeerConnection) createICETransport() *ICETransport {
	t := pc.api.NewICETransport(pc.iceGatherer)

	t.OnConnectionStateChange(func(state ICETransportState) {
		var cs ICEConnectionState
		switch state {
		case ICETransportStateNew:
			cs = ICEConnectionStateNew
		case ICETransportStateChecking:
			cs = ICEConnectionStateChecking
		case ICETransportStateConnected:
			cs = ICEConnectionStateConnected
		case ICETransportStateCompleted:
			cs = ICEConnectionStateCompleted
		case ICETransportStateFailed:
			cs = ICEConnectionStateFailed
		case ICETransportStateDisconnected:
			cs = ICEConnectionStateDisconnected
		case ICETransportStateClosed:
			cs = ICEConnectionStateClosed
		default:
			pc.log.Warnf("OnConnectionStateChange: unhandled ICE state: %s", state)
			return
		}
		pc.iceStateChange(cs)
	})

	return t
}

func (pc *PeerConnection) getPeerDirection(media *sdp.MediaDescription) RTPTransceiverDirection {
	for _, a := range media.Attributes {
		if direction := NewRTPTransceiverDirection(a.Key); direction != RTPTransceiverDirection(Unknown) {
			return direction
		}
	}
	return RTPTransceiverDirection(Unknown)
}

func (pc *PeerConnection) getMidValue(media *sdp.MediaDescription) string {
	for _, attr := range media.Attributes {
		if attr.Key == "mid" {
			return attr.Value
		}
	}
	return ""
}

// Given a direction+type pluck a transceiver from the passed list
// if no entry satisfies the requested type+direction return a inactive Transceiver
func satisfyTypeAndDirection(remoteKind RTPCodecType, remoteDirection RTPTransceiverDirection, localTransceivers []*RTPTransceiver) (*RTPTransceiver, []*RTPTransceiver) {
	// Get direction order from most preferred to least
	getPreferredDirections := func() []RTPTransceiverDirection {
		switch remoteDirection {
		case RTPTransceiverDirectionSendrecv:
			return []RTPTransceiverDirection{RTPTransceiverDirectionRecvonly, RTPTransceiverDirectionSendrecv}
		case RTPTransceiverDirectionSendonly:
			return []RTPTransceiverDirection{RTPTransceiverDirectionRecvonly, RTPTransceiverDirectionSendrecv}
		case RTPTransceiverDirectionRecvonly:
			return []RTPTransceiverDirection{RTPTransceiverDirectionSendonly, RTPTransceiverDirectionSendrecv}
		}
		return []RTPTransceiverDirection{}
	}

	for _, possibleDirection := range getPreferredDirections() {
		for i := range localTransceivers {
			t := localTransceivers[i]
			if t.kind != remoteKind || possibleDirection != t.Direction {
				continue
			}

			return t, append(localTransceivers[:i], localTransceivers[i+1:]...)
		}
	}

	return &RTPTransceiver{
		kind:      remoteKind,
		Direction: RTPTransceiverDirectionInactive,
	}, localTransceivers
}

func (pc *PeerConnection) addAnswerMediaTransceivers(d *sdp.SessionDescription) (*sdp.SessionDescription, error) {
	iceParams, err := pc.iceGatherer.GetLocalParameters()
	if err != nil {
		return nil, err
	}

	candidates, err := pc.iceGatherer.GetLocalCandidates()
	if err != nil {
		return nil, err
	}

	bundleValue := "BUNDLE"
	appendBundle := func(midValue string) {
		bundleValue += " " + midValue
	}

	var t *RTPTransceiver
	localTransceivers := append([]*RTPTransceiver{}, pc.GetTransceivers()...)
	detectedPlanB := pc.descriptionIsPlanB(pc.RemoteDescription())

	for _, media := range pc.RemoteDescription().parsed.MediaDescriptions {
		midValue := pc.getMidValue(media)
		if midValue == "" {
			return nil, fmt.Errorf("RemoteDescription contained media section without mid value")
		}

		if media.MediaName.Media == "application" {
			pc.addDataMediaSection(d, midValue, iceParams, candidates, sdp.ConnectionRoleActive)
			appendBundle(midValue)
			continue
		}

		kind := NewRTPCodecType(media.MediaName.Media)
		direction := pc.getPeerDirection(media)
		if kind == 0 || direction == RTPTransceiverDirection(Unknown) {
			continue
		}

		t, localTransceivers = satisfyTypeAndDirection(kind, direction, localTransceivers)
		mediaTransceivers := []*RTPTransceiver{t}
		switch pc.configuration.SDPSemantics {
		case SDPSemanticsUnifiedPlanWithFallback:
			// If no match, process as unified-plan
			if !detectedPlanB {
				break
			}
			// If there was a match, fall through to plan-b
			fallthrough
		case SDPSemanticsPlanB:
			if !detectedPlanB {
				return nil, &rtcerr.TypeError{Err: ErrIncorrectSDPSemantics}
			}
			// If we're responding to a plan-b offer, then we should try to fill up this
			// media entry with all matching local transceivers
			for {
				// keep going until we can't get any more
				t, localTransceivers = satisfyTypeAndDirection(kind, direction, localTransceivers)
				if t.Direction == RTPTransceiverDirectionInactive {
					break
				}
				mediaTransceivers = append(mediaTransceivers, t)
			}
		case SDPSemanticsUnifiedPlan:
			if detectedPlanB {
				return nil, &rtcerr.TypeError{Err: ErrIncorrectSDPSemantics}
			}
		}
		if err := pc.addTransceiverSDP(d, midValue, iceParams, candidates, sdp.ConnectionRoleActive, mediaTransceivers...); err != nil {
			return nil, err
		}
		appendBundle(midValue)
	}

	if pc.configuration.SDPSemantics == SDPSemanticsUnifiedPlanWithFallback && detectedPlanB {
		pc.log.Info("Plan-B Offer detected; responding with Plan-B Answer")
	}

	return d.WithValueAttribute(sdp.AttrKeyGroup, bundleValue), nil
}

// CreateAnswer starts the PeerConnection and generates the localDescription
func (pc *PeerConnection) CreateAnswer(options *AnswerOptions) (SessionDescription, error) {
	useIdentity := pc.idpLoginURL != nil
	switch {
	case options != nil:
		return SessionDescription{}, fmt.Errorf("TODO handle options")
	case pc.RemoteDescription() == nil:
		return SessionDescription{}, &rtcerr.InvalidStateError{Err: ErrNoRemoteDescription}
	case useIdentity:
		return SessionDescription{}, fmt.Errorf("TODO handle identity provider")
	case pc.isClosed:
		return SessionDescription{}, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	d := sdp.NewJSEPSessionDescription(useIdentity)
	if err := pc.addFingerprint(d); err != nil {
		return SessionDescription{}, err
	}

	d, err := pc.addAnswerMediaTransceivers(d)
	if err != nil {
		return SessionDescription{}, err
	}

	sdpBytes, err := d.Marshal()
	if err != nil {
		return SessionDescription{}, err
	}

	desc := SessionDescription{
		Type:   SDPTypeAnswer,
		SDP:    string(sdpBytes),
		parsed: d,
	}
	pc.lastAnswer = desc.SDP
	return desc, nil
}

// 4.4.1.6 Set the SessionDescription
func (pc *PeerConnection) setDescription(sd *SessionDescription, op stateChangeOp) error {
	if pc.isClosed {
		return &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	cur := pc.signalingState
	setLocal := stateChangeOpSetLocal
	setRemote := stateChangeOpSetRemote
	newSDPDoesNotMatchOffer := &rtcerr.InvalidModificationError{Err: fmt.Errorf("new sdp does not match previous offer")}
	newSDPDoesNotMatchAnswer := &rtcerr.InvalidModificationError{Err: fmt.Errorf("new sdp does not match previous answer")}

	var nextState SignalingState
	var err error
	switch op {
	case setLocal:
		switch sd.Type {
		// stable->SetLocal(offer)->have-local-offer
		case SDPTypeOffer:
			if sd.SDP != pc.lastOffer {
				return newSDPDoesNotMatchOffer
			}
			nextState, err = checkNextSignalingState(cur, SignalingStateHaveLocalOffer, setLocal, sd.Type)
			if err == nil {
				pc.pendingLocalDescription = sd
			}
		// have-remote-offer->SetLocal(answer)->stable
		// have-local-pranswer->SetLocal(answer)->stable
		case SDPTypeAnswer:
			if sd.SDP != pc.lastAnswer {
				return newSDPDoesNotMatchAnswer
			}
			nextState, err = checkNextSignalingState(cur, SignalingStateStable, setLocal, sd.Type)
			if err == nil {
				pc.currentLocalDescription = sd
				pc.currentRemoteDescription = pc.pendingRemoteDescription
				pc.pendingRemoteDescription = nil
				pc.pendingLocalDescription = nil
			}
		case SDPTypeRollback:
			nextState, err = checkNextSignalingState(cur, SignalingStateStable, setLocal, sd.Type)
			if err == nil {
				pc.pendingLocalDescription = nil
			}
		// have-remote-offer->SetLocal(pranswer)->have-local-pranswer
		case SDPTypePranswer:
			if sd.SDP != pc.lastAnswer {
				return newSDPDoesNotMatchAnswer
			}
			nextState, err = checkNextSignalingState(cur, SignalingStateHaveLocalPranswer, setLocal, sd.Type)
			if err == nil {
				pc.pendingLocalDescription = sd
			}
		default:
			return &rtcerr.OperationError{Err: fmt.Errorf("invalid state change op: %s(%s)", op, sd.Type)}
		}
	case setRemote:
		switch sd.Type {
		// stable->SetRemote(offer)->have-remote-offer
		case SDPTypeOffer:
			nextState, err = checkNextSignalingState(cur, SignalingStateHaveRemoteOffer, setRemote, sd.Type)
			if err == nil {
				pc.pendingRemoteDescription = sd
			}
		// have-local-offer->SetRemote(answer)->stable
		// have-remote-pranswer->SetRemote(answer)->stable
		case SDPTypeAnswer:
			nextState, err = checkNextSignalingState(cur, SignalingStateStable, setRemote, sd.Type)
			if err == nil {
				pc.currentRemoteDescription = sd
				pc.currentLocalDescription = pc.pendingLocalDescription
				pc.pendingRemoteDescription = nil
				pc.pendingLocalDescription = nil
			}
		case SDPTypeRollback:
			nextState, err = checkNextSignalingState(cur, SignalingStateStable, setRemote, sd.Type)
			if err == nil {
				pc.pendingRemoteDescription = nil
			}
		// have-local-offer->SetRemote(pranswer)->have-remote-pranswer
		case SDPTypePranswer:
			nextState, err = checkNextSignalingState(cur, SignalingStateHaveRemotePranswer, setRemote, sd.Type)
			if err == nil {
				pc.pendingRemoteDescription = sd
			}
		default:
			return &rtcerr.OperationError{Err: fmt.Errorf("invalid state change op: %s(%s)", op, sd.Type)}
		}
	default:
		return &rtcerr.OperationError{Err: fmt.Errorf("unhandled state change op: %q", op)}
	}

	if err == nil {
		pc.signalingState = nextState
		pc.onSignalingStateChange(nextState)
	}
	return err
}

// SetLocalDescription sets the SessionDescription of the local peer
func (pc *PeerConnection) SetLocalDescription(desc SessionDescription) error {
	if pc.isClosed {
		return &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	// JSEP 5.4
	if desc.SDP == "" {
		switch desc.Type {
		case SDPTypeAnswer, SDPTypePranswer:
			desc.SDP = pc.lastAnswer
		case SDPTypeOffer:
			desc.SDP = pc.lastOffer
		default:
			return &rtcerr.InvalidModificationError{
				Err: fmt.Errorf("invalid SDP type supplied to SetLocalDescription(): %s", desc.Type),
			}
		}
	}

	desc.parsed = &sdp.SessionDescription{}
	if err := desc.parsed.Unmarshal([]byte(desc.SDP)); err != nil {
		return err
	}
	if err := pc.setDescription(&desc, stateChangeOpSetLocal); err != nil {
		return err
	}

	// To support all unittests which are following the future trickle=true
	// setup while also support the old trickle=false synchronous gathering
	// process this is necessary to avoid calling Garther() in multiple
	// pleces; which causes race conditions. (issue-707)
	if !pc.iceGatherer.agentIsTrickle {
		if err := pc.iceGatherer.SignalCandidates(); err != nil {
			return err
		}
		return nil
	}

	if desc.Type == SDPTypeAnswer {
		return pc.iceGatherer.Gather()
	}
	return nil
}

// LocalDescription returns pendingLocalDescription if it is not null and
// otherwise it returns currentLocalDescription. This property is used to
// determine if setLocalDescription has already been called.
// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-localdescription
func (pc *PeerConnection) LocalDescription() *SessionDescription {
	if localDescription := pc.PendingLocalDescription(); localDescription != nil {
		return localDescription
	}
	return pc.currentLocalDescription
}

// SetRemoteDescription sets the SessionDescription of the remote peer
func (pc *PeerConnection) SetRemoteDescription(desc SessionDescription) error { //nolint pion/webrtc#614
	if pc.currentRemoteDescription != nil { // pion/webrtc#207
		return fmt.Errorf("remoteDescription is already defined, SetRemoteDescription can only be called once")
	}
	if pc.isClosed {
		return &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	desc.parsed = &sdp.SessionDescription{}
	if err := desc.parsed.Unmarshal([]byte(desc.SDP)); err != nil {
		return err
	}
	if err := pc.setDescription(&desc, stateChangeOpSetRemote); err != nil {
		return err
	}

	weOffer := true
	remoteUfrag := ""
	remotePwd := ""
	if desc.Type == SDPTypeOffer {
		weOffer = false
	}

	fingerprint, haveFingerprint := desc.parsed.Attribute("fingerprint")
	for _, m := range pc.RemoteDescription().parsed.MediaDescriptions {
		if !haveFingerprint {
			fingerprint, haveFingerprint = m.Attribute("fingerprint")
		}

		for _, a := range m.Attributes {
			switch {
			case a.IsICECandidate():
				sdpCandidate, err := a.ToICECandidate()
				if err != nil {
					return err
				}

				candidate, err := newICECandidateFromSDP(sdpCandidate)
				if err != nil {
					return err
				}

				if err = pc.iceTransport.AddRemoteCandidate(candidate); err != nil {
					return err
				}
			case strings.HasPrefix(*a.String(), "ice-ufrag"):
				remoteUfrag = (*a.String())[len("ice-ufrag:"):]
			case strings.HasPrefix(*a.String(), "ice-pwd"):
				remotePwd = (*a.String())[len("ice-pwd:"):]
			}
		}
	}

	if !haveFingerprint {
		return fmt.Errorf("could not find fingerprint")
	}

	parts := strings.Split(fingerprint, " ")
	if len(parts) != 2 {
		return fmt.Errorf("invalid fingerprint")
	}
	fingerprint = parts[1]
	fingerprintHash := parts[0]

	// Create the SCTP transport
	sctp := pc.api.NewSCTPTransport(pc.dtlsTransport)
	pc.sctpTransport = sctp

	// Wire up the on datachannel handler
	sctp.OnDataChannel(func(d *DataChannel) {
		pc.mu.RLock()
		hdlr := pc.onDataChannelHandler
		pc.dataChannels[*d.ID()] = d
		pc.dataChannelsAccepted++
		pc.mu.RUnlock()
		if hdlr != nil {
			hdlr(d)
		}
	})

	// Wire up the on datachannel opened handler
	sctp.OnDataChannelOpened(func(d *DataChannel) {
		pc.mu.RLock()
		pc.dataChannelsOpened++
		pc.mu.RUnlock()
	})

	go func() {
		// Star the networking in a new routine since it will block until
		// the connection is actually established.

		// Start the ice transport
		iceRole := ICERoleControlled
		if weOffer {
			iceRole = ICERoleControlling
		}
		err := pc.iceTransport.Start(
			pc.iceGatherer,
			ICEParameters{
				UsernameFragment: remoteUfrag,
				Password:         remotePwd,
				ICELite:          false,
			},
			&iceRole,
		)

		if err != nil {
			// pion/webrtc#614
			pc.log.Warnf("Failed to start manager: %s", err)
			return
		}

		// Start the dtls transport
		err = pc.dtlsTransport.Start(DTLSParameters{
			Role:         dtlsRoleFromRemoteSDP(desc.parsed),
			Fingerprints: []DTLSFingerprint{{Algorithm: fingerprintHash, Value: fingerprint}},
		})
		if err != nil {
			// pion/webrtc#614
			pc.log.Warnf("Failed to start manager: %s", err)
			return
		}

		pc.openSRTP()

		for _, tranceiver := range pc.GetTransceivers() {
			if tranceiver.Sender != nil {
				err = tranceiver.Sender.Send(RTPSendParameters{
					Encodings: RTPEncodingParameters{
						RTPCodingParameters{
							SSRC:        tranceiver.Sender.track.SSRC(),
							PayloadType: tranceiver.Sender.track.PayloadType(),
						},
					}})

				if err != nil {
					pc.log.Warnf("Failed to start Sender: %s", err)
				}
			}
		}

		go pc.drainSRTP()

		// Start sctp
		err = pc.sctpTransport.Start(SCTPCapabilities{
			MaxMessageSize: 0,
		})
		if err != nil {
			// pion/webrtc#614
			pc.log.Warnf("Failed to start SCTP: %s", err)
			return
		}

		// Open data channels that where created before signaling
		pc.mu.Lock()
		// make a copy of dataChannels to avoid race condition accessing pc.dataChannels
		dataChannels := make(map[uint16]*DataChannel, len(pc.dataChannels))
		for k, v := range pc.dataChannels {
			dataChannels[k] = v
		}
		pc.mu.Unlock()

		var openedDCCount uint32
		for _, d := range dataChannels {
			err := d.open(pc.sctpTransport)
			if err != nil {
				pc.log.Warnf("failed to open data channel: %s", err)
				continue
			}
			openedDCCount++
		}

		pc.mu.Lock()
		pc.dataChannelsOpened += openedDCCount
		pc.mu.Unlock()
	}()

	if (desc.Type == SDPTypeAnswer || desc.Type == SDPTypePranswer) && pc.iceGatherer.agentIsTrickle {
		return pc.iceGatherer.Gather()
	}
	return nil
}

func (pc *PeerConnection) descriptionIsPlanB(desc *SessionDescription) bool {
	if desc == nil || desc.parsed == nil {
		return false
	}

	detectionRegex := regexp.MustCompile(`(?i)^(audio|video|data)$`)
	for _, media := range desc.parsed.MediaDescriptions {
		if len(detectionRegex.FindStringSubmatch(pc.getMidValue(media))) == 2 {
			return true
		}
	}
	return false
}

// openSRTP opens knows inbound SRTP streams from the RemoteDescription
func (pc *PeerConnection) openSRTP() {
	type incomingTrack struct {
		kind  RTPCodecType
		label string
		id    string
		ssrc  uint32
	}
	incomingTracks := map[uint32]incomingTrack{}

	remoteIsPlanB := false
	switch pc.configuration.SDPSemantics {
	case SDPSemanticsPlanB:
		remoteIsPlanB = true
	case SDPSemanticsUnifiedPlanWithFallback:
		remoteIsPlanB = pc.descriptionIsPlanB(pc.RemoteDescription())
	}

	for _, media := range pc.RemoteDescription().parsed.MediaDescriptions {
		for _, attr := range media.Attributes {

			codecType := NewRTPCodecType(media.MediaName.Media)
			if codecType == 0 {
				continue
			}

			if attr.Key == sdp.AttrKeySSRC {
				split := strings.Split(attr.Value, " ")
				ssrc, err := strconv.ParseUint(split[0], 10, 32)
				if err != nil {
					pc.log.Warnf("Failed to parse SSRC: %v", err)
					continue
				}

				trackID := ""
				trackLabel := ""
				if len(split) == 3 && strings.HasPrefix(split[1], "msid:") {
					trackLabel = split[1][len("msid:"):]
					trackID = split[2]
				}

				incomingTracks[uint32(ssrc)] = incomingTrack{codecType, trackLabel, trackID, uint32(ssrc)}
				if trackID != "" && trackLabel != "" {
					break // Remote provided Label+ID, we have all the information we need
				}
			}
		}
	}

	startReceiver := func(incoming incomingTrack, receiver *RTPReceiver) {
		err := receiver.Receive(RTPReceiveParameters{
			Encodings: RTPDecodingParameters{
				RTPCodingParameters{SSRC: incoming.ssrc},
			}})
		if err != nil {
			pc.log.Warnf("RTPReceiver Receive failed %s", err)
			return
		}

		if err = receiver.Track().determinePayloadType(); err != nil {
			pc.log.Warnf("Could not determine PayloadType for SSRC %d", receiver.Track().SSRC())
			return
		}

		pc.mu.RLock()
		defer pc.mu.RUnlock()

		if pc.currentLocalDescription == nil {
			pc.log.Warnf("SetLocalDescription not called, unable to handle incoming media streams")
			return
		}

		sdpCodec, err := pc.currentLocalDescription.parsed.GetCodecForPayloadType(receiver.Track().PayloadType())
		if err != nil {
			pc.log.Warnf("no codec could be found in RemoteDescription for payloadType %d", receiver.Track().PayloadType())
			return
		}

		codec, err := pc.api.mediaEngine.getCodecSDP(sdpCodec)
		if err != nil {
			pc.log.Warnf("codec %s in not registered", sdpCodec)
			return
		}

		receiver.Track().mu.Lock()
		receiver.Track().id = incoming.id
		receiver.Track().label = incoming.label
		receiver.Track().kind = codec.Type
		receiver.Track().codec = codec
		receiver.Track().mu.Unlock()

		if pc.onTrackHandler != nil {
			pc.onTrack(receiver.Track(), receiver)
		} else {
			pc.log.Warnf("OnTrack unset, unable to handle incoming media streams")
		}
	}

	localTransceivers := append([]*RTPTransceiver{}, pc.GetTransceivers()...)
	for ssrc, incoming := range incomingTracks {
		for i := range localTransceivers {
			t := localTransceivers[i]
			switch {
			case incomingTracks[ssrc].kind != t.kind:
				continue
			case t.Direction != RTPTransceiverDirectionRecvonly && t.Direction != RTPTransceiverDirectionSendrecv:
				continue
			case t.Receiver == nil:
				continue
			}

			delete(incomingTracks, ssrc)
			localTransceivers = append(localTransceivers[:i], localTransceivers[i+1:]...)
			go startReceiver(incoming, t.Receiver)
			break
		}
	}

	if remoteIsPlanB {
		for ssrc, incoming := range incomingTracks {
			t, err := pc.AddTransceiver(incoming.kind, RtpTransceiverInit{
				Direction: RTPTransceiverDirectionSendrecv,
			})
			if err != nil {
				pc.log.Warnf("Could not add transceiver for remote SSRC %d: %s", ssrc, err)
				continue
			}
			go startReceiver(incoming, t.Receiver)
		}
	}
}

// drainSRTP pulls and discards RTP/RTCP packets that don't match any SRTP
// These could be sent to the user, but right now we don't provide an API
// to distribute orphaned RTCP messages. This is needed to make sure we don't block
// and provides useful debugging messages
func (pc *PeerConnection) drainSRTP() {
	go func() {
		for {
			srtpSession, err := pc.dtlsTransport.getSRTPSession()
			if err != nil {
				pc.log.Warnf("drainSRTP failed to open SrtpSession: %v", err)
				return
			}

			_, ssrc, err := srtpSession.AcceptStream()
			if err != nil {
				pc.log.Warnf("Failed to accept RTP %v \n", err)
				return
			}

			pc.log.Debugf("Incoming unhandled RTP ssrc(%d)", ssrc)
		}
	}()

	for {
		srtcpSession, err := pc.dtlsTransport.getSRTCPSession()
		if err != nil {
			pc.log.Warnf("drainSRTP failed to open SrtcpSession: %v", err)
			return
		}

		_, ssrc, err := srtcpSession.AcceptStream()
		if err != nil {
			pc.log.Warnf("Failed to accept RTCP %v \n", err)
			return
		}
		pc.log.Debugf("Incoming unhandled RTCP ssrc(%d)", ssrc)
	}
}

// RemoteDescription returns pendingRemoteDescription if it is not null and
// otherwise it returns currentRemoteDescription. This property is used to
// determine if setRemoteDescription has already been called.
// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-remotedescription
func (pc *PeerConnection) RemoteDescription() *SessionDescription {
	if pc.pendingRemoteDescription != nil {
		return pc.pendingRemoteDescription
	}
	return pc.currentRemoteDescription
}

// AddICECandidate accepts an ICE candidate string and adds it
// to the existing set of candidates
func (pc *PeerConnection) AddICECandidate(candidate ICECandidateInit) error {
	if pc.RemoteDescription() == nil {
		return &rtcerr.InvalidStateError{Err: ErrNoRemoteDescription}
	}

	candidateValue := strings.TrimPrefix(candidate.Candidate, "candidate:")
	attribute := sdp.NewAttribute("candidate", candidateValue)
	sdpCandidate, err := attribute.ToICECandidate()
	if err != nil {
		return err
	}

	iceCandidate, err := newICECandidateFromSDP(sdpCandidate)
	if err != nil {
		return err
	}

	return pc.iceTransport.AddRemoteCandidate(iceCandidate)
}

// ICEConnectionState returns the ICE connection state of the
// PeerConnection instance.
func (pc *PeerConnection) ICEConnectionState() ICEConnectionState {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	return pc.iceConnectionState
}

// GetSenders returns the RTPSender that are currently attached to this PeerConnection
func (pc *PeerConnection) GetSenders() []*RTPSender {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	result := []*RTPSender{}
	for _, tranceiver := range pc.rtpTransceivers {
		if tranceiver.Sender != nil {
			result = append(result, tranceiver.Sender)
		}
	}
	return result
}

// GetReceivers returns the RTPReceivers that are currently attached to this RTCPeerConnection
func (pc *PeerConnection) GetReceivers() []*RTPReceiver {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	result := []*RTPReceiver{}
	for _, tranceiver := range pc.rtpTransceivers {
		if tranceiver.Receiver != nil {
			result = append(result, tranceiver.Receiver)
		}
	}
	return result
}

// GetTransceivers returns the RTCRtpTransceiver that are currently attached to this RTCPeerConnection
func (pc *PeerConnection) GetTransceivers() []*RTPTransceiver {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	return pc.rtpTransceivers
}

// AddTrack adds a Track to the PeerConnection
func (pc *PeerConnection) AddTrack(track *Track) (*RTPSender, error) {
	if pc.isClosed {
		return nil, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}
	var transceiver *RTPTransceiver
	for _, t := range pc.GetTransceivers() {
		if !t.stopped &&
			t.Sender != nil &&
			!t.Sender.hasSent() &&
			t.Receiver != nil &&
			t.Receiver.Track() != nil &&
			t.Receiver.Track().Kind() == track.Kind() {
			transceiver = t
			break
		}
	}
	if transceiver != nil {
		if err := transceiver.setSendingTrack(track); err != nil {
			return nil, err
		}
	} else {
		receiver, err := pc.api.NewRTPReceiver(track.Kind(), pc.dtlsTransport)
		if err != nil {
			return nil, err
		}

		sender, err := pc.api.NewRTPSender(track, pc.dtlsTransport)
		if err != nil {
			return nil, err
		}
		transceiver = pc.newRTPTransceiver(
			receiver,
			sender,
			RTPTransceiverDirectionSendrecv,
			track.Kind(),
		)
	}

	return transceiver.Sender, nil
}

// AddTransceiver Create a new RTCRtpTransceiver and add it to the set of transceivers.
// Deprecated: Use AddTrack, AddTransceiverFromKind or AddTransceiverFromTrack
func (pc *PeerConnection) AddTransceiver(trackOrKind RTPCodecType, init ...RtpTransceiverInit) (*RTPTransceiver, error) {
	return pc.AddTransceiverFromKind(trackOrKind, init...)
}

// AddTransceiverFromKind Create a new RTCRtpTransceiver(SendRecv or RecvOnly) and add it to the set of transceivers.
func (pc *PeerConnection) AddTransceiverFromKind(kind RTPCodecType, init ...RtpTransceiverInit) (*RTPTransceiver, error) {
	direction := RTPTransceiverDirectionSendrecv
	if len(init) > 1 {
		return nil, fmt.Errorf("AddTransceiverFromKind only accepts one RtpTransceiverInit")
	} else if len(init) == 1 {
		direction = init[0].Direction
	}

	switch direction {
	case RTPTransceiverDirectionSendrecv:
		receiver, err := pc.api.NewRTPReceiver(kind, pc.dtlsTransport)
		if err != nil {
			return nil, err
		}

		codecs := pc.api.mediaEngine.GetCodecsByKind(kind)
		if len(codecs) == 0 {
			return nil, fmt.Errorf("no %s codecs found", kind.String())
		}

		track, err := pc.NewTrack(codecs[0].PayloadType, mathRand.Uint32(), util.RandSeq(trackDefaultIDLength), util.RandSeq(trackDefaultLabelLength))
		if err != nil {
			return nil, err
		}

		sender, err := pc.api.NewRTPSender(track, pc.dtlsTransport)
		if err != nil {
			return nil, err
		}

		return pc.newRTPTransceiver(
			receiver,
			sender,
			RTPTransceiverDirectionSendrecv,
			kind,
		), nil

	case RTPTransceiverDirectionRecvonly:
		receiver, err := pc.api.NewRTPReceiver(kind, pc.dtlsTransport)
		if err != nil {
			return nil, err
		}

		return pc.newRTPTransceiver(
			receiver,
			nil,
			RTPTransceiverDirectionRecvonly,
			kind,
		), nil
	default:
		return nil, fmt.Errorf("AddTransceiverFromKind currently only supports recvonly and sendrecv")
	}
}

// AddTransceiverFromTrack Creates a new send only transceiver and add it to the set of
func (pc *PeerConnection) AddTransceiverFromTrack(track *Track, init ...RtpTransceiverInit) (*RTPTransceiver, error) {
	direction := RTPTransceiverDirectionSendrecv
	if len(init) > 1 {
		return nil, fmt.Errorf("AddTransceiverFromTrack only accepts one RtpTransceiverInit")
	} else if len(init) == 1 {
		direction = init[0].Direction
	}

	switch direction {
	case RTPTransceiverDirectionSendrecv:
		receiver, err := pc.api.NewRTPReceiver(track.Kind(), pc.dtlsTransport)
		if err != nil {
			return nil, err
		}

		sender, err := pc.api.NewRTPSender(track, pc.dtlsTransport)
		if err != nil {
			return nil, err
		}

		return pc.newRTPTransceiver(
			receiver,
			sender,
			RTPTransceiverDirectionSendrecv,
			track.Kind(),
		), nil

	case RTPTransceiverDirectionSendonly:
		sender, err := pc.api.NewRTPSender(track, pc.dtlsTransport)
		if err != nil {
			return nil, err
		}

		return pc.newRTPTransceiver(
			nil,
			sender,
			RTPTransceiverDirectionSendonly,
			track.Kind(),
		), nil
	default:
		return nil, fmt.Errorf("AddTransceiverFromTrack currently only supports sendonly and sendrecv")
	}
}

// CreateDataChannel creates a new DataChannel object with the given label
// and optional DataChannelInit used to configure properties of the
// underlying channel such as data reliability.
func (pc *PeerConnection) CreateDataChannel(label string, options *DataChannelInit) (*DataChannel, error) {
	pc.mu.Lock()

	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #2)
	if pc.isClosed {
		pc.mu.Unlock()
		return nil, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	// pion/webrtc#748
	params := &DataChannelParameters{
		Label:   label,
		Ordered: true,
	}

	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #19)
	if options == nil || options.ID == nil {
		var err error
		if params.ID, err = pc.generateDataChannelID(true); err != nil {
			pc.mu.Unlock()
			return nil, err
		}
	} else {
		params.ID = *options.ID
	}

	if options != nil {
		// Ordered indicates if data is allowed to be delivered out of order. The
		// default value of true, guarantees that data will be delivered in order.
		if options.Ordered != nil {
			params.Ordered = *options.Ordered
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #7)
		if options.MaxPacketLifeTime != nil {
			params.MaxPacketLifeTime = options.MaxPacketLifeTime
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #8)
		if options.MaxRetransmits != nil {
			params.MaxRetransmits = options.MaxRetransmits
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #9)
		if options.Ordered != nil {
			params.Ordered = *options.Ordered
		}
	}

	// pion/webrtc#748
	d, err := pc.api.newDataChannel(params, pc.log)
	if err != nil {
		pc.mu.Unlock()
		return nil, err
	}

	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #16)
	if d.maxPacketLifeTime != nil && d.maxRetransmits != nil {
		pc.mu.Unlock()
		return nil, &rtcerr.TypeError{Err: ErrRetransmitsOrPacketLifeTime}
	}

	// Remember datachannel
	pc.dataChannels[params.ID] = d

	sctpReady := pc.sctpTransport != nil && pc.sctpTransport.association != nil

	pc.dataChannelsRequested++
	pc.mu.Unlock()

	// Open if networking already started
	if sctpReady {
		err = d.open(pc.sctpTransport)
		if err != nil {
			return nil, err
		}
	}

	return d, nil
}

func (pc *PeerConnection) generateDataChannelID(client bool) (uint16, error) {
	var id uint16
	if !client {
		id++
	}

	max := sctpMaxChannels
	if pc.sctpTransport != nil {
		max = pc.sctpTransport.MaxChannels()
	}

	for ; id < max-1; id += 2 {
		_, ok := pc.dataChannels[id]
		if !ok {
			return id, nil
		}
	}
	return 0, &rtcerr.OperationError{Err: ErrMaxDataChannelID}
}

// SetIdentityProvider is used to configure an identity provider to generate identity assertions
func (pc *PeerConnection) SetIdentityProvider(provider string) error {
	return fmt.Errorf("TODO SetIdentityProvider")
}

// WriteRTCP sends a user provided RTCP packet to the connected peer
// If no peer is connected the packet is discarded
func (pc *PeerConnection) WriteRTCP(pkts []rtcp.Packet) error {
	raw, err := rtcp.Marshal(pkts)
	if err != nil {
		return err
	}

	srtcpSession, err := pc.dtlsTransport.getSRTCPSession()
	if err != nil {
		return nil
	}

	writeStream, err := srtcpSession.OpenWriteStream()
	if err != nil {
		return fmt.Errorf("WriteRTCP failed to open WriteStream: %v", err)
	}

	if _, err := writeStream.Write(raw); err != nil {
		return err
	}
	return nil
}

// Close ends the PeerConnection
func (pc *PeerConnection) Close() error {
	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #2)
	if pc.isClosed {
		return nil
	}
	// Try closing everything and collect the errors
	// Shutdown strategy:
	// 1. All Conn close by closing their underlying Conn.
	// 2. A Mux stops this chain. It won't close the underlying
	//    Conn if one of the endpoints is closed down. To
	//    continue the chain the Mux has to be closed.
	var closeErrs []error

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #3)
	pc.isClosed = true

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #4)
	pc.signalingState = SignalingStateClosed

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #11)
	if pc.iceTransport != nil {
		if err := pc.iceTransport.Stop(); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #12)
	pc.connectionState = PeerConnectionStateClosed

	if err := pc.dtlsTransport.Stop(); err != nil {
		closeErrs = append(closeErrs, err)
	}

	if pc.sctpTransport != nil {
		if err := pc.sctpTransport.Stop(); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}

	for _, t := range pc.rtpTransceivers {
		if err := t.Stop(); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}
	return util.FlattenErrs(closeErrs)
}

func (pc *PeerConnection) iceStateChange(newState ICEConnectionState) {
	pc.mu.Lock()
	pc.iceConnectionState = newState
	pc.mu.Unlock()

	pc.onICEConnectionStateChange(newState)
}

func (pc *PeerConnection) addFingerprint(d *sdp.SessionDescription) error {
	// pion/webrtc#753
	fingerprints, err := pc.configuration.Certificates[0].GetFingerprints()
	if err != nil {
		return err
	}
	for _, fingerprint := range fingerprints {
		d.WithFingerprint(fingerprint.Algorithm, strings.ToUpper(fingerprint.Value))
	}
	return nil
}

func (pc *PeerConnection) addTransceiverSDP(d *sdp.SessionDescription, midValue string, iceParams ICEParameters, candidates []ICECandidate, dtlsRole sdp.ConnectionRole, transceivers ...*RTPTransceiver) error {
	if len(transceivers) < 1 {
		return fmt.Errorf("addTransceiverSDP() called with 0 transceivers")
	}
	// Use the first transceiver to generate the section attributes
	t := transceivers[0]
	media := sdp.NewJSEPMediaDescription(t.kind.String(), []string{}).
		WithValueAttribute(sdp.AttrKeyConnectionSetup, dtlsRole.String()).
		WithValueAttribute(sdp.AttrKeyMID, midValue).
		WithICECredentials(iceParams.UsernameFragment, iceParams.Password).
		WithPropertyAttribute(sdp.AttrKeyRTCPMux).
		WithPropertyAttribute(sdp.AttrKeyRTCPRsize)

	codecs := pc.api.mediaEngine.GetCodecsByKind(t.kind)
	for _, codec := range codecs {
		media.WithCodec(codec.PayloadType, codec.Name, codec.ClockRate, codec.Channels, codec.SDPFmtpLine)

		for _, feedback := range codec.RTPCodecCapability.RTCPFeedback {
			media.WithValueAttribute("rtcp-fb", fmt.Sprintf("%d %s %s", codec.PayloadType, feedback.Type, feedback.Parameter))
		}
	}
	if len(codecs) == 0 {
		// Explicitly reject track if we don't have the codec
		d.WithMedia(&sdp.MediaDescription{
			MediaName: sdp.MediaName{
				Media:   t.kind.String(),
				Port:    sdp.RangedPort{Value: 0},
				Protos:  []string{"UDP", "TLS", "RTP", "SAVPF"},
				Formats: []string{"0"},
			},
		})
		return nil
	}

	for _, mt := range transceivers {
		if mt.Sender != nil && mt.Sender.track != nil {
			track := mt.Sender.track
			media = media.WithMediaSource(track.SSRC(), track.Label() /* cname */, track.Label() /* streamLabel */, track.ID())
			if pc.configuration.SDPSemantics == SDPSemanticsUnifiedPlan {
				media = media.WithPropertyAttribute("msid:" + track.Label() + " " + track.ID())
				break
			}
		}
	}

	media = media.WithPropertyAttribute(t.Direction.String())

	addCandidatesToMediaDescriptions(candidates, media)
	d.WithMedia(media)

	return nil
}

func (pc *PeerConnection) addDataMediaSection(d *sdp.SessionDescription, midValue string, iceParams ICEParameters, candidates []ICECandidate, dtlsRole sdp.ConnectionRole) {
	media := (&sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   "application",
			Port:    sdp.RangedPort{Value: 9},
			Protos:  []string{"DTLS", "SCTP"},
			Formats: []string{"5000"},
		},
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address: &sdp.Address{
				Address: "0.0.0.0",
			},
		},
	}).
		WithValueAttribute(sdp.AttrKeyConnectionSetup, dtlsRole.String()).
		WithValueAttribute(sdp.AttrKeyMID, midValue).
		WithPropertyAttribute(RTPTransceiverDirectionSendrecv.String()).
		WithPropertyAttribute("sctpmap:5000 webrtc-datachannel 1024").
		WithICECredentials(iceParams.UsernameFragment, iceParams.Password)

	addCandidatesToMediaDescriptions(candidates, media)
	d.WithMedia(media)
}

// NewTrack Creates a new Track
func (pc *PeerConnection) NewTrack(payloadType uint8, ssrc uint32, id, label string) (*Track, error) {
	codec, err := pc.api.mediaEngine.getCodec(payloadType)
	if err != nil {
		return nil, err
	} else if codec.Payloader == nil {
		return nil, fmt.Errorf("codec payloader not set")
	}

	return NewTrack(payloadType, ssrc, id, label, codec)
}

func (pc *PeerConnection) newRTPTransceiver(
	receiver *RTPReceiver,
	sender *RTPSender,
	direction RTPTransceiverDirection,
	kind RTPCodecType,
) *RTPTransceiver {

	t := &RTPTransceiver{
		Receiver:  receiver,
		Sender:    sender,
		Direction: direction,
		kind:      kind,
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.rtpTransceivers = append(pc.rtpTransceivers, t)
	return t
}

func (pc *PeerConnection) populateLocalCandidates(orig *SessionDescription) *SessionDescription {
	if orig == nil {
		return nil
	} else if pc.iceGatherer == nil {
		return orig
	}

	candidates, err := pc.iceGatherer.GetLocalCandidates()
	if err != nil {
		return orig
	}

	parsed := pc.pendingLocalDescription.parsed
	for _, m := range parsed.MediaDescriptions {
		addCandidatesToMediaDescriptions(candidates, m)
	}
	sdp, err := parsed.Marshal()
	if err != nil {
		return orig
	}

	return &SessionDescription{
		SDP:  string(sdp),
		Type: pc.pendingLocalDescription.Type,
	}
}

// CurrentLocalDescription represents the local description that was
// successfully negotiated the last time the PeerConnection transitioned
// into the stable state plus any local candidates that have been generated
// by the ICEAgent since the offer or answer was created.
func (pc *PeerConnection) CurrentLocalDescription() *SessionDescription {
	return pc.populateLocalCandidates(pc.currentLocalDescription)
}

// PendingLocalDescription represents a local description that is in the
// process of being negotiated plus any local candidates that have been
// generated by the ICEAgent since the offer or answer was created. If the
// PeerConnection is in the stable state, the value is null.
func (pc *PeerConnection) PendingLocalDescription() *SessionDescription {
	return pc.populateLocalCandidates(pc.pendingLocalDescription)
}

// CurrentRemoteDescription represents the last remote description that was
// successfully negotiated the last time the PeerConnection transitioned
// into the stable state plus any remote candidates that have been supplied
// via AddICECandidate() since the offer or answer was created.
func (pc *PeerConnection) CurrentRemoteDescription() *SessionDescription {
	return pc.currentRemoteDescription
}

// PendingRemoteDescription represents a remote description that is in the
// process of being negotiated, complete with any remote candidates that
// have been supplied via AddICECandidate() since the offer or answer was
// created. If the PeerConnection is in the stable state, the value is
// null.
func (pc *PeerConnection) PendingRemoteDescription() *SessionDescription {
	return pc.pendingRemoteDescription
}

// SignalingState attribute returns the signaling state of the
// PeerConnection instance.
func (pc *PeerConnection) SignalingState() SignalingState {
	return pc.signalingState
}

// ICEGatheringState attribute returns the ICE gathering state of the
// PeerConnection instance.
func (pc *PeerConnection) ICEGatheringState() ICEGatheringState {
	switch pc.iceGatherer.State() {
	case ICEGathererStateNew:
		return ICEGatheringStateNew
	case ICEGathererStateGathering:
		return ICEGatheringStateGathering
	default:
		return ICEGatheringStateComplete
	}
}

// ConnectionState attribute returns the connection state of the
// PeerConnection instance.
func (pc *PeerConnection) ConnectionState() PeerConnectionState {
	return pc.connectionState
}

// GetStats return data providing statistics about the overall connection
func (pc *PeerConnection) GetStats() StatsReport {
	statsCollector := newStatsReportCollector()
	statsCollector.Collecting()

	pc.mu.Lock()
	var dataChannelsClosed uint32
	for _, d := range pc.dataChannels {
		state := d.ReadyState()

		if state != DataChannelStateConnecting && state != DataChannelStateOpen {
			dataChannelsClosed++
		}

		d.collectStats(statsCollector)
	}

	pc.iceGatherer.collectStats(statsCollector)

	stats := PeerConnectionStats{
		Timestamp:             statsTimestampNow(),
		Type:                  StatsTypePeerConnection,
		ID:                    pc.statsID,
		DataChannelsOpened:    pc.dataChannelsOpened,
		DataChannelsClosed:    dataChannelsClosed,
		DataChannelsRequested: pc.dataChannelsRequested,
		DataChannelsAccepted:  pc.dataChannelsAccepted,
	}
	pc.mu.Unlock()

	statsCollector.Collect(stats.ID, stats)
	return statsCollector.Ready()
}

func addCandidatesToMediaDescriptions(candidates []ICECandidate, m *sdp.MediaDescription) {
	for _, c := range candidates {
		sdpCandidate := iceCandidateToSDP(c)
		sdpCandidate.ExtensionAttributes = append(sdpCandidate.ExtensionAttributes, sdp.ICECandidateAttribute{Key: "generation", Value: "0"})
		sdpCandidate.Component = 1
		m.WithICECandidate(sdpCandidate)
		sdpCandidate.Component = 2
		m.WithICECandidate(sdpCandidate)
	}
	if len(candidates) != 0 {
		m.WithPropertyAttribute("end-of-candidates")
	}
}
