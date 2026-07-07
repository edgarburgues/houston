// Package config reads Houston's user configuration file,
// <StoreDir>/config.json. The file is hand-edited and read-only to Houston
// (machine-mutated state like the module registry lives in separate files):
// a missing or malformed config yields the zero value so a typo can never
// break startup — `houston doctor` is where problems get surfaced.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"houston/internal/accounts"
	"houston/internal/theme"
)

type Config struct {
	Theme   theme.Overrides         `json:"theme,omitempty"`
	Modules map[string]ModuleConfig `json:"modules,omitempty"`
}

// ModuleConfig holds per-module settings passed opaquely to that module's
// handlers in the envelope; Houston never interprets them, so RawMessage
// preserves whatever shape the module documents.
type ModuleConfig struct {
	Settings json.RawMessage `json:"settings,omitempty"`
}

// Path is where the user config lives, next to accounts.json.
func Path() string { return filepath.Join(accounts.StoreDir(), "config.json") }

// Load returns the parsed config. Missing or malformed files yield the zero
// value, never an error: config must not be able to take the TUI or the
// statusline down.
func Load() Config {
	c, _ := load()
	return c
}

// Check reports why an existing config.json is being ignored — Load hides
// the error by design, so doctor calls this to surface it. Missing file =
// nil (a config is optional).
func Check() error {
	_, err := load()
	return err
}

func load() (Config, error) {
	b, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	// The same PowerShell encoding traps module.DecodeReply hardens against:
	// default encodings prepend a UTF-8 BOM (stripped — json.Unmarshal would
	// reject it), and PS 5.1 `>` redirection writes UTF-16 (rejected with the
	// actionable message instead of an unreadable syntax error).
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	if len(b) >= 2 && ((b[0] == 0xFF && b[1] == 0xFE) || (b[0] == 0xFE && b[1] == 0xFF)) {
		return Config{}, errors.New("file is UTF-16; save it as UTF-8")
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		// Discard partial decode results: half a config is worse than none.
		return Config{}, err
	}
	return c, nil
}
