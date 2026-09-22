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

// Control bundles a satellite's controllable state (mute/volume) and the
// reaction to Core's commands. Topic names and payload formats are taken 1:1
// from satellite-esp/components/hannah_net/hannah_net.c, so the same Core
// instance can address Lite and ESP satellites the same way.
type Control struct {
	SatelliteID string

	// OnListen is called when Core wants to trigger a recording via .../listen
	// (e.g. right after a TTS follow-up question).
	OnListen func()

	// OnPlayAsset is called with the asset ID when Core triggers playback of a
	// predefined sound/TTS asset via .../play_asset.
	OnPlayAsset func(assetID string)

	mu     sync.Mutex
	muted  bool
	volume int // 0-100
}

func NewControl(satelliteID string) *Control {
	return &Control{SatelliteID: satelliteID, volume: 100}
}

// Muted returns the current mute state (e.g. for Audio.Muted).
func (c *Control) Muted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.muted
}

func (c *Control) topic(suffix string) string {
	return fmt.Sprintf("hannah/satellite/%s/%s", c.SatelliteID, suffix)
}

// Subscriptions returns the TopicSubscriptions for ConnectMQTT.
func (c *Control) Subscriptions() []TopicSubscription {
	return []TopicSubscription{
		{Topic: c.topic("mute/set"), QoS: 0, Handler: c.handleMuteSet},
		{Topic: c.topic("volume/set"), QoS: 0, Handler: c.handleVolumeSet},
		{Topic: c.topic("listen"), QoS: 0, Handler: c.handleListen},
		{Topic: c.topic("play_asset"), QoS: 0, Handler: c.handlePlayAsset},
	}
}

// PublishState sends the mute and volume state to Core, retained. Since both
// topics are retained, a single call after the initial connect is enough —
// the broker keeps the last value around across satellite reconnects too.
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
			log.Printf("[Control] Publish failed (%s): %v", topic, token.Error())
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
		log.Printf("[Control] Invalid volume/set payload: %q", msg.Payload())
		return
	}
	c.setVolume(client, vol)
}

// ToggleMute flips mute locally — the counterpart to handleMuteSet, just
// triggered locally instead of by Core (e.g. via a keybinding).
func (c *Control) ToggleMute(client mqtt.Client) {
	c.mu.Lock()
	muted := !c.muted
	c.mu.Unlock()
	c.setMuted(client, muted)
}

// AdjustVolume changes the volume locally by delta (clamped 0-100) — the
// counterpart to handleVolumeSet, just triggered locally (e.g. keybinding).
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
	log.Println("[Control] listen received")
	if c.OnListen != nil {
		c.OnListen()
	}
}

func (c *Control) handlePlayAsset(client mqtt.Client, msg mqtt.Message) {
	var payload struct {
		AssetID string `json:"asset_id"`
	}
	if err := json.Unmarshal(msg.Payload(), &payload); err != nil || payload.AssetID == "" {
		log.Printf("[Control] Invalid play_asset payload: %q", msg.Payload())
		return
	}

	log.Printf("[Control] play_asset: %s", payload.AssetID)
	if c.OnPlayAsset != nil {
		c.OnPlayAsset(payload.AssetID)
	}
}
