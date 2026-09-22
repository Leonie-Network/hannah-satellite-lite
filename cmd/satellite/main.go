package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"hannah-satellite-lite/internal/config"
	"hannah-satellite-lite/internal/hannah"
)

const volumeStep = 5

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	audio, err := hannah.NewAudio(cfg.Satellite.SatelliteID)
	if err != nil {
		log.Fatalf("Failed to initialize audio devices: %v", err)
	}
	defer audio.Close()

	if err := audio.Start(ctx); err != nil {
		log.Fatalf("Failed to start audio pipeline: %v", err)
	}

	control := hannah.NewControl(cfg.Satellite.SatelliteID)
	control.OnListen = audio.StartListening
	control.OnPlayAsset = func(assetID string) {
		log.Printf("TODO: play asset: %s", assetID)
	}
	audio.Muted = control.Muted

	subs := append(control.Subscriptions(), audio.Subscription())

	mqttClient, err := hannah.ConnectMQTT(ctx, &cfg.MQTT, cfg.Satellite.SatelliteID, subs)
	if err != nil {
		log.Fatalf("Failed to connect to MQTT: %v", err)
	}
	defer mqttClient.Disconnect(250)

	control.PublishState(mqttClient)

	keys := &hannah.Keybindings{
		ToggleMute: func() { control.ToggleMute(mqttClient) },
		PTTDown:    audio.StartPTT,
		PTTUp:      audio.StopPTT,
		VolumeUp:   func() { control.AdjustVolume(mqttClient, volumeStep) },
		VolumeDown: func() { control.AdjustVolume(mqttClient, -volumeStep) },
	}
	keys.Register(cfg.Keybindings)
	defer keys.Close()

	<-ctx.Done()
	log.Println("Shutting down...")
}
