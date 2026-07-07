package module

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"houston/internal/config"
	"houston/internal/theme"
)

// writeModuleDir lands a module directory with a manifest, bypassing the
// (not yet existing) install path — registry tests exercise load, not add.
func writeModuleDir(t *testing.T, name, manifest string) {
	t.Helper()
	dir := filepath.Join(Dir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "module.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
}

func minManifest(name string) string {
	return `{"api":1,"name":"` + name + `"}`
}

func TestRegLoadMissing(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	list, err := RegLoad()
	if err != nil || list != nil {
		t.Errorf("RegLoad() = %v, %v; want nil, nil", list, err)
	}
}

func TestRegLoadMalformed(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	if err := os.WriteFile(regPath(), []byte(`{"modules": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RegLoad(); err == nil || !strings.Contains(err.Error(), "modules.json") {
		t.Errorf("RegLoad() err = %v, want modules.json parse error", err)
	}
}

func TestRegSaveSortsByName(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	in := []Entry{
		{Name: "zeta", Enabled: true, AddedAt: "2026-07-06T10:00:00Z"},
		{Name: "alpha", Source: "https://example.com/alpha"},
		{Name: "mid"},
	}
	if err := RegSave(in); err != nil {
		t.Fatal(err)
	}
	got, err := RegLoad()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("entry[%d].Name = %q, want %q", i, got[i].Name, name)
		}
	}
	// Fields survive the roundtrip verbatim.
	if !got[2].Enabled || got[2].AddedAt != "2026-07-06T10:00:00Z" {
		t.Errorf("zeta fields lost: %+v", got[2])
	}
	if got[0].Source != "https://example.com/alpha" {
		t.Errorf("alpha source lost: %+v", got[0])
	}
}

func TestSetEnabled(t *testing.T) {
	t.Run("rejects unsafe names before touching anything", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		for _, name := range []string{"../evil", `a\b`, "con", "Foo"} {
			if err := SetEnabled(name, true); err == nil {
				t.Errorf("SetEnabled(%q) = nil, want error", name)
			}
		}
		if _, err := os.Stat(regPath()); !os.IsNotExist(err) {
			t.Error("rejected SetEnabled must not create modules.json")
		}
	})
	t.Run("unregistered module errors", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		if err := SetEnabled("ghost", true); err == nil || !strings.Contains(err.Error(), "not registered") {
			t.Errorf("SetEnabled(ghost) = %v, want not-registered error", err)
		}
	})
	t.Run("enable/disable roundtrip", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		if err := RegSave([]Entry{{Name: "mod"}, {Name: "other", Enabled: true}}); err != nil {
			t.Fatal(err)
		}
		if err := SetEnabled("mod", true); err != nil {
			t.Fatal(err)
		}
		list, _ := RegLoad()
		if !list[0].Enabled || !list[1].Enabled {
			t.Errorf("after enable: %+v", list)
		}
		if err := SetEnabled("mod", false); err != nil {
			t.Fatal(err)
		}
		list, _ = RegLoad()
		if list[0].Enabled {
			t.Errorf("after disable: %+v", list)
		}
		if !list[1].Enabled {
			t.Error("SetEnabled(mod) touched the other entry")
		}
	})
}

// TestSetEnabledConcurrent pins that the registry flock makes SetEnabled a
// real read-modify-write: concurrent flips of distinct entries must not lose
// each other's updates.
func TestSetEnabledConcurrent(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	names := []string{"m0", "m1", "m2", "m3", "m4", "m5", "m6", "m7"}
	var entries []Entry
	for _, n := range names {
		entries = append(entries, Entry{Name: n})
	}
	if err := RegSave(entries); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(names))
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = SetEnabled(n, true)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("SetEnabled(%s) = %v", names[i], err)
		}
	}
	list, err := RegLoad()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list {
		if !e.Enabled {
			t.Errorf("update to %s lost under concurrency", e.Name)
		}
	}
}

func TestLoadEnabled(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	// Deliberately registered out of order: load order must be lexicographic
	// regardless of registry file order.
	writeModuleDir(t, "bravo", minManifest("bravo"))
	writeModuleDir(t, "alpha", minManifest("alpha"))
	writeModuleDir(t, "charlie", minManifest("charlie"))
	writeModuleDir(t, "delta", minManifest("delta"))     // registered but disabled
	writeModuleDir(t, "echo", `{"api":2,"name":"echo"}`) // unsupported api
	writeModuleDir(t, "foxtrot", `{not json`)            // broken manifest
	writeModuleDir(t, "golf", minManifest("renamed"))    // manifest/dir name mismatch
	entries := []Entry{
		{Name: "charlie", Enabled: true},
		{Name: "alpha", Enabled: true},
		{Name: "bravo", Enabled: true},
		{Name: "delta", Enabled: false},
		{Name: "echo", Enabled: true},
		{Name: "foxtrot", Enabled: true},
		{Name: "golf", Enabled: true},
		{Name: "hotel", Enabled: true}, // no directory on disk
	}
	if err := RegSave(entries); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Modules: map[string]config.ModuleConfig{
		"bravo": {Settings: json.RawMessage(`{"url":"https://x"}`)},
	}}
	mods, warns := LoadEnabled(cfg)
	var names []string
	for _, m := range mods {
		names = append(names, m.Name)
	}
	if want := "alpha bravo charlie"; strings.Join(names, " ") != want {
		t.Errorf("loaded %q, want %q (lexicographic, enabled only)", names, want)
	}
	for _, m := range mods {
		if m.Dir != filepath.Join(Dir(), m.Name) {
			t.Errorf("module %s: Dir = %q", m.Name, m.Dir)
		}
		if m.Manifest.Name != m.Name {
			t.Errorf("module %s: manifest name %q", m.Name, m.Manifest.Name)
		}
	}
	if got := string(mods[1].Settings); got != `{"url":"https://x"}` {
		t.Errorf("bravo settings = %q, want config passthrough", got)
	}
	if mods[0].Settings != nil {
		t.Errorf("alpha settings = %q, want nil (not configured)", mods[0].Settings)
	}
	wantWarns := map[string]string{
		"echo":    "needs modules api 2; try houston update",
		"foxtrot": "module.json",
		"golf":    `manifest name "renamed" does not match directory name "golf"`,
		"hotel":   "missing (registered but no directory)",
	}
	if len(warns) != len(wantWarns) {
		t.Fatalf("got %d warnings %q, want %d", len(warns), warns, len(wantWarns))
	}
	for name, frag := range wantWarns {
		found := false
		for _, w := range warns {
			if strings.Contains(w, "module "+name+":") && strings.Contains(w, frag) {
				found = true
			}
		}
		if !found {
			t.Errorf("no warning for %s containing %q in %q", name, frag, warns)
		}
	}
}

// TestLoadEnabledUnsafeRegistryName pins that names are re-validated at load:
// the registry is a plain file anyone can edit, and its names reach
// filepath.Join.
func TestLoadEnabledUnsafeRegistryName(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	// RegSave does not police names — SafeName runs at every use instead —
	// so a hostile entry lands the same way a hand edit would.
	if err := RegSave([]Entry{{Name: "../escape", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	mods, warns := LoadEnabled(config.Config{})
	if len(mods) != 0 {
		t.Errorf("loaded %+v, want none", mods)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "invalid module name") {
		t.Errorf("warns = %q, want invalid-name warning", warns)
	}
}

func TestLoadAll(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	writeModuleDir(t, "alpha", minManifest("alpha"))
	writeModuleDir(t, "bravo", minManifest("bravo"))   // registered but disabled
	writeModuleDir(t, "orphan", minManifest("orphan")) // on disk, not registered
	writeModuleDir(t, ".staging-x", `{}`)              // transient install staging
	if err := os.WriteFile(filepath.Join(Dir(), "file"), nil, 0o600); err != nil {
		t.Fatal(err) // plain file next to module dirs: ignored
	}
	entries := []Entry{
		{Name: "bravo", Enabled: false},
		{Name: "alpha", Enabled: true},
		{Name: "gone", Enabled: true}, // no directory
	}
	if err := RegSave(entries); err != nil {
		t.Fatal(err)
	}
	mods, errs := LoadAll(config.Config{})
	var names []string
	for _, m := range mods {
		names = append(names, m.Name)
	}
	if want := "alpha bravo"; strings.Join(names, " ") != want {
		t.Errorf("loaded %q, want %q (disabled included, lexicographic)", names, want)
	}
	wantErrs := map[string]string{
		"gone":   "missing (registered but no directory)",
		"orphan": "unregistered",
	}
	if len(errs) != len(wantErrs) {
		t.Fatalf("got %d errors %q, want %d", len(errs), errs, len(wantErrs))
	}
	for name, frag := range wantErrs {
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), "module "+name+":") && strings.Contains(e.Error(), frag) {
				found = true
			}
		}
		if !found {
			t.Errorf("no error for %s containing %q in %q", name, frag, errs)
		}
	}
}

func TestThemeOverrides(t *testing.T) {
	withTheme := func(name, accent string) Module {
		m := Module{Entry: Entry{Name: name}}
		m.Manifest.Theme = &theme.Overrides{Colors: map[string]string{"accent": accent}}
		return m
	}
	// Slice order is preserved (LoadEnabled sorts, this must not re-sort) and
	// modules without a theme contribute nothing rather than a zero override.
	mods := []Module{
		withTheme("bravo", "75"),
		{Entry: Entry{Name: "alpha"}}, // no theme
		withTheme("charlie", "99"),
	}
	got := ThemeOverrides(mods)
	if len(got) != 2 {
		t.Fatalf("got %d overrides, want 2: %+v", len(got), got)
	}
	if got[0].Colors["accent"] != "75" || got[1].Colors["accent"] != "99" {
		t.Errorf("order not preserved: %+v", got)
	}
	if out := ThemeOverrides(nil); out != nil {
		t.Errorf("ThemeOverrides(nil) = %+v, want nil", out)
	}
}

// TestLoadEnabledThemePrecedence pins the wiring end to end: manifests on
// disk → LoadEnabled (lexicographic) → ThemeOverrides → theme.Resolve, with
// the later module name winning per field and config.json trumping both.
func TestLoadEnabledThemePrecedence(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	writeModuleDir(t, "aaa", `{"api":1,"name":"aaa","theme":{"colors":{"accent":"75","green":"34"}}}`)
	writeModuleDir(t, "bbb", `{"api":1,"name":"bbb","theme":{"colors":{"accent":"99","yellow":"999"}}}`)
	writeModuleDir(t, "ccc", minManifest("ccc")) // enabled, no theme
	// Registered out of lex order on purpose.
	entries := []Entry{
		{Name: "bbb", Enabled: true},
		{Name: "ccc", Enabled: true},
		{Name: "aaa", Enabled: true},
	}
	if err := RegSave(entries); err != nil {
		t.Fatal(err)
	}
	mods, warns := LoadEnabled(config.Config{})
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %q", warns)
	}
	user := theme.Overrides{Colors: map[string]string{"green": "40"}}
	got := theme.Resolve(ThemeOverrides(mods), user)
	if got.Colors.Accent != "99" {
		t.Errorf("accent = %q, want 99 (later lex module wins)", got.Colors.Accent)
	}
	if got.Colors.Green != "40" {
		t.Errorf("green = %q, want 40 (config.json trumps modules)", got.Colors.Green)
	}
	if def := theme.Default().Colors.Yellow; got.Colors.Yellow != def {
		t.Errorf("yellow = %q, want default %q (invalid module color skipped per-field)", got.Colors.Yellow, def)
	}
	if def := theme.Default().Colors.Grey; got.Colors.Grey != def {
		t.Errorf("grey = %q, want default %q (untouched field)", got.Colors.Grey, def)
	}
}

func TestLoadAllBadRegistry(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(regPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(regPath(), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeModuleDir(t, "orphan", minManifest("orphan"))
	mods, errs := LoadAll(config.Config{})
	if len(mods) != 0 {
		t.Errorf("loaded %+v from a broken registry", mods)
	}
	// The registry error must not mask the on-disk scan.
	if len(errs) != 2 {
		t.Fatalf("errs = %q, want registry error + unregistered orphan", errs)
	}
}

func TestWithin(t *testing.T) {
	root := filepath.Join("some", "root")
	tests := []struct {
		p    string
		want bool
	}{
		{filepath.Join(root, "child"), true},
		{filepath.Join(root, "a", "b"), true},
		{root, true},
		{filepath.Join(root, ".."), false},
		{filepath.Join(root, "..", "sibling"), false},
		{filepath.Join("other", "tree"), false},
	}
	for _, tt := range tests {
		if got := within(root, tt.p); got != tt.want {
			t.Errorf("within(%q, %q) = %v, want %v", root, tt.p, got, tt.want)
		}
	}
}
