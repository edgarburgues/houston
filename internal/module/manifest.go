// Package module implements Houston's external modules: directories under
// <StoreDir>/modules/<name>/ described by a module.json manifest, contributing
// TUI actions, mission-list transforms, preview sections, statusline segments
// and theme overrides through exec'd handlers. This file owns the manifest
// schema, name rules and validation — nothing here executes module code.
package module

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"houston/internal/accounts"
	"houston/internal/theme"
)

// Dir is where module directories live, under Houston's store.
func Dir() string { return filepath.Join(accounts.StoreDir(), "modules") }

// windowsReserved are device names Windows refuses as a file stem — with or
// without an extension ("CON.sync" is as invalid as "CON"). Copied from
// store.progFile to keep the dependency graph flat.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// nameCharset enforces ^[a-z0-9][a-z0-9._-]{0,63}$ — shared by module names
// and action ids. ASCII-only by construction, so len() counts runes.
func nameCharset(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// SafeName validates a module name. Names become directory names under
// modules/ and reach filepath.Join from more sources than the registry
// (unregistered dirs are a supported state), so every name-taking entry
// point must call this before touching the filesystem. The Windows lore
// comes from store.progFile: trailing dots are silently dropped by Windows
// ("foo." collides with "foo") and reserved device stems are refused
// outright, extension or not.
func SafeName(s string) error {
	if !nameCharset(s) {
		return fmt.Errorf("invalid module name %q (want ^[a-z0-9][a-z0-9._-]{0,63}$)", s)
	}
	if strings.HasSuffix(s, ".") {
		return fmt.Errorf("invalid module name %q (must not end in '.')", s)
	}
	if windowsReserved[strings.ToUpper(strings.SplitN(s, ".", 2)[0])] {
		return fmt.Errorf("invalid module name %q (Windows reserved device name)", s)
	}
	return nil
}

// within reports whether p stays inside root (no "../" escape).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// Manifest is the parsed module.json. Unknown fields at any level are
// silently ignored (forward compatibility); known fields with wrong types
// are hard errors. TimeoutMs is an optional module-wide fallback for
// handler timeouts, not a default — see ResolveTimeout.
type Manifest struct {
	API         int      `json:"api"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	TimeoutMs   int      `json:"timeoutMs"`
	Actions     []Action `json:"actions"`
	Transforms  struct {
		Missions *Handler `json:"missions"`
		Preview  *Handler `json:"preview"`
	} `json:"transforms"`
	Statusline *Segment         `json:"statusline"`
	PreLaunch  *Handler         `json:"preLaunch"`
	Views      []View           `json:"views"`
	Theme      *theme.Overrides `json:"theme"`
}

// Action is a user-invocable key binding contributed to a TUI screen.
// TimeoutMs is ignored when Interactive (the user owns the terminal).
type Action struct {
	ID           string   `json:"id"`
	Key          string   `json:"key"`
	Title        string   `json:"title"`
	Screen       string   `json:"screen"`
	Command      []string `json:"command"`
	Interactive  bool     `json:"interactive"`
	RefreshAfter bool     `json:"refreshAfter"`
	TimeoutMs    int      `json:"timeoutMs"`
}

// Handler is a transform hook (missions list or preview append).
type Handler struct {
	Command   []string `json:"command"`
	TimeoutMs int      `json:"timeoutMs"`
}

// Segment is a module's single statusline contribution.
type Segment struct {
	Command    []string `json:"command"`
	TTLSeconds int      `json:"ttlSeconds"`
	TimeoutMs  int      `json:"timeoutMs"`
}

// TTL returns the statusline cache TTL: ttlSeconds defaulting to 60 and
// clamped to [60, 3600]. The floor matches the statusline usage-cache TTL —
// anything lower would re-exec handlers on nearly every render (fork storm).
func (s Segment) TTL() time.Duration {
	t := s.TTLSeconds
	if t < 60 {
		t = 60
	}
	if t > 3600 {
		t = 3600
	}
	return time.Duration(t) * time.Second
}

// View is a module-contributed full-screen read-only page, opened from the
// missions screen by its key. The handler renders on demand (view.render)
// and on the r refresh key; plain text only, scrolled by Houston.
type View struct {
	ID        string   `json:"id"`
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Command   []string `json:"command"`
	TimeoutMs int      `json:"timeoutMs"`
}

// Surface identifies which per-surface timeout default and clamp applies to
// a handler exec.
type Surface int

const (
	SurfaceAction    Surface = iota // non-interactive action
	SurfaceTransform                // missions transform
	SurfacePreview                  // preview append
	SurfaceSegment                  // statusline segment
	SurfaceView                     // full-screen module view
)

// surfaceSpec is a surface's default timeout and clamp range, in ms.
type surfaceSpec struct{ def, min, max int }

var surfaceSpecs = [...]surfaceSpec{
	SurfaceAction:    {def: 10000, min: 500, max: 30000},
	SurfaceTransform: {def: 2000, min: 200, max: 10000},
	SurfacePreview:   {def: 3000, min: 200, max: 10000},
	SurfaceSegment:   {def: 4000, min: 500, max: 4000},
	SurfaceView:      {def: 8000, min: 500, max: 30000},
}

// ResolveTimeout resolves a handler's effective timeout: the handler-level
// timeoutMs if set, else the manifest's module-wide timeoutMs fallback, else
// the surface default — then clamped to the surface's range. Interactive
// actions never go through here: they have no timeout at all.
func (m Manifest) ResolveTimeout(s Surface, handlerMs int) time.Duration {
	spec := surfaceSpecs[s]
	ms := handlerMs
	if ms <= 0 {
		ms = m.TimeoutMs
	}
	if ms <= 0 {
		ms = spec.def
	}
	if ms < spec.min {
		ms = spec.min
	}
	if ms > spec.max {
		ms = spec.max
	}
	return time.Duration(ms) * time.Millisecond
}

// ParseManifest decodes and validates a module.json. Unknown fields are
// ignored, wrong-typed known fields and every §2 rule violation are errors.
// An unsupported api yields the "needs modules api N" error so ls/doctor can
// show the module as unavailable rather than broken.
func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("module.json: %v", err)
	}
	if err := m.validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) validate() error {
	if m.API == 0 {
		return errors.New(`manifest is missing "api": 1`)
	}
	if m.API != 1 {
		return fmt.Errorf("needs modules api %d; try houston update", m.API)
	}
	if err := SafeName(m.Name); err != nil {
		return err
	}
	if utf8.RuneCountInString(m.Description) > 200 {
		return errors.New("description exceeds 200 runes")
	}
	if len(m.Actions) > 16 {
		return fmt.Errorf("%d actions declared, max 16", len(m.Actions))
	}
	ids := map[string]bool{}
	keys := map[string]bool{}
	for i, a := range m.Actions {
		where := fmt.Sprintf("actions[%d]", i)
		if a.ID != "" {
			where = fmt.Sprintf("action %q", a.ID)
		}
		if !nameCharset(a.ID) {
			return fmt.Errorf("%s: invalid id (want ^[a-z0-9][a-z0-9._-]{0,63}$)", where)
		}
		if ids[a.ID] {
			return fmt.Errorf("%s: duplicate id", where)
		}
		ids[a.ID] = true
		if err := validKey(a.Key); err != nil {
			return fmt.Errorf("%s: %v", where, err)
		}
		if keys[a.Key] {
			return fmt.Errorf("%s: duplicate key %q", where, a.Key)
		}
		keys[a.Key] = true
		if a.Title == "" || utf8.RuneCountInString(a.Title) > 40 {
			return fmt.Errorf("%s: title must be 1-40 runes", where)
		}
		if a.Screen != "missions" && a.Screen != "accounts" {
			return fmt.Errorf(`%s: screen must be "missions" or "accounts"`, where)
		}
		if err := validateCommand(a.Command); err != nil {
			return fmt.Errorf("%s: %v", where, err)
		}
	}
	if m.Transforms.Missions != nil {
		if err := validateCommand(m.Transforms.Missions.Command); err != nil {
			return fmt.Errorf("transforms.missions: %v", err)
		}
	}
	if m.Transforms.Preview != nil {
		if err := validateCommand(m.Transforms.Preview.Command); err != nil {
			return fmt.Errorf("transforms.preview: %v", err)
		}
	}
	if m.Statusline != nil {
		if err := validateCommand(m.Statusline.Command); err != nil {
			return fmt.Errorf("statusline: %v", err)
		}
	}
	if m.PreLaunch != nil {
		// Always interactive (it exists to prompt the user before claude gets
		// the terminal), so the Handler's TimeoutMs is ignored — the user owns
		// the pace, and the exit code is the verdict.
		if err := validateCommand(m.PreLaunch.Command); err != nil {
			return fmt.Errorf("preLaunch: %v", err)
		}
	}
	if len(m.Views) > 8 {
		return fmt.Errorf("%d views declared, max 8", len(m.Views))
	}
	vids := map[string]bool{}
	vkeys := map[string]bool{}
	for i, v := range m.Views {
		where := fmt.Sprintf("views[%d]", i)
		if v.ID != "" {
			where = fmt.Sprintf("view %q", v.ID)
		}
		if !nameCharset(v.ID) {
			return fmt.Errorf("%s: invalid id (want ^[a-z0-9][a-z0-9._-]{0,63}$)", where)
		}
		if vids[v.ID] {
			return fmt.Errorf("%s: duplicate id", where)
		}
		vids[v.ID] = true
		if err := validKey(v.Key); err != nil {
			return fmt.Errorf("%s: %v", where, err)
		}
		// Views open from the missions screen and share its key space with
		// this module's own missions actions.
		if vkeys[v.Key] || keys[v.Key] {
			return fmt.Errorf("%s: duplicate key %q", where, v.Key)
		}
		vkeys[v.Key] = true
		if v.Title == "" || utf8.RuneCountInString(v.Title) > 40 {
			return fmt.Errorf("%s: title must be 1-40 runes", where)
		}
		if err := validateCommand(v.Command); err != nil {
			return fmt.Errorf("%s: %v", where, err)
		}
	}
	return nil
}

// ctrlAliases are ctrl combinations Bubble Tea normalizes into other keys —
// msg.String() can never produce them, so a binding on one would never fire
// and the help footer would advertise a dead key.
var ctrlAliases = map[string]string{
	"ctrl+i": "tab",
	"ctrl+m": "enter",
	"ctrl+[": "esc",
	"ctrl+h": "backspace",
}

// validKey accepts what msg.String() can actually deliver: a single
// printable rune, or "ctrl+" plus one rune minus the normalized aliases.
func validKey(k string) error {
	if produced, ok := ctrlAliases[k]; ok {
		return fmt.Errorf("key %q arrives as %q and can never fire", k, produced)
	}
	if rest, ok := strings.CutPrefix(k, "ctrl+"); ok {
		if !printableRune(rest) {
			return fmt.Errorf("invalid key %q (want a single printable rune or ctrl+<rune>)", k)
		}
		return nil
	}
	if !printableRune(k) {
		return fmt.Errorf("invalid key %q (want a single printable rune or ctrl+<rune>)", k)
	}
	return nil
}

func printableRune(s string) bool {
	r, size := utf8.DecodeRuneInString(s)
	return size > 0 && size == len(s) && r != utf8.RuneError && unicode.IsPrint(r)
}

var errCommandPath = errors.New("commands must be bare executable names or paths inside the module directory")

// validateCommand enforces the command-resolution rule on argv: len ≥ 1 and
// command[0] either a bare executable name (resolved via exec.LookPath at
// exec time) or a relative path that stays inside the module directory.
// Elements [1:] are never inspected — flags are indistinguishable from paths
// by shape, and rewriting them would break every command that takes options.
// This is review hygiene (what the user reads in the module dir is what
// runs), not a security boundary.
func validateCommand(argv []string) error {
	if len(argv) == 0 {
		return errors.New("command must have at least one element")
	}
	c := argv[0]
	if c == "" {
		return errors.New("command[0] must not be empty")
	}
	// Windows drive prefixes ("C:\x", drive-relative "c:x") are absolute-ish
	// on any OS the manifest is reviewed on; reject regardless of platform.
	drive := len(c) >= 2 && c[1] == ':'
	if !strings.ContainsAny(c, `/\`) {
		if drive {
			return errCommandPath
		}
		return nil // bare name → LookPath at exec time
	}
	// Normalize both separators before the escape check: manifests are
	// portable, so "..\evil" must be rejected on POSIX CI too.
	s := path.Clean(strings.ReplaceAll(c, `\`, "/"))
	if drive || path.IsAbs(s) || filepath.IsAbs(c) || s == ".." || strings.HasPrefix(s, "../") {
		return errCommandPath
	}
	return nil
}
