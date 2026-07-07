package module

// The registry (<StoreDir>/modules.json) is machine-mutated state and lives
// apart from the hand-edited config.json — the two disciplines must not
// share a file. Mutations follow the accounts.json pattern: flock around a
// Load-modify-Save cycle, unique-temp atomic writes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"houston/internal/accounts"
	"houston/internal/config"
	"houston/internal/flock"
	"houston/internal/theme"
)

// Entry is one row of modules.json. Source is informational only — installs
// are snapshots, never auto-refetched (update = rm + add). A directory
// without an entry is "unregistered" (flagged by ls/doctor); an entry
// without a directory is "missing" (skipped at load).
type Entry struct {
	Name    string `json:"name"`
	Source  string `json:"source,omitempty"`
	AddedAt string `json:"addedAt"`
	Enabled bool   `json:"enabled"`
}

func regPath() string { return filepath.Join(accounts.StoreDir(), "modules.json") }

// regLockWait caps how long a registry mutation waits for the lock; these
// are read-modify-write cycles over a small JSON file, so contention is brief.
const regLockWait = 3 * time.Second

// lockRegistry serializes Load-modify-Save cycles on modules.json across
// processes: without it concurrent writers lose each other's updates.
func lockRegistry() (*flock.Lock, error) {
	_ = os.MkdirAll(accounts.StoreDir(), 0o700)
	return flock.Acquire(regPath()+".lock", regLockWait)
}

// RegLoad returns the registry entries (empty if none yet).
func RegLoad() ([]Entry, error) {
	b, err := os.ReadFile(regPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var on struct {
		Modules []Entry `json:"modules"`
	}
	if err := json.Unmarshal(b, &on); err != nil {
		return nil, fmt.Errorf("modules.json: %v", err)
	}
	return on.Modules, nil
}

// RegSave writes the registry atomically, sorted by name. Mutators hold the
// registry lock around their whole Load-modify-Save cycle; RegSave itself
// does not lock (flock is not reentrant).
func RegSave(list []Entry) error {
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.MarshalIndent(struct {
		Modules []Entry `json:"modules"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(accounts.StoreDir(), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(regPath(), b, 0o600)
}

// SetEnabled flips a registered module's enabled bit under the registry lock.
func SetEnabled(name string, on bool) error {
	if err := SafeName(name); err != nil {
		return err
	}
	lk, err := lockRegistry()
	if err != nil {
		return err
	}
	defer lk.Release()
	list, err := RegLoad()
	if err != nil {
		return err
	}
	for i := range list {
		if list[i].Name == name {
			list[i].Enabled = on
			return RegSave(list)
		}
	}
	return fmt.Errorf("module %q is not registered", name)
}

// Module is a fully loaded module: its registry entry, parsed manifest,
// on-disk directory, and the user's opaque settings from config.json. Plain
// copyable data by design — it travels through Bubble Tea value receivers
// and goroutine closures.
type Module struct {
	Entry
	Manifest Manifest
	Dir      string
	Settings json.RawMessage
}

// LoadEnabled loads every enabled module in lexicographic name order.
// Broken modules (missing dir, bad manifest, unsupported api, name mismatch)
// are skipped with a warning string — a broken module must never take
// startup down.
func LoadEnabled(cfg config.Config) ([]Module, []string) {
	entries, err := RegLoad()
	if err != nil {
		return nil, []string{fmt.Sprintf("module registry: %v", err)}
	}
	sortEntries(entries)
	var mods []Module
	var warns []string
	for _, e := range entries {
		if !e.Enabled {
			continue
		}
		m, err := loadOne(e, cfg)
		if err != nil {
			warns = append(warns, fmt.Sprintf("module %s: %v", e.Name, err))
			continue
		}
		mods = append(mods, m)
	}
	return mods, warns
}

// LoadAll loads every registered module (enabled or not) in lexicographic
// name order, for ls/doctor. Every problem — missing dirs, broken manifests,
// unsupported api, and module directories present on disk but absent from
// the registry ("unregistered") — is reported as an error naming the module.
func LoadAll(cfg config.Config) ([]Module, []error) {
	var errs []error
	entries, err := RegLoad()
	if err != nil {
		errs = append(errs, fmt.Errorf("module registry: %v", err))
		entries = nil
	}
	sortEntries(entries)
	var mods []Module
	registered := map[string]bool{}
	for _, e := range entries {
		registered[e.Name] = true
		m, err := loadOne(e, cfg)
		if err != nil {
			errs = append(errs, fmt.Errorf("module %s: %v", e.Name, err))
			continue
		}
		mods = append(mods, m)
	}
	// Dirs without a registry entry are a supported (if flagged) state; dot
	// dirs (install staging) are transient and swept elsewhere.
	dirents, _ := os.ReadDir(Dir())
	for _, d := range dirents {
		if !d.IsDir() || registered[d.Name()] || d.Name()[0] == '.' {
			continue
		}
		errs = append(errs, fmt.Errorf("module %s: unregistered (directory present but not in modules.json)", d.Name()))
	}
	return mods, errs
}

func sortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
}

// ThemeOverrides collects the modules' theme contributions in slice order;
// modules without one are skipped. LoadEnabled returns modules sorted
// lexicographically by name, so feeding its result straight to theme.Resolve
// yields the documented precedence: defaults < module themes (later name wins
// per field) < config.json.
func ThemeOverrides(mods []Module) []theme.Overrides {
	var out []theme.Overrides
	for _, m := range mods {
		if m.Manifest.Theme != nil {
			out = append(out, *m.Manifest.Theme)
		}
	}
	return out
}

// loadOne reads and validates one registered module from disk. The name is
// re-validated even though installs check it too: the registry is a file
// anyone can edit, and the name goes straight into filepath.Join.
func loadOne(e Entry, cfg config.Config) (Module, error) {
	if err := SafeName(e.Name); err != nil {
		return Module{}, err
	}
	dir := filepath.Join(Dir(), e.Name)
	if !within(Dir(), dir) {
		return Module{}, errors.New("name escapes the modules directory")
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return Module{}, errors.New("missing (registered but no directory)")
	}
	b, err := os.ReadFile(filepath.Join(dir, "module.json"))
	if err != nil {
		return Module{}, fmt.Errorf("module.json: %v", err)
	}
	man, err := ParseManifest(b)
	if err != nil {
		return Module{}, err
	}
	if man.Name != e.Name {
		return Module{}, fmt.Errorf("manifest name %q does not match directory name %q", man.Name, e.Name)
	}
	return Module{Entry: e, Manifest: man, Dir: dir, Settings: cfg.Modules[e.Name].Settings}, nil
}

// writeFileAtomic writes b via a uniquely-named same-dir temp file + rename.
// The unique name matters as much as the rename: two processes writing the
// same fixed ".tmp" path can interleave and rename corrupted bytes into place.
func writeFileAtomic(p string, b []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, p); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
