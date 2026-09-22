package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"hannah-satellite-go/internal/config"
	"hannah-satellite-go/internal/hannah"
)

const volumeStep = 5

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("Fehler beim Laden der Konfiguration: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	audio, err := hannah.NewAudio(cfg.Satellite.SatelliteID)
	if err != nil {
		log.Fatalf("Fehler beim Initialisieren der Audio-Geräte: %v", err)
	}
	defer audio.Close()

	if err := audio.Start(ctx); err != nil {
		log.Fatalf("Fehler beim Starten der Audio-Pipeline: %v", err)
	}

	control := hannah.NewControl(cfg.Satellite.SatelliteID)
	control.OnListen = audio.StartListening
	control.OnPlayAsset = func(assetID string) {
		log.Printf("TODO: Asset abspielen: %s", assetID)
	}
	audio.Muted = control.Muted

	subs := append(control.Subscriptions(), audio.Subscription())

	mqttClient, err := hannah.ConnectMQTT(ctx, &cfg.MQTT, cfg.Satellite.SatelliteID, subs)
	if err != nil {
		log.Fatalf("Fehler beim Verbinden mit MQTT: %v", err)
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
	log.Println("Beende...")
}
