package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExampleConfig(t *testing.T) {
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Address != "192.168.1.1" {
		t.Errorf("mqtt.address mismatch: %q", cfg.MQTT.Address)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Setenv("HANNAH_SATELLITE_MQTT__ADDRESS", "192.168.1.5:50051")
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Address != "192.168.1.5:50051" {
		t.Errorf("mqtt.address mismatch: %q", cfg.MQTT.Address)
	}
}

func TestLoadMissingFileWithoutAddressStillErrors(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error when hannah.address is missing from both file and env")
	}
}

func TestEnvOverridesExistingValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "mqtt:\n  address: 192.168.1.15:50051\n")
	t.Setenv("HANNAH_SATELLITE_MQTT__ADDRESS", "192.168.1.5:50051")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Address != "192.168.1.5:50051" {
		t.Errorf("mqtt.address mismatch: %q", cfg.MQTT.Address)
	}
}

func TestOtherHannahPrefixedVarsAreNotAbsorbed(t *testing.T) {
	// Same collision Core hit in CI (#327): other HANNAH_* variables exist in the
	// project for unrelated purposes and must not affect this component's config.
	t.Setenv("HANNAH_ASSET_SERVER_BASE_URL", "https://hannah-asset.example.com")
	t.Setenv("HANNAH_CORE_MQTT__HOST", "192.168.1.5")
	t.Setenv("HANNAH_PROXY_HANNAH__ADDRESS", "192.168.1.5:50051")
	t.Setenv("HANNAH_SATELLITE_MQTT__ADDRESS", "192.168.1.6:50051")

	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Address != "192.168.1.6:50051" {
		t.Errorf("mqtt.address mismatch: %q", cfg.MQTT.Address)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
