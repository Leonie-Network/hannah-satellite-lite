# Hannah Satellite (Lite)

A PC-based satellite for [Hannah](https://hannah-docs.leonie.network/) — runs over
the same MQTT/UDP path as the real ESP32 satellites, but doesn't need ESP hardware
and deliberately covers only part of their feature set.

## Features

- **MQTT control** (`internal/hannah/control.go`): mute, volume, `listen` (start
  recording), and `play_asset` — topic names and payload formats taken 1:1 from
  the ESP firmware, so Core can address Lite and ESP satellites the same way.
- **Audio** (`internal/hannah/audio.go`): microphone capture and speaker playback
  via [malgo](https://github.com/gen2brain/malgo), UDP connection to the Hannah
  proxy using the same 1-byte type-prefix protocol (control JSON / audio / TTS)
  as the ESP satellites, including register/heartbeat/reregister handling.
- **Keybindings** (`internal/hannah/keybindings.go`): global hotkeys for mute,
  push-to-talk, and volume — a local stand-in for the physical buttons on the
  ESP device.

### Deliberately out of scope

Being the "Lite" variant, this intentionally omits:

- Wakeword detection
- Real VAD (WebRTC) — replaced by a simple RMS threshold with hangover
- BLE presence watchlist, OTA updates, seed-based pairing/provisioning

## Requirements

- Go (see `go.mod` for the version)
- CGO enabled (`go env -w CGO_ENABLED=1`) plus a C compiler:
  - **Windows**: MinGW-w64, e.g. `scoop install mingw`
  - **Linux**: `build-essential` and `libx11-dev` (for
    [golang.design/x/hotkey](https://github.com/golang-design/hotkey))

## Configuration

`config.yaml` in the working directory (see [`config.example.yaml`](config.example.yaml)
as a template — `config.yaml` itself is gitignored). Every value can also be
overridden via env var with the `HANNAH_SATELLITE_` prefix (e.g.
`HANNAH_SATELLITE_MQTT__ADDRESS`), handy for Docker deployments without a config
file.

```yaml
mqtt:
  address: 192.168.1.1
  port: 1883
  username: ""
  password: ""

satellite:
  satellite_id: satellite-01

# Optional — leave empty to disable a binding.
keybindings:
  mute: ctrl+shift+m
  ptt: ctrl+shift+space
  vol_up: ctrl+shift+up
  vol_down: ctrl+shift+down
```

## Build & run

```sh
go build ./cmd/satellite
go run ./cmd/satellite
```

## Related projects

- [NurPech/Hannah](https://github.com/NurPech/Hannah) — public mirror of Hannah
  Core, and thereby also of the upstream project for the ESP satellites
  (`satellite-esp`), whose MQTT/UDP protocol this satellite follows.
- [hannah-docs.leonie.network](https://hannah-docs.leonie.network/) — official
  Hannah documentation.
