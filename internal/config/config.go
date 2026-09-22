package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

type Config struct {
	MQTT        MQTTCfg        `json:"mqtt"`
	Satellite   SatelliteCfg   `json:"satellite"`
	Keybindings KeybindingsCfg `json:"keybindings"`
}

type MQTTCfg struct {
	// Hostname/IP of the MQTT broker, without port — e.g. "192.0.2.1"
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type SatelliteCfg struct {
	SatelliteID string `json:"satellite_id"`
}

// KeybindingsCfg defines optional global hotkeys as a local stand-in for the
// physical buttons on the ESP satellites (mute, PTT, volume). Format: "+"-separated,
// e.g. "ctrl+alt+m". Empty value = binding disabled.
type KeybindingsCfg struct {
	Mute    string `json:"mute"`
	PTT     string `json:"ptt"`
	VolUp   string `json:"vol_up"`
	VolDown string `json:"vol_down"`
}

const envPrefix = "HANNAH_SATELLITE_"

func Load(path string) (*Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	case os.IsNotExist(err):
		// The config file is optional as long as enough comes from env vars (see
		// applyEnvOverrides below) — unlike an existing but broken file, this isn't
		// a user error, it's the normal case for purely env-based deployments (Docker).
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	applyEnvOverrides(reflect.ValueOf(&cfg).Elem(), nil)

	if cfg.MQTT.Address == "" {
		return nil, fmt.Errorf("mqtt.address is required")
	}

	return &cfg, nil
}

// applyEnvOverrides walks val's fields via reflection (recursing into nested structs
// like HannahCfg/UDPCfg) and overrides each one whose HANNAH_SATELLITE_<PATH> environment
// variable is set, without any hardcoded allowlist. val must be an addressable struct
// value (e.g. reflect.ValueOf(&cfg).Elem()). Path segments come from the existing
// `json:"..."` tags. "." between nesting levels always becomes "__" (even without
// ambiguity) — otherwise reversing an env var name back into a nested path would be
// ambiguous, same reasoning as Core (#327). Plain "_" inside a tag name stays untouched
// (hannah.address -> HANNAH_SATELLITE_HANNAH__ADDRESS).
func applyEnvOverrides(val reflect.Value, pathPrefix []string) {
	typ := val.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fieldVal := val.Field(i)

		tag := strings.Split(field.Tag.Get("json"), ",")[0]
		if tag == "" {
			tag = strings.ToLower(field.Name)
		}
		path := append(append([]string{}, pathPrefix...), tag)

		if fieldVal.Kind() == reflect.Struct {
			applyEnvOverrides(fieldVal, path)
			continue
		}

		envName := envPrefix + strings.ToUpper(strings.Join(path, "__"))
		raw, ok := os.LookupEnv(envName)
		if !ok {
			continue
		}
		setField(fieldVal, raw)
	}
}

// setField assigns raw (always a string, since it comes from an env var) to fieldVal,
// coerced to whatever type the field already is — best-effort, no schema declared.
func setField(fieldVal reflect.Value, raw string) {
	switch fieldVal.Kind() {
	case reflect.String:
		fieldVal.SetString(raw)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			fieldVal.SetInt(n)
		}
	case reflect.Bool:
		if b, err := strconv.ParseBool(raw); err == nil {
			fieldVal.SetBool(b)
		}
	}
}
