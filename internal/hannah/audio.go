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

// UDP-Protokoll (1-Byte Type-Prefix), 1:1 aus satellite-esp/components/hannah_net/hannah_net.c:
//
//	0x01 + JSON = Control (beide Richtungen)
//	0x02 + PCM  = Audio   (Satellit → Proxy)
//	0x03 + PCM  = TTS     (Proxy → Satellit)
const (
	udpTypeControl byte = 0x01
	udpTypeAudio   byte = 0x02
	udpTypeTTS     byte = 0x03
)

const (
	// Muss mit Hannah Core übereinstimmen (config.yaml: audio.sample_rate) —
	// siehe hannah_audio Kconfig in satellite-esp, Default 16000.
	sampleRate   = 16000
	frameMS      = 30
	frameSamples = sampleRate * frameMS / 1000 // 480
	frameBytes   = frameSamples * 2            // 960 (16-bit mono)

	// Lokaler UDP-Port für TTS/Status vom Proxy — Default aus satellite-esp
	// (HANNAH_UDP_LISTEN_PORT), damit Core denselben Proxy-Registrierungsfluss
	// für Lite- und ESP-Satelliten nutzen kann.
	udpListenPort = 7776

	heartbeatInterval = 10 * time.Second

	// Ersatz für die WebRTC-VAD der ESP-Firmware: einfache RMS-Schwelle mit
	// Nachlaufzeit, um eine Sprechpause zu erkennen und audio_end zu senden.
	// Für die Lite-Version bewusst simpel gehalten (weniger Funktionen).
	silenceRMSThreshold = 400.0
	silenceHangover     = 800 * time.Millisecond
	maxListenDuration   = 15 * time.Second
)

// Audio kapselt Mikrofon-Aufnahme/Lautsprecher-Wiedergabe (malgo) und die
// UDP-Verbindung zum Hannah-Proxy (Adresse kommt per "hannah/server" MQTT-
// Broadcast, siehe Subscription).
type Audio struct {
	SatelliteID string

	// Muted wird vor jedem Senden abgefragt (z.B. control.Muted) — bei true
	// wird weiterhin aufgenommen (für die Stille-Erkennung), aber nichts an
	// den Proxy geschickt.
	Muted func() bool

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
	manual         bool // true während StartPTT()/StopPTT() — kein Silence-/Timeout-Ende
	lastVoiceAt    time.Time
	listenDeadline time.Time

	captureMu  sync.Mutex
	captureBuf []byte

	playMu  sync.Mutex
	playBuf []byte
}

// NewAudio initialisiert malgo-Context sowie Mikrofon- und Lautsprecher-Gerät
// (16 kHz mono S16). Die Geräte laufen ab Start() durchgehend — das Mikrofon
// wird nur bei aktivem StartListening()-Fenster tatsächlich an den Proxy
// gesendet, genau wie bei der ESP-Firmware ist das Mikrofon technisch immer an.
func NewAudio(satelliteID string) (*Audio, error) {
	a := &Audio{SatelliteID: satelliteID}

	malgoCtx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(message string) {
		log.Printf("[Audio/malgo] %s", strings.TrimSpace(message))
	})
	if err != nil {
		return nil, fmt.Errorf("malgo-Context: %w", err)
	}
	a.malgoCtx = malgoCtx

	capture, err := a.newCaptureDevice()
	if err != nil {
		_ = malgoCtx.Uninit()
		malgoCtx.Free()
		return nil, fmt.Errorf("Mikrofon-Gerät: %w", err)
	}
	a.capture = capture

	playback, err := a.newPlaybackDevice()
	if err != nil {
		capture.Uninit()
		_ = malgoCtx.Uninit()
		malgoCtx.Free()
		return nil, fmt.Errorf("Lautsprecher-Gerät: %w", err)
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

// onCaptureStopped/onPlaybackStopped reagieren auf ein vom Betriebssystem
// erzwungenes Geräte-Stopp (z.B. Windows wechselt das Standardgerät, USB-Gerät
// wird kurz getrennt — WASAPI liefert dann AUDCLNT_E_DEVICE_INVALIDATED).
// miniaudio stoppt das Gerät in diesem Fall sauber, initialisiert es aber
// nicht automatisch neu — ohne diese Callbacks bliebe die Aufnahme/Wiedergabe
// dauerhaft stumm, ohne dass der Prozess abstürzt oder sich meldet.
func (a *Audio) onCaptureStopped() {
	a.deviceMu.Lock()
	if a.closing || a.captureReiniting {
		a.deviceMu.Unlock()
		return
	}
	a.captureReiniting = true
	a.deviceMu.Unlock()

	log.Println("[Audio] Mikrofon gestoppt (Geräte-/Standardwechsel?) — initialisiere neu...")
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

	log.Println("[Audio] Lautsprecher gestoppt (Geräte-/Standardwechsel?) — initialisiere neu...")
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
			log.Printf("[Audio] Mikrofon-Neuinitialisierung fehlgeschlagen (Versuch %d/%d): %v", attempt, deviceReinitAttempts, err)
			continue
		}
		if err := dev.Start(); err != nil {
			dev.Uninit()
			a.deviceMu.Unlock()
			log.Printf("[Audio] Mikrofon-Start fehlgeschlagen (Versuch %d/%d): %v", attempt, deviceReinitAttempts, err)
			continue
		}
		a.capture = dev
		a.deviceMu.Unlock()

		if old != nil {
			old.Uninit()
		}
		log.Println("[Audio] Mikrofon erfolgreich neu initialisiert")
		return
	}

	log.Println("[Audio] Mikrofon-Neuinitialisierung endgültig fehlgeschlagen — Aufnahme bleibt stumm bis Neustart")
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
			log.Printf("[Audio] Lautsprecher-Neuinitialisierung fehlgeschlagen (Versuch %d/%d): %v", attempt, deviceReinitAttempts, err)
			continue
		}
		if err := dev.Start(); err != nil {
			dev.Uninit()
			a.deviceMu.Unlock()
			log.Printf("[Audio] Lautsprecher-Start fehlgeschlagen (Versuch %d/%d): %v", attempt, deviceReinitAttempts, err)
			continue
		}
		a.playback = dev
		a.deviceMu.Unlock()

		if old != nil {
			old.Uninit()
		}
		log.Println("[Audio] Lautsprecher erfolgreich neu initialisiert")
		return
	}

	log.Println("[Audio] Lautsprecher-Neuinitialisierung endgültig fehlgeschlagen — Wiedergabe bleibt stumm bis Neustart")
}

// Start bindet den UDP-Listen-Socket, startet Mikrofon/Lautsprecher und die
// Hintergrund-Loops (Empfang, Heartbeat). Läuft bis ctx abgebrochen wird.
func (a *Audio) Start(ctx context.Context) error {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: udpListenPort})
	if err != nil {
		return fmt.Errorf("UDP-Listen :%d: %w", udpListenPort, err)
	}
	a.conn = conn

	if err := a.capture.Start(); err != nil {
		return fmt.Errorf("Mikrofon starten: %w", err)
	}
	if err := a.playback.Start(); err != nil {
		return fmt.Errorf("Lautsprecher starten: %w", err)
	}

	go a.receiveLoop(ctx)
	go a.heartbeatLoop(ctx)

	log.Printf("[Audio] bereit, UDP-Listen :%d", udpListenPort)
	return nil
}

// Close gibt Audio-Geräte und UDP-Socket frei.
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

// Subscription liefert das MQTT-Topic, über das Core die Proxy-Adresse bekannt
// gibt (Broadcast, kein Satelliten-Präfix).
func (a *Audio) Subscription() TopicSubscription {
	return TopicSubscription{Topic: "hannah/server", QoS: 0, Handler: a.handleServerBroadcast}
}

// StartListening öffnet ein Aufnahme-Fenster (z.B. via Control.OnListen) —
// endet automatisch nach Sprechpause (silenceHangover) oder spätestens nach
// maxListenDuration, und sendet dann audio_end.
func (a *Audio) StartListening() {
	now := time.Now()
	a.mu.Lock()
	a.listening = true
	a.manual = false
	a.lastVoiceAt = now
	a.listenDeadline = now.Add(maxListenDuration)
	a.mu.Unlock()

	log.Println("[Audio] Aufnahme gestartet")
}

// StartPTT beginnt eine manuell gesteuerte Aufnahme (Push-to-Talk, z.B. per
// Tastenkombination als Ersatz für den physischen Knopf am ESP-Satelliten) —
// anders als StartListening endet sie nicht automatisch durch Stille oder
// Timeout, sondern erst durch StopPTT().
func (a *Audio) StartPTT() {
	a.mu.Lock()
	a.listening = true
	a.manual = true
	a.mu.Unlock()

	log.Println("[Audio] PTT: Aufnahme gestartet")
}

// StopPTT beendet eine per StartPTT gestartete Aufnahme sofort.
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

	log.Println("[Audio] Aufnahme beendet")
	a.sendControl(map[string]any{"type": "audio_end", "device": a.SatelliteID})
}

/* ── Mikrofon → Proxy ────────────────────────────────────────────────────── */

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

	if a.Muted == nil || !a.Muted() {
		a.sendAudio(frame)
	}

	if manual {
		return // Ende nur über StopPTT(), nicht durch Stille/Timeout
	}

	a.mu.Lock()
	silence := now.Sub(a.lastVoiceAt) > silenceHangover
	timeout := now.After(a.listenDeadline)
	a.mu.Unlock()

	if silence || timeout {
		a.stopListening()
	}
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

/* ── Proxy → Lautsprecher ────────────────────────────────────────────────── */

func (a *Audio) onPlayback(output []byte, _ []byte, _ uint32) {
	a.playMu.Lock()
	n := copy(output, a.playBuf)
	a.playBuf = a.playBuf[n:]
	a.playMu.Unlock()

	for i := n; i < len(output); i++ {
		output[i] = 0
	}
}

func (a *Audio) enqueuePlayback(pcm []byte) {
	a.playMu.Lock()
	a.playBuf = append(a.playBuf, pcm...)
	a.playMu.Unlock()
}

func (a *Audio) clearPlayback() {
	a.playMu.Lock()
	a.playBuf = nil
	a.playMu.Unlock()
}

/* ── UDP-Transport ───────────────────────────────────────────────────────── */

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
	case "stop":
		a.clearPlayback()
		log.Println("[Audio] TTS-Wiedergabe gestoppt (stop empfangen)")
	case "reregister":
		// Proxy kennt uns nicht (mehr) — z.B. nach Proxy-Neustart, in dessen
		// In-Memory-Satelliten-Map wir dann fehlen. Ohne erneutes register
		// bleiben wir dauerhaft unregistriert (Proxy beantwortet Heartbeats
		// dann nur noch mit reregister statt heartbeat_ack).
		log.Println("[Audio] Re-Registrierung angefordert")
		a.sendRegister()
	case "registered", "heartbeat_ack":
		// Erwartete Bestätigungen vom Proxy, keine Aktion nötig.
	case "pause", "resume":
		log.Printf("[Audio] Control (noch nicht umgesetzt): %s", msg.Type)
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

// handleServerBroadcast reagiert auf "hannah/server" ({"host","port"}, mit
// Fallback "host:port") und meldet den Satelliten beim Proxy an.
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
		log.Printf("[Audio] Ungültige hannah/server-Payload: %q", data)
		return
	}

	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", payload.Host, payload.Port))
	if err != nil {
		log.Printf("[Audio] Proxy-Adresse ungültig: %v", err)
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
		log.Printf("[Audio] UDP-Send fehlgeschlagen: %v", err)
	}
}
