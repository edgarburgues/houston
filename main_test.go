package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"houston/internal/accounts"
	"houston/internal/module"
)

// The envelope's houston.version and HOUSTON_VERSION must always match what
// `houston version` prints — init() stamps the module package's copy, and
// this pins the wiring (a release build only differs by the ldflags value).
func TestModuleVersionStamped(t *testing.T) {
	if module.HoustonVersion != version {
		t.Fatalf("module.HoustonVersion = %q, want %q", module.HoustonVersion, version)
	}
}

// installHookModule writes a registered, enabled module whose preLaunch hook
// is a real shell one-liner exiting with the given code.
func installHookModule(t *testing.T, name string, exitCode int) {
	t.Helper()
	dir := filepath.Join(module.Dir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var argv []string
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/d", "/c", "exit", itoaTest(exitCode)}
	} else {
		argv = []string{"sh", "-c", "exit " + itoaTest(exitCode)}
	}
	man := map[string]any{
		"api": 1, "name": name,
		"preLaunch": map[string]any{"command": argv},
	}
	b, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(dir, "module.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := module.RegLoad()
	if err != nil {
		t.Fatal(err)
	}
	list = append(list, module.Entry{Name: name, Enabled: true, AddedAt: accounts.Now()})
	if err := module.RegSave(list); err != nil {
		t.Fatal(err)
	}
}

func itoaTest(n int) string { return string(rune('0' + n)) }

func TestRunPreLaunchHooks(t *testing.T) {
	acc := accounts.Account{ID: "t1"}

	t.Run("no hooks proceeds", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		if !runPreLaunchHooks("run", acc) {
			t.Fatal("no hooks must proceed")
		}
	})

	t.Run("exit 0 proceeds", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		installHookModule(t, "ok-hook", 0)
		if !runPreLaunchHooks("run", acc) {
			t.Fatal("clean hook must proceed")
		}
	})

	t.Run("nonzero exit cancels", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		installHookModule(t, "gate", 3)
		if runPreLaunchHooks("run", acc) {
			t.Fatal("nonzero exit must cancel the launch")
		}
	})

	t.Run("earlier clean hook then veto cancels", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		installHookModule(t, "a-ok", 0)
		installHookModule(t, "b-gate", 2)
		if runPreLaunchHooks("run", acc) {
			t.Fatal("any veto in the chain must cancel")
		}
	})

	t.Run("unbuildable hook fails open", func(t *testing.T) {
		t.Setenv("HOUSTON_HOME", t.TempDir())
		dir := filepath.Join(module.Dir(), "broken")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		man := `{"api":1,"name":"broken","preLaunch":{"command":["missing-binary-hopefully-not-on-path-xyz"]}}`
		if err := os.WriteFile(filepath.Join(dir, "module.json"), []byte(man), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := module.RegSave([]module.Entry{{Name: "broken", Enabled: true, AddedAt: accounts.Now()}}); err != nil {
			t.Fatal(err)
		}
		if !runPreLaunchHooks("run", acc) {
			t.Fatal("a hook that cannot start must never brick the launch")
		}
	})
}
