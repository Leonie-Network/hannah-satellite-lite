package hannah

import (
	"context"
	"fmt"
	"log"

	"hannah-satellite-lite/internal/config"

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
		log.Println("[MQTT] Connected. Setting up subscriptions...")

		for _, sub := range subs {
			token := c.Subscribe(sub.Topic, sub.QoS, sub.Handler)

			go func(t string) {
				if token.Wait() && token.Error() != nil {
					log.Printf("[MQTT] Failed to subscribe to %s: %v", t, token.Error())
				} else {
					log.Printf("[MQTT] Subscribed to topic: %s", t)
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

	// Wait for context cancellation OR connection result
	select {
	case <-ctx.Done():
		// If the connection succeeds in the background anyway, disconnect right away
		go func() {
			<-connectCh
			if client.IsConnected() {
				client.Disconnect(250)
			}
		}()
		return nil, fmt.Errorf("MQTT connection aborted: %w", ctx.Err())

	case err := <-connectCh:
		if err != nil {
			return nil, fmt.Errorf("MQTT connection error to %s: %w", broker, err)
		}
		return client, nil
	}
}
