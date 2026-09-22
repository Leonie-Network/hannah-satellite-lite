package hannah

import (
	"context"
	"fmt"
	"log"

	"hannah-satellite-go/internal/config"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type TopicSubscription struct {
	Topic   string
	QoS     byte
	Handler mqtt.MessageHandler
}

func ConnectMQTT(ctx context.Context, cfg *config.MQTTCfg, satelliteID string, subs []TopicSubscription) (mqtt.Client, error) {
	broker := fmt.Sprintf("tcp://%s:%d", cfg.Address, cfg.Port)
	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID("hannah-satellite-" + satelliteID).
		SetAutoReconnect(true)

	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
	}
	if cfg.Password != "" {
		opts.SetPassword(cfg.Password)
	}

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		log.Println("[MQTT] Verbunden. Richte Subscriptions ein...")

		for _, sub := range subs {
			token := c.Subscribe(sub.Topic, sub.QoS, sub.Handler)

			go func(t string) {
				if token.Wait() && token.Error() != nil {
					log.Printf("[MQTT] Fehler beim Abonnieren von %s: %v", t, token.Error())
				} else {
					log.Printf("[MQTT] Topic erfolgreich abonniert: %s", t)
				}
			}(sub.Topic)
		}
	})

	client := mqtt.NewClient(opts)
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
			return nil, fmt.Errorf("MQTT-Verbindungsfehler zu %s: %w", broker, err)
		}
		return client, nil
	}
}
