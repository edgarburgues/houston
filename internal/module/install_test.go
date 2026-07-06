package module

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"houston/internal/config"
)

// writeSourceDir materializes a module source tree in its own temp dir,
// outside the HOUSTON_HOME store the test points at.
func writeSourceDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// assertNoStagingLeftovers pins that every install path — success or failure —
// cleans its .staging-* dir.
func assertNoStagingLeftovers(t *testing.T) {
	t.Helper()
	dirents, _ := os.ReadDir(Dir())
	for _, d := range dirents {
		if strings.HasPrefix(d.Name(), ".staging-") {
			t.Errorf("staging dir left behind: %s", d.Name())
		}
	}
}

func TestAddLocal(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	manifest := `{"api":1,"name":"hello","version":"0.1.0","actions":[{"id":"greet","key":"H","title":"say hello","screen":"missions","command":["pwsh","-NoProfile","-File","scripts/greet.ps1"]}]}`
	src := writeSourceDir(t, map[string]string{
		"module.json":       manifest,
		"scripts/greet.ps1": "Write-Output hi\n",
	})
	e, err := Add(src, "")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "hello" || e.Enabled {
		t.Errorf("entry = %+v, want name hello, disabled", e)
	}
	if e.AddedAt == "" {
		t.Error("AddedAt not set")
	}
	abs, _ := filepath.Abs(src)
	if e.Source != abs {
		t.Errorf("Source = %q, want the absolute source path %q", e.Source, abs)
	}
	// Files landed, nested dirs included.
	b, err := os.ReadFile(filepath.Join(Dir(), "hello", "scripts", "greet.ps1"))
	if err != nil || string(b) != "Write-Output hi\n" {
		t.Errorf("copied script = %q, %v", b, err)
	}
	list, _ := RegLoad()
	if len(list) != 1 || list[0].Name != "hello" || list[0].Enabled {
		t.Errorf("registry = %+v, want one disabled entry", list)
	}
	mods, errs := LoadAll(config.Config{})
	if len(errs) != 0 || len(mods) != 1 || mods[0].Name != "hello" {
		t.Errorf("LoadAll = %+v, %q", mods, errs)
	}
	if enabled, _ := LoadEnabled(config.Config{}); len(enabled) != 0 {
		t.Error("a fresh install must not load as enabled")
	}
	assertNoStagingLeftovers(t)
}

func TestAddForcedName(t *testing.T) {
	t.Run("installs under the forced name, manifest rewritten", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		src := writeSourceDir(t, map[string]string{
			"module.json": `{"api":1,"name":"orig","future":{"keep":true}}`,
		})
		e, err := Add(src, "renamed")
		if err != nil {
			t.Fatal(err)
		}
		if e.Name != "renamed" {
			t.Fatalf("entry name = %q, want renamed", e.Name)
		}
		b, err := os.ReadFile(filepath.Join(Dir(), "renamed", "module.json"))
		if err != nil {
			t.Fatal(err)
		}
		man, err := ParseManifest(b)
		if err != nil || man.Name != "renamed" {
			t.Errorf("landed manifest name = %q, %v", man.Name, err)
		}
		if !strings.Contains(string(b), `"future"`) {
			t.Error("unknown manifest fields must survive the --name rewrite")
		}
		if _, errs := LoadAll(config.Config{}); len(errs) != 0 {
			t.Errorf("LoadAll after --name install: %q", errs)
		}
	})
	t.Run("forced name equal to the manifest name is a no-op", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		src := writeSourceDir(t, map[string]string{"module.json": minManifest("same")})
		if _, err := Add(src, "same"); err != nil {
			t.Fatal(err)
		}
		if _, errs := LoadAll(config.Config{}); len(errs) != 0 {
			t.Errorf("LoadAll: %q", errs)
		}
	})
	t.Run("invalid forced name rejected before anything lands", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		src := writeSourceDir(t, map[string]string{"module.json": minManifest("ok")})
		for _, bad := range []string{"../evil", "CON", "a b", "nul.sync"} {
			if _, err := Add(src, bad); err == nil {
				t.Errorf("Add(--name %q) = nil, want error", bad)
			}
		}
		if _, err := os.Stat(Dir()); !os.IsNotExist(err) {
			t.Error("a rejected --name must not create the modules dir")
		}
	})
	t.Run("resolves a directory collision", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		// An unregistered dir already occupies the manifest name.
		writeModuleDir(t, "taken", minManifest("taken"))
		src := writeSourceDir(t, map[string]string{"module.json": minManifest("taken")})
		if _, err := Add(src, ""); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("Add = %v, want directory-exists error", err)
		}
		if _, err := Add(src, "taken2"); err != nil {
			t.Fatalf("Add(--name taken2) = %v, want the collision resolved", err)
		}
	})
}

func TestAddCollisions(t *testing.T) {
	t.Run("registered name, case-insensitively", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		// Hand-edited registries can carry any casing; collisions must fold case
		// because NTFS/APFS alias case variants onto one directory.
		if err := RegSave([]Entry{{Name: "Hello"}}); err != nil {
			t.Fatal(err)
		}
		src := writeSourceDir(t, map[string]string{"module.json": minManifest("hello")})
		if _, err := Add(src, ""); err == nil || !strings.Contains(err.Error(), "already registered") {
			t.Errorf("Add = %v, want already-registered error", err)
		}
	})
	t.Run("existing dir, case-insensitively", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		if err := os.MkdirAll(filepath.Join(Dir(), "HELLO"), 0o700); err != nil {
			t.Fatal(err)
		}
		src := writeSourceDir(t, map[string]string{"module.json": minManifest("hello")})
		if _, err := Add(src, ""); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("Add = %v, want directory-exists error", err)
		}
	})
	t.Run("unregistered dir is never clobbered", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		writeModuleDir(t, "hello", `{"user":"edits"}`)
		src := writeSourceDir(t, map[string]string{"module.json": minManifest("hello")})
		if _, err := Add(src, ""); err == nil {
			t.Fatal("Add over an unregistered dir must fail")
		}
		b, _ := os.ReadFile(filepath.Join(Dir(), "hello", "module.json"))
		if string(b) != `{"user":"edits"}` {
			t.Errorf("existing dir was clobbered: %q", b)
		}
		assertNoStagingLeftovers(t)
	})
}

func TestAddBadSource(t *testing.T) {
	t.Run("directory without module.json", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		src := writeSourceDir(t, map[string]string{"README.md": "hi"})
		if _, err := Add(src, ""); err == nil || !strings.Contains(err.Error(), "module.json") {
			t.Errorf("Add = %v, want missing-manifest error", err)
		}
	})
	t.Run("unsupported api", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		src := writeSourceDir(t, map[string]string{"module.json": `{"api":2,"name":"future"}`})
		if _, err := Add(src, ""); err == nil || !strings.Contains(err.Error(), "needs modules api 2") {
			t.Errorf("Add = %v, want api error", err)
		}
		if _, err := os.Stat(Dir()); !os.IsNotExist(err) {
			t.Error("a rejected manifest must not create the modules dir")
		}
	})
	t.Run("hostile git sources rejected without exec", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		for _, src := range []string{"ext::sh -c whoami", "file:///etc", "-oProxyCommand=calc", "fd::17"} {
			if _, err := Add(src, ""); err == nil {
				t.Errorf("Add(%q) = nil, want rejection", src)
			}
		}
	})
}

func TestAddRejectsSymlink(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := writeSourceDir(t, map[string]string{"module.json": minManifest("linky")})
	if err := os.Symlink(secret, filepath.Join(src, "link")); err != nil {
		t.Skipf("cannot create symlinks here (%v)", err) // Windows without Developer Mode
	}
	_, err := Add(src, "")
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Add = %v, want the install to FAIL on a symlink, not skip it", err)
	}
	if _, err := os.Stat(filepath.Join(Dir(), "linky")); !os.IsNotExist(err) {
		t.Error("failed install must not land the module dir")
	}
	if list, _ := RegLoad(); len(list) != 0 {
		t.Errorf("failed install must not register: %+v", list)
	}
	assertNoStagingLeftovers(t) // staging cleaned on failure
}

func TestRemove(t *testing.T) {
	t.Run("rejects unsafe names before touching anything", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		for _, name := range []string{"..", "../evil", `evil\..`, "a/b", "con"} {
			if err := Remove(name); err == nil {
				t.Errorf("Remove(%q) = nil, want error", name)
			}
		}
	})
	t.Run("not installed", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		if err := Remove("ghost"); err == nil || !strings.Contains(err.Error(), "not installed") {
			t.Errorf("Remove(ghost) = %v, want not-installed error", err)
		}
	})
	t.Run("registered without a directory: entry pruned", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		if err := RegSave([]Entry{{Name: "gone", Enabled: true}}); err != nil {
			t.Fatal(err)
		}
		if err := Remove("gone"); err != nil {
			t.Fatalf("Remove = %v", err)
		}
		if list, _ := RegLoad(); len(list) != 0 {
			t.Errorf("registry = %+v, want empty", list)
		}
	})
	t.Run("removes files and entry, leaves others alone", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		writeModuleDir(t, "mod", minManifest("mod"))
		if err := RegSave([]Entry{{Name: "mod"}, {Name: "other"}}); err != nil {
			t.Fatal(err)
		}
		if err := Remove("mod"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(Dir(), "mod")); !os.IsNotExist(err) {
			t.Error("module dir still present")
		}
		list, _ := RegLoad()
		if len(list) != 1 || list[0].Name != "other" {
			t.Errorf("registry = %+v, want only the other entry", list)
		}
	})
	t.Run("unregistered dir removable", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		writeModuleDir(t, "orphan", minManifest("orphan"))
		if err := Remove("orphan"); err != nil {
			t.Fatalf("Remove = %v", err)
		}
		if _, err := os.Stat(filepath.Join(Dir(), "orphan")); !os.IsNotExist(err) {
			t.Error("orphan dir still present")
		}
	})
}

// TestRemovePartialFailureSurfaced pins two behaviors at once: the registry
// entry goes FIRST (nothing new can execute once deregistered) and a partial
// RemoveAll is an error, never silence — the remainder stays visible as an
// unregistered dir in ls/doctor.
func TestRemovePartialFailureSurfaced(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	writeModuleDir(t, "mod", minManifest("mod"))
	sub := filepath.Join(Dir(), "mod", "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RegSave([]Entry{{Name: "mod", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// A process's working directory cannot be deleted on Windows — the same
		// in-use failure mode as a running handler holding its script open.
		old, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(sub); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chdir(old) }()
	} else {
		if os.Geteuid() == 0 {
			t.Skip("running as root: permission bits cannot make RemoveAll fail")
		}
		// A write-protected subdir makes unlinking its contents fail.
		if err := os.WriteFile(filepath.Join(sub, "f"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(sub, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(sub, 0o700)
	}
	err := Remove("mod")
	if err == nil || !strings.Contains(err.Error(), "could not be fully removed") {
		t.Fatalf("Remove = %v, want the partial removal surfaced", err)
	}
	if list, _ := RegLoad(); len(list) != 0 {
		t.Errorf("registry = %+v, want the entry removed before RemoveAll", list)
	}
}
