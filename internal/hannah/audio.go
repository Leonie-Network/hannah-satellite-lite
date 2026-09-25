package hannah

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gen2brain/malgo"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// UDP protocol (1-byte type prefix), taken 1:1 from
// satellite-esp/components/hannah_net/hannah_net.c:
//
//	0x01 + JSON = Control (both directions)
//	0x02 + PCM  = Audio   (satellite → proxy)
//	0x03 + PCM  = TTS     (proxy → satellite)
const (
	udpTypeControl byte = 0x01
	udpTypeAudio   byte = 0x02
	udpTypeTTS     byte = 0x03
)

const (
	// Must match Hannah Core (config.yaml: audio.sample_rate) — see the
	// hannah_audio Kconfig in satellite-esp, default 16000.
	sampleRate   = 16000
	frameMS      = 30
	frameSamples = sampleRate * frameMS / 1000 // 480
	frameBytes   = frameSamples * 2            // 960 (16-bit mono)

	// Local UDP port for TTS/status from the proxy — default from satellite-esp
	// (HANNAH_UDP_LISTEN_PORT), so Core can use the same proxy registration flow
	// for Lite and ESP satellites.
	udpListenPort = 7776

	heartbeatInterval = 10 * time.Second

	// Stand-in for the ESP firmware's WebRTC VAD: a simple RMS threshold with
	// hangover time to detect a pause in speech and send audio_end. Kept
	// deliberately simple for the Lite version (fewer features).
	silenceRMSThreshold = 400.0
	silenceHangover     = 800 * time.Millisecond
	maxListenDuration   = 15 * time.Second
)

// Audio wraps microphone capture/speaker playback (malgo) and the UDP
// connection to the Hannah proxy (address arrives via the "hannah/server" MQTT
// broadcast, see Subscription).
type Audio struct {
	SatelliteID string

	// Muted is checked before every send (e.g. control.Muted) — when true,
	// capture keeps running (for silence detection), but nothing is sent to
	// the proxy.
	Muted func() bool

	// Volume returns the current playback volume 0-100 (e.g. control.Volume)
	// — TTS samples are scaled linearly by it, like on the ESP firmware.
	Volume func() int

	// OnPlaybackDone is called once the playback buffer has run empty after a
	// "tts_end" from the proxy (e.g. to publish .../playback_done).
	OnPlaybackDone func()

	malgoCtx *malgo.AllocatedContext

	deviceMu          sync.Mutex
	capture           *malgo.Device
	playback          *malgo.Device
	closing           bool
	captureReiniting  bool
	playbackReiniting bool

	conn *net.UDPConn

	mu             sync.Mutex
	proxyAddr      *net.UDPAddr
	ready          bool
	listening      bool
	manual         bool // true during StartPTT()/StopPTT() — no silence/timeout end
	lastVoiceAt    time.Time
	listenDeadline time.Time

	captureMu  sync.Mutex
	captureBuf []byte

	playMu      sync.Mutex
	playBuf     []byte
	playEndSeen bool // "tts_end" received, playback_done pending until playBuf drains
	playing     bool // TTS response in progress (only used for start/done logging)
}

// NewAudio initializes the malgo context and the microphone/speaker devices
// (16 kHz mono S16). The devices keep running once Start() is called — the
// microphone is only actually sent to the proxy while a StartListening()
// window is active, just like on the ESP firmware the mic is technically
// always on.
func NewAudio(satelliteID string) (*Audio, error) {
	a := &Audio{SatelliteID: satelliteID}

	malgoCtx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(message string) {
		log.Printf("[Audio/malgo] %s", strings.TrimSpace(message))
	})
	if err != nil {
		return nil, fmt.Errorf("malgo context: %w", err)
	}
	a.malgoCtx = malgoCtx

	capture, err := a.newCaptureDevice()
	if err != nil {
		_ = malgoCtx.Uninit()
		malgoCtx.Free()
		return nil, fmt.Errorf("microphone device: %w", err)
	}
	a.capture = capture

	playback, err := a.newPlaybackDevice()
	if err != nil {
		capture.Uninit()
		_ = malgoCtx.Uninit()
		malgoCtx.Free()
		return nil, fmt.Errorf("speaker device: %w", err)
	}
	a.playback = playback

	return a, nil
}

func (a *Audio) newCaptureDevice() (*malgo.Device, error) {
	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = 1
	cfg.SampleRate = sampleRate

	return malgo.InitDevice(a.malgoCtx.Context, cfg, malgo.DeviceCallbacks{
		Data: a.onCapture,
		Stop: a.onCaptureStopped,
	})
}

func (a *Audio) newPlaybackDevice() (*malgo.Device, error) {
	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = 1
	cfg.SampleRate = sampleRate

	return malgo.InitDevice(a.malgoCtx.Context, cfg, malgo.DeviceCallbacks{
		Data: a.onPlayback,
		Stop: a.onPlaybackStopped,
	})
}

// onCaptureStopped/onPlaybackStopped react to a device stop forced by the OS
// (e.g. Windows switches the default device, a USB device is briefly
// unplugged — WASAPI then returns AUDCLNT_E_DEVICE_INVALIDATED). miniaudio
// stops the device cleanly in that case but doesn't reinitialize it
// automatically — without these callbacks, capture/playback would stay
// silent forever without the process crashing or saying anything.
func (a *Audio) onCaptureStopped() {
	a.deviceMu.Lock()
	if a.closing || a.captureReiniting {
		a.deviceMu.Unlock()
		return
	}
	a.captureReiniting = true
	a.deviceMu.Unlock()

	log.Println("[Audio] Microphone stopped (device/default change?) — reinitializing...")
	go a.reinitCapture()
}

func (a *Audio) onPlaybackStopped() {
	a.deviceMu.Lock()
	if a.closing || a.playbackReiniting {
		a.deviceMu.Unlock()
		return
	}
	a.playbackReiniting = true
	a.deviceMu.Unlock()

	log.Println("[Audio] Speaker stopped (device/default change?) — reinitializing...")
	go a.reinitPlayback()
}

const (
	deviceReinitAttempts = 5
	deviceReinitBaseWait = 1 * time.Second
)

func (a *Audio) reinitCapture() {
	defer func() {
		a.deviceMu.Lock()
		a.captureReiniting = false
		a.deviceMu.Unlock()
	}()

	for attempt := 1; attempt <= deviceReinitAttempts; attempt++ {
		time.Sleep(time.Duration(attempt) * deviceReinitBaseWait)

		a.deviceMu.Lock()
		if a.closing {
			a.deviceMu.Unlock()
			return
		}
		old := a.capture
		dev, err := a.newCaptureDevice()
		if err != nil {
			a.deviceMu.Unlock()
			log.Printf("[Audio] Microphone reinitialization failed (attempt %d/%d): %v", attempt, deviceReinitAttempts, err)
			continue
		}
		if err := dev.Start(); err != nil {
			dev.Uninit()
			a.deviceMu.Unlock()
			log.Printf("[Audio] Microphone start failed (attempt %d/%d): %v", attempt, deviceReinitAttempts, err)
			continue
		}
		a.capture = dev
		a.deviceMu.Unlock()

		if old != nil {
			old.Uninit()
		}
		log.Println("[Audio] Microphone reinitialized successfully")
		return
	}

	log.Println("[Audio] Microphone reinitialization ultimately failed — capture stays silent until restart")
}

func (a *Audio) reinitPlayback() {
	defer func() {
		a.deviceMu.Lock()
		a.playbackReiniting = false
		a.deviceMu.Unlock()
	}()

	for attempt := 1; attempt <= deviceReinitAttempts; attempt++ {
		time.Sleep(time.Duration(attempt) * deviceReinitBaseWait)

		a.deviceMu.Lock()
		if a.closing {
			a.deviceMu.Unlock()
			return
		}
		old := a.playback
		dev, err := a.newPlaybackDevice()
		if err != nil {
			a.deviceMu.Unlock()
			log.Printf("[Audio] Speaker reinitialization failed (attempt %d/%d): %v", attempt, deviceReinitAttempts, err)
			continue
		}
		if err := dev.Start(); err != nil {
			dev.Uninit()
			a.deviceMu.Unlock()
			log.Printf("[Audio] Speaker start failed (attempt %d/%d): %v", attempt, deviceReinitAttempts, err)
			continue
		}
		a.playback = dev
		a.deviceMu.Unlock()

		if old != nil {
			old.Uninit()
		}
		log.Println("[Audio] Speaker reinitialized successfully")
		return
	}

	log.Println("[Audio] Speaker reinitialization ultimately failed — playback stays silent until restart")
}

// Start binds the UDP listen socket, starts the microphone/speaker, and the
// background loops (receive, heartbeat). Runs until ctx is canceled.
func (a *Audio) Start(ctx context.Context) error {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: udpListenPort})
	if err != nil {
		return fmt.Errorf("UDP listen :%d: %w", udpListenPort, err)
	}
	a.conn = conn

	if err := a.capture.Start(); err != nil {
		return fmt.Errorf("start microphone: %w", err)
	}
	if err := a.playback.Start(); err != nil {
		return fmt.Errorf("start speaker: %w", err)
	}

	go a.receiveLoop(ctx)
	go a.heartbeatLoop(ctx)

	log.Printf("[Audio] ready, UDP listen :%d", udpListenPort)
	return nil
}

// Close releases the audio devices and the UDP socket.
func (a *Audio) Close() {
	a.deviceMu.Lock()
	a.closing = true
	capture, playback := a.capture, a.playback
	a.deviceMu.Unlock()

	if a.conn != nil {
		a.conn.Close()
	}
	if capture != nil {
		capture.Uninit()
	}
	if playback != nil {
		playback.Uninit()
	}
	if a.malgoCtx != nil {
		_ = a.malgoCtx.Uninit()
		a.malgoCtx.Free()
	}
}

// Subscription returns the MQTT topic through which Core announces the proxy
// address (broadcast, no satellite prefix).
func (a *Audio) Subscription() TopicSubscription {
	return TopicSubscription{Topic: "hannah/server", QoS: 0, Handler: a.handleServerBroadcast}
}

// StartListening opens a recording window (e.g. via Control.OnListen) — ends
// automatically after a pause in speech (silenceHangover) or at the latest
// after maxListenDuration, and sends audio_end at that point.
func (a *Audio) StartListening() {
	if a.isMuted() {
		log.Println("[Audio] Muted — recording not started")
		return
	}

	now := time.Now()
	a.mu.Lock()
	a.listening = true
	a.manual = false
	a.lastVoiceAt = now
	a.listenDeadline = now.Add(maxListenDuration)
	a.mu.Unlock()

	log.Println("[Audio] Recording started")
}

// StartPTT begins a manually controlled recording (push-to-talk, e.g. via a
// keybinding as a stand-in for the physical button on the ESP satellite) —
// unlike StartListening it doesn't end automatically on silence or timeout,
// only via StopPTT().
func (a *Audio) StartPTT() {
	if a.isMuted() {
		log.Println("[Audio] Muted — PTT recording not started")
		return
	}

	a.mu.Lock()
	a.listening = true
	a.manual = true
	a.mu.Unlock()

	log.Println("[Audio] PTT: recording started")
}

// StopPTT ends a recording started via StartPTT immediately.
func (a *Audio) StopPTT() {
	a.mu.Lock()
	a.manual = false
	a.mu.Unlock()

	a.stopListening()
}

func (a *Audio) stopListening() {
	a.mu.Lock()
	if !a.listening {
		a.mu.Unlock()
		return
	}
	a.listening = false
	a.mu.Unlock()

	log.Println("[Audio] Recording ended")
	a.sendControl(map[string]any{"type": "audio_end", "device": a.SatelliteID})
}

/* ── Microphone → proxy ──────────────────────────────────────────────────── */

func (a *Audio) onCapture(_ []byte, input []byte, _ uint32) {
	a.captureMu.Lock()
	defer a.captureMu.Unlock()

	a.captureBuf = append(a.captureBuf, input...)
	for len(a.captureBuf) >= frameBytes {
		frame := a.captureBuf[:frameBytes]
		a.processFrame(frame)
		a.captureBuf = append([]byte(nil), a.captureBuf[frameBytes:]...)
	}
}

func (a *Audio) processFrame(frame []byte) {
	a.mu.Lock()
	listening := a.listening
	manual := a.manual
	a.mu.Unlock()
	if !listening {
		return
	}

	now := time.Now()
	if rmsInt16(frame) > silenceRMSThreshold {
		a.mu.Lock()
		a.lastVoiceAt = now
		a.mu.Unlock()
	}

	if !a.isMuted() {
		a.sendAudio(frame)
	}

	if manual {
		return // ends only via StopPTT(), not by silence/timeout
	}

	a.mu.Lock()
	silence := now.Sub(a.lastVoiceAt) > silenceHangover
	timeout := now.After(a.listenDeadline)
	a.mu.Unlock()

	if silence || timeout {
		a.stopListening()
	}
}

func (a *Audio) isMuted() bool {
	return a.Muted != nil && a.Muted()
}

func rmsInt16(frame []byte) float64 {
	n := len(frame) / 2
	if n == 0 {
		return 0
	}
	var sumSq float64
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(frame[i*2:])))
		sumSq += s * s
	}
	return math.Sqrt(sumSq / float64(n))
}

/* ── Proxy → speaker ─────────────────────────────────────────────────────── */

func (a *Audio) onPlayback(output []byte, _ []byte, _ uint32) {
	a.playMu.Lock()
	n := copy(output, a.playBuf)
	a.playBuf = a.playBuf[n:]
	done := a.playEndSeen && len(a.playBuf) == 0
	if done {
		a.playEndSeen = false
		a.playing = false
	}
	a.playMu.Unlock()

	applyVolume(output[:n], a.volume())
	for i := n; i < len(output); i++ {
		output[i] = 0
	}

	// Not called inline — the malgo data callback must not block (MQTT publish).
	if done {
		go func() {
			log.Println("[Audio] TTS playback done")
			if a.OnPlaybackDone != nil {
				a.OnPlaybackDone()
			}
		}()
	}
}

func (a *Audio) volume() int {
	if a.Volume == nil {
		return 100
	}
	return a.Volume()
}

// applyVolume scales 16-bit LE samples in place by vol (0-100).
func applyVolume(pcm []byte, vol int) {
	if vol >= 100 {
		return
	}
	if vol < 0 {
		vol = 0
	}
	for i := 0; i+1 < len(pcm); i += 2 {
		s := int32(int16(binary.LittleEndian.Uint16(pcm[i:])))
		binary.LittleEndian.PutUint16(pcm[i:], uint16(int16(s*int32(vol)/100)))
	}
}

func (a *Audio) enqueuePlayback(pcm []byte) {
	a.playMu.Lock()
	a.playBuf = append(a.playBuf, pcm...)
	started := !a.playing
	a.playing = true
	a.playMu.Unlock()

	if started {
		log.Println("[Audio] TTS playback started")
	}
}

// markPlaybackEnd arms playback_done — fired by onPlayback as soon as the
// buffer has drained (immediately on the next callback if it already is).
func (a *Audio) markPlaybackEnd() {
	a.playMu.Lock()
	a.playEndSeen = true
	a.playMu.Unlock()
}

func (a *Audio) clearPlayback() {
	a.playMu.Lock()
	a.playBuf = nil
	a.playing = false
	a.playMu.Unlock()
}

/* ── UDP transport ───────────────────────────────────────────────────────── */

func (a *Audio) receiveLoop(ctx context.Context) {
	buf := make([]byte, 65536)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_ = a.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _, err := a.conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		if n < 2 {
			continue
		}

		switch buf[0] {
		case udpTypeTTS:
			payload := make([]byte, n-1)
			copy(payload, buf[1:n])
			a.enqueuePlayback(payload)
		case udpTypeControl:
			a.handleControl(buf[1:n])
		}
	}
}

func (a *Audio) handleControl(payload []byte) {
	var msg struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return
	}

	switch msg.Type {
	case "tts_end":
		a.markPlaybackEnd()
	case "stop":
		a.clearPlayback()
		log.Println("[Audio] TTS playback stopped (stop received)")
	case "reregister":
		// The proxy doesn't know us (anymore) — e.g. after a proxy restart, whose
		// in-memory satellite map we're then missing from. Without a fresh
		// register we'd stay unregistered forever (the proxy then only answers
		// heartbeats with reregister instead of heartbeat_ack).
		log.Println("[Audio] Reregistration requested")
		a.sendRegister()
	case "registered", "heartbeat_ack":
		// Expected acks from the proxy, no action needed.
	case "pause", "resume":
		log.Printf("[Audio] Control (not yet implemented): %s", msg.Type)
	default:
		log.Printf("[Audio] Control: %s", string(payload))
	}
}

func (a *Audio) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.sendControl(map[string]any{"type": "heartbeat", "device": a.SatelliteID})
		}
	}
}

// handleServerBroadcast reacts to "hannah/server" ({"host","port"}, with a
// "host:port" fallback) and registers the satellite with the proxy.
func (a *Audio) handleServerBroadcast(_ mqtt.Client, msg mqtt.Message) {
	var payload struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	data := msg.Payload()
	if err := json.Unmarshal(data, &payload); err != nil || payload.Host == "" || payload.Port == 0 {
		if host, portStr, ok := strings.Cut(string(data), ":"); ok {
			if port, err := strconv.Atoi(portStr); err == nil {
				payload.Host, payload.Port = host, port
			}
		}
	}
	if payload.Host == "" || payload.Port == 0 {
		log.Printf("[Audio] Invalid hannah/server payload: %q", data)
		return
	}

	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", payload.Host, payload.Port))
	if err != nil {
		log.Printf("[Audio] Invalid proxy address: %v", err)
		return
	}

	a.mu.Lock()
	a.proxyAddr = addr
	a.ready = true
	a.mu.Unlock()

	log.Printf("[Audio] Proxy: %s:%d", payload.Host, payload.Port)
	a.sendRegister()
}

func (a *Audio) sendRegister() {
	a.sendControl(map[string]any{
		"type":        "register",
		"device":      a.SatelliteID,
		"listen_port": udpListenPort,
	})
}

func (a *Audio) sendControl(payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	a.send(udpTypeControl, data)
}

func (a *Audio) sendAudio(pcm []byte) {
	a.send(udpTypeAudio, pcm)
}

func (a *Audio) send(packetType byte, payload []byte) {
	a.mu.Lock()
	addr, ready := a.proxyAddr, a.ready
	a.mu.Unlock()
	if !ready {
		return
	}

	pkt := make([]byte, 1+len(payload))
	pkt[0] = packetType
	copy(pkt[1:], payload)
	if _, err := a.conn.WriteToUDP(pkt, addr); err != nil {
		log.Printf("[Audio] UDP send failed: %v", err)
	}
}
