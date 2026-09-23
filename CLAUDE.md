# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`hannah-satellite-lite`: a PC-based satellite for Hannah that speaks the same MQTT/UDP protocol as the ESP32 satellites (`satellite-esp` in the Hannah repo), so Core addresses Lite and ESP satellites identically. **Wire compatibility with the ESP firmware is the core constraint** — topic names, payload formats, UDP packet types and ports are copied 1:1 from `satellite-esp/components/hannah_net/hannah_net.c`. Don't "improve" them unilaterally.

Deliberately out of scope (Lite): wakeword, WebRTC VAD (replaced by RMS threshold + hangover), BLE presence, OTA, seed-based pairing.

Code, comments and docs are in English.

## Commands

CGO is required (malgo for audio, `golang.design/x/hotkey` for keybindings): `CGO_ENABLED=1` plus a C compiler (MinGW-w64 on Windows; `build-essential` + `libx11-dev` on Linux).

```sh
go build ./cmd/satellite
go run ./cmd/satellite          # reads ./config.yaml (gitignored; template: config.example.yaml)
go vet ./...
go test ./...
go test ./internal/config -run TestName
```

CI (`.github/workflows/test-and-release.yml`) runs `go vet` + `go test` on Ubuntu and Windows. Pushing a `vX.Y.Z` tag builds native linux/windows amd64 binaries (no CGO cross-compile) and attaches them to a GitHub Release.

## Architecture

`cmd/satellite/main.go` is the wiring point: it builds `Audio` and `Control`, connects them via function fields (callbacks), passes their subscriptions to `ConnectMQTT`, then registers keybindings. Components don't reference each other directly — cross-component behavior is always wired in `main.go` (e.g. `audio.Muted = control.Muted`, `control.OnListen = audio.StartListening`, `audio.OnPlaybackDone → control.PublishPlaybackDone`).

- **`internal/hannah/control.go`** — owns mute/volume state. Subscribes to `hannah/satellite/<id>/{mute/set,volume/set,listen,play_asset}`, publishes retained `mute/state`/`volume/state` and non-retained `playback_done`. Local actions (`ToggleMute`, `AdjustVolume`) go through the same setters as MQTT commands so state is always republished.
- **`internal/hannah/audio.go`** — malgo capture/playback at 16 kHz mono S16, 30 ms frames (960 bytes), plus the UDP link to the Hannah proxy:
  - Proxy address arrives via MQTT broadcast `hannah/server` (`{"host","port"}` or `host:port`); on receipt the satellite sends `register`. Until then all sends are dropped.
  - UDP packets have a 1-byte type prefix: `0x01` control JSON (both ways), `0x02` mic PCM (→ proxy), `0x03` TTS PCM (→ satellite). Local listen port 7776, heartbeat every 10 s; `reregister` from the proxy triggers a fresh `register`.
  - Mic is always running; frames are only sent during a listen window. `StartListening` (from MQTT `listen`) ends on silence/timeout and sends `audio_end`; `StartPTT`/`StopPTT` (keybinding) is manual-only.
  - Playback: TTS PCM is appended to `playBuf`; `tts_end` arms `playback_done`, which fires once the buffer drains. Volume is applied in the playback callback.
  - Device-stop callbacks reinitialize capture/playback with backoff (OS default-device changes, WASAPI invalidation) — otherwise audio would silently die.
- **`internal/hannah/mqtt.go`** — `ConnectMQTT` with auto-reconnect; subscriptions are (re)established in the OnConnect handler, so they survive reconnects.
- **`internal/hannah/keybindings.go`** — global hotkeys as a stand-in for the ESP buttons. Only `ctrl`/`shift` modifiers are supported on purpose (Alt/Win aren't portable to X11).
- **`internal/config`** — YAML via `sigs.k8s.io/yaml` (so struct tags are `json:"..."`). Config file is optional; every field can be overridden by env var `HANNAH_SATELLITE_<PATH>`, where nesting levels are joined by `__` (e.g. `HANNAH_SATELLITE_MQTT__ADDRESS`). This is done by reflection over the json tags — new config fields get env overrides automatically, no allowlist to update.

### Gotchas

- malgo data callbacks (`onCapture`, `onPlayback`) run on the audio thread and must not block — anything that may block (MQTT publish) is dispatched in a goroutine.
- `Audio` uses several mutexes with distinct scopes (`deviceMu` devices/reinit, `mu` proxy+listen state, `captureMu`, `playMu`); keep new state under the matching one.
- `play_asset` is currently a TODO stub in `main.go`.
