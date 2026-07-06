// Package config reads Houston's user configuration file,
// <StoreDir>/config.json. The file is hand-edited and read-only to Houston
// (machine-mutated state like the module registry lives in separate files):
// a missing or malformed config yields the zero value so a typo can never
// break startup — `houston doctor` is where problems get surfaced.
package config

import (
	"encoding/json"
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
	b, err := os.ReadFile(Path())
	if err != nil {
		return Config{}
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		// Discard partial decode results: half a config is worse than none.
		return Config{}
	}
	return c
}
