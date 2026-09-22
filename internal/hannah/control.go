package hannah

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Control bündelt den steuerbaren Zustand eines Satelliten (Mute/Volume) und die
// Reaktion auf die Core-Kommandos. Topic-Namen und Payload-Formate sind 1:1 aus
// satellite-esp/components/hannah_net/hannah_net.c übernommen, damit dieselbe
// Core-Instanz Lite- und ESP-Satelliten gleich ansprechen kann.
type Control struct {
	SatelliteID string

	// OnListen wird aufgerufen, wenn Core über .../listen eine Sprachaufnahme
	// anstoßen will (z.B. direkt nach einer TTS-Rückfrage).
	OnListen func()

	// OnPlayAsset wird mit der Asset-ID aufgerufen, wenn Core über .../play_asset
	// die Wiedergabe eines vordefinierten Sounds/TTS-Assets anstößt.
	OnPlayAsset func(assetID string)

	mu     sync.Mutex
	muted  bool
	volume int // 0-100
}

func NewControl(satelliteID string) *Control {
	return &Control{SatelliteID: satelliteID, volume: 100}
}

// Muted gibt den aktuellen Mute-Status zurück (z.B. für Audio.Muted).
func (c *Control) Muted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.muted
}

func (c *Control) topic(suffix string) string {
	return fmt.Sprintf("hannah/satellite/%s/%s", c.SatelliteID, suffix)
}

// Subscriptions liefert die TopicSubscriptions für ConnectMQTT.
func (c *Control) Subscriptions() []TopicSubscription {
	return []TopicSubscription{
		{Topic: c.topic("mute/set"), QoS: 0, Handler: c.handleMuteSet},
		{Topic: c.topic("volume/set"), QoS: 0, Handler: c.handleVolumeSet},
		{Topic: c.topic("listen"), QoS: 0, Handler: c.handleListen},
		{Topic: c.topic("play_asset"), QoS: 0, Handler: c.handlePlayAsset},
	}
}

// PublishState schickt Mute- und Volume-Status retained an Core. Da beide Topics
// retained sind, reicht ein einmaliger Aufruf nach dem ersten Connect — der Broker
// hält den letzten Stand auch über Satelliten-Reconnects hinweg vor.
func (c *Control) PublishState(client mqtt.Client) {
	c.mu.Lock()
	muted, volume := c.muted, c.volume
	c.mu.Unlock()

	c.publish(client, "mute/state", strconv.FormatBool(muted))
	c.publish(client, "volume/state", strconv.Itoa(volume))
}

func (c *Control) publish(client mqtt.Client, suffix, payload string) {
	topic := c.topic(suffix)
	token := client.Publish(topic, 1, true, payload)
	go func() {
		if token.Wait() && token.Error() != nil {
			log.Printf("[Control] Publish fehlgeschlagen (%s): %v", topic, token.Error())
		}
	}()
}

func (c *Control) handleMuteSet(client mqtt.Client, msg mqtt.Message) {
	payload := string(msg.Payload())
	muted := payload == "1" || strings.EqualFold(payload, "true")
	c.setMuted(client, muted)
}

func (c *Control) handleVolumeSet(client mqtt.Client, msg mqtt.Message) {
	vol, err := strconv.Atoi(string(msg.Payload()))
	if err != nil {
		log.Printf("[Control] Ungültiger volume/set-Payload: %q", msg.Payload())
		return
	}
	c.setVolume(client, vol)
}

// ToggleMute schaltet Mute lokal um — Gegenstück zu handleMuteSet, nur lokal
// ausgelöst statt von Core (z.B. über eine Tastenkombination).
func (c *Control) ToggleMute(client mqtt.Client) {
	c.mu.Lock()
	muted := !c.muted
	c.mu.Unlock()
	c.setMuted(client, muted)
}

// AdjustVolume ändert die Lautstärke lokal um delta (geclamped 0-100) —
// Gegenstück zu handleVolumeSet, nur lokal ausgelöst (z.B. Tastenkombination).
func (c *Control) AdjustVolume(client mqtt.Client, delta int) {
	c.mu.Lock()
	vol := c.volume + delta
	c.mu.Unlock()
	c.setVolume(client, vol)
}

func (c *Control) setMuted(client mqtt.Client, muted bool) {
	c.mu.Lock()
	c.muted = muted
	c.mu.Unlock()

	log.Printf("[Control] Mute: %v", muted)
	c.publish(client, "mute/state", strconv.FormatBool(muted))
}

func (c *Control) setVolume(client mqtt.Client, vol int) {
	if vol < 0 {
		vol = 0
	}
	if vol > 100 {
		vol = 100
	}

	c.mu.Lock()
	c.volume = vol
	c.mu.Unlock()

	log.Printf("[Control] Volume: %d", vol)
	c.publish(client, "volume/state", strconv.Itoa(vol))
}

func (c *Control) handleListen(client mqtt.Client, msg mqtt.Message) {
	log.Println("[Control] listen empfangen")
	if c.OnListen != nil {
		c.OnListen()
	}
}

func (c *Control) handlePlayAsset(client mqtt.Client, msg mqtt.Message) {
	var payload struct {
		AssetID string `json:"asset_id"`
	}
	if err := json.Unmarshal(msg.Payload(), &payload); err != nil || payload.AssetID == "" {
		log.Printf("[Control] Ungültiger play_asset-Payload: %q", msg.Payload())
		return
	}

	log.Printf("[Control] play_asset: %s", payload.AssetID)
	if c.OnPlayAsset != nil {
		c.OnPlayAsset(payload.AssetID)
	}
}
