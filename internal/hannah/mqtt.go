package hannah

import (
	"config"
	"context"
	"fmt"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

func ConnectMQTT(ctx context.Context, cfg *config.MQTTCfg) (mqtt.Client, error) {
	opts := mqtt.NewClientOptions().
		AddBroker(cfg.Address).
		SetClientID("hannah-satellite-" + cfg.SatelliteID).
		SetAutoReconnect(true)

	if cfg.username != "" {
		opts.SetUsername(cfg.username)
	}
	if cfg.password != "" {
		opts.SetPassword(cfg.password)
	}

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		// Logik für Re-Subscriptions hier platzieren (oder via Callback injizieren)
	})

	connectCh := make(chan error, 1)

	go func() {
		token := client.Connect()
		if token.Wait() && token.Error() != nil {
			connectCh <- token.Error()
		} else {
			connectCh <- nil
		}
	}()

	// Warten auf Context-Abbruch ODER Verbindungs-Ergebnis
	select {
	case <-ctx.Done():
		// Falls die Verbindung im Hintergrund doch noch klappt, direkt trennen
		go func() {
			<-connectCh
			if client.IsConnected() {
				client.Disconnect(250)
			}
		}()
		return nil, fmt.Errorf("MQTT-Verbindung abgebrochen: %w", ctx.Err())

	case err := <-connectCh:
		if err != nil {
			return nil, fmt.Errorf("MQTT-Verbindungsfehler zu %s: %w", cfg.Address, err)
		}
		return client, nil
	}
}