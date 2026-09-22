package hannah

import (
	"fmt"
	"log"
	"strings"

	"golang.design/x/hotkey"

	"hannah-satellite-lite/internal/config"
)

// Keybindings registers optional global hotkeys as a local stand-in for the
// physical buttons on the ESP satellites (mute, PTT, volume).
type Keybindings struct {
	ToggleMute func()
	PTTDown    func()
	PTTUp      func()
	VolumeUp   func()
	VolumeDown func()

	registered []*hotkey.Hotkey
}

type keybinding struct {
	name string
	spec string
	down func()
	up   func()
}

// Register registers every binding set in cfg. An empty binding is skipped;
// if a single registration fails (e.g. because another application already
// holds it), that's logged and the remaining bindings are still registered.
func (k *Keybindings) Register(cfg config.KeybindingsCfg) {
	bindings := []keybinding{
		{"mute", cfg.Mute, k.ToggleMute, nil},
		{"ptt", cfg.PTT, k.PTTDown, k.PTTUp},
		{"vol_up", cfg.VolUp, k.VolumeUp, nil},
		{"vol_down", cfg.VolDown, k.VolumeDown, nil},
	}

	for _, b := range bindings {
		if b.spec == "" {
			continue
		}

		mods, key, err := parseHotkey(b.spec)
		if err != nil {
			log.Printf("[Keybindings] %s (%q) invalid: %v", b.name, b.spec, err)
			continue
		}

		hk := hotkey.New(mods, key)
		if err := hk.Register(); err != nil {
			log.Printf("[Keybindings] %s (%s) could not be registered: %v", b.name, b.spec, err)
			continue
		}
		k.registered = append(k.registered, hk)
		log.Printf("[Keybindings] %s: %s", b.name, b.spec)

		down, up := b.down, b.up
		go func() {
			for range hk.Keydown() {
				if down != nil {
					down()
				}
			}
		}()
		if up != nil {
			go func() {
				for range hk.Keyup() {
					up()
				}
			}()
		}
	}
}

// Close removes all registered hotkeys.
func (k *Keybindings) Close() {
	for _, hk := range k.registered {
		_ = hk.Unregister()
	}
}

func parseHotkey(spec string) ([]hotkey.Modifier, hotkey.Key, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(spec)), "+")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return nil, 0, fmt.Errorf("empty key combination")
	}

	var mods []hotkey.Modifier
	for _, p := range parts[:len(parts)-1] {
		mod, ok := modifiers[p]
		if !ok {
			return nil, 0, fmt.Errorf("unknown modifier: %q", p)
		}
		mods = append(mods, mod)
	}

	key, ok := keys[parts[len(parts)-1]]
	if !ok {
		return nil, 0, fmt.Errorf("unknown key: %q", parts[len(parts)-1])
	}
	return mods, key, nil
}

// Ctrl/Shift only — Alt and Win/Super aren't addressable as a named modifier
// on Linux (X11), which only has the raw Mod1-Mod5, so allowing them would
// break the cross-platform build. "strg" is kept as a German alias for ctrl
// for config-file convenience.
var modifiers = map[string]hotkey.Modifier{
	"ctrl":  hotkey.ModCtrl,
	"strg":  hotkey.ModCtrl,
	"shift": hotkey.ModShift,
}

var keys = map[string]hotkey.Key{
	"a": hotkey.KeyA, "b": hotkey.KeyB, "c": hotkey.KeyC, "d": hotkey.KeyD,
	"e": hotkey.KeyE, "f": hotkey.KeyF, "g": hotkey.KeyG, "h": hotkey.KeyH,
	"i": hotkey.KeyI, "j": hotkey.KeyJ, "k": hotkey.KeyK, "l": hotkey.KeyL,
	"m": hotkey.KeyM, "n": hotkey.KeyN, "o": hotkey.KeyO, "p": hotkey.KeyP,
	"q": hotkey.KeyQ, "r": hotkey.KeyR, "s": hotkey.KeyS, "t": hotkey.KeyT,
	"u": hotkey.KeyU, "v": hotkey.KeyV, "w": hotkey.KeyW, "x": hotkey.KeyX,
	"y": hotkey.KeyY, "z": hotkey.KeyZ,

	"0": hotkey.Key0, "1": hotkey.Key1, "2": hotkey.Key2, "3": hotkey.Key3,
	"4": hotkey.Key4, "5": hotkey.Key5, "6": hotkey.Key6, "7": hotkey.Key7,
	"8": hotkey.Key8, "9": hotkey.Key9,

	"space":  hotkey.KeySpace,
	"return": hotkey.KeyReturn,
	"enter":  hotkey.KeyReturn,
	"escape": hotkey.KeyEscape,
	"esc":    hotkey.KeyEscape,
	"delete": hotkey.KeyDelete,
	"tab":    hotkey.KeyTab,

	"left":  hotkey.KeyLeft,
	"right": hotkey.KeyRight,
	"up":    hotkey.KeyUp,
	"down":  hotkey.KeyDown,

	"f1": hotkey.KeyF1, "f2": hotkey.KeyF2, "f3": hotkey.KeyF3, "f4": hotkey.KeyF4,
	"f5": hotkey.KeyF5, "f6": hotkey.KeyF6, "f7": hotkey.KeyF7, "f8": hotkey.KeyF8,
	"f9": hotkey.KeyF9, "f10": hotkey.KeyF10, "f11": hotkey.KeyF11, "f12": hotkey.KeyF12,
}
