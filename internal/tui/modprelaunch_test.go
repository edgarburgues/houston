package tui

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"houston/internal/module"
)

func hookModule(name string, argv []string) module.Module {
	return module.Module{
		Entry: module.Entry{Name: name, Enabled: true},
		Manifest: module.Manifest{
			API: 1, Name: name,
			PreLaunch: &module.Handler{Command: argv},
		},
		Dir: ".",
	}
}

// exitErr manufactures a real *exec.ExitError with the given code.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit", string(rune('0'+code)))
	} else {
		cmd = exec.Command("sh", "-c", "exit "+string(rune('0'+code)))
	}
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected nonzero exit")
	}
	return err
}

func TestLaunchWithHooksChainsAndLaunches(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := Model{mods: []module.Module{hookModule("a", []string{"cmd"}), hookModule("b", []string{"cmd"})}}
	claude := exec.Command("claude")

	got, cmd := m.launchWithHooks(module.PreLaunchPayload{Source: "resume", Cwd: "x"}, claude)
	mm := got.(Model)
	if cmd == nil {
		t.Fatal("first hook must yield an ExecProcess command")
	}
	if mm.pending == nil || len(mm.pending.hooks) != 1 || mm.pending.hooks[0].Name != "b" {
		t.Fatalf("after first dispatch: %+v", mm.pending)
	}

	// First hook succeeds: the second is dispatched.
	got, cmd = mm.onHookDone(hookDoneMsg{mod: "a"})
	mm = got.(Model)
	if cmd == nil || mm.pending == nil || len(mm.pending.hooks) != 0 {
		t.Fatalf("after second dispatch: %+v", mm.pending)
	}

	// Second succeeds: the parked claude launches and pending clears.
	got, cmd = mm.onHookDone(hookDoneMsg{mod: "b"})
	mm = got.(Model)
	if cmd == nil {
		t.Fatal("claude launch command missing")
	}
	if mm.pending != nil {
		t.Fatal("pending must clear once claude is dispatched")
	}
}

func TestLaunchWithHooksVeto(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := Model{mods: []module.Module{hookModule("gate", []string{"cmd"})}}
	got, _ := m.launchWithHooks(module.PreLaunchPayload{Source: "run", Cwd: "x"}, exec.Command("claude"))
	mm := got.(Model)

	got, cmd := mm.onHookDone(hookDoneMsg{mod: "gate", err: exitErr(t, 2)})
	mm = got.(Model)
	if cmd != nil {
		t.Fatal("a veto must not launch anything")
	}
	if mm.pending != nil {
		t.Fatal("pending must clear on veto")
	}
	if !strings.Contains(mm.status, "launch cancelled") || !strings.Contains(mm.status, "gate") {
		t.Fatalf("status: %q", mm.status)
	}
}

func TestLaunchWithHooksFailOpenOnUnbuildable(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	// "../escape" fails resolveArgv at build time: the hook is skipped and
	// the chain drains straight into the claude launch (fail-open).
	m := Model{mods: []module.Module{hookModule("broken", []string{"../escape"})}}
	got, cmd := m.launchWithHooks(module.PreLaunchPayload{Source: "account", Cwd: "x"}, exec.Command("claude"))
	mm := got.(Model)
	if cmd == nil {
		t.Fatal("claude must still launch")
	}
	if mm.pending != nil {
		t.Fatal("pending must clear after draining")
	}
	if !strings.Contains(mm.status, "preLaunch skipped") {
		t.Fatalf("status: %q", mm.status)
	}
}

func TestLaunchWithHooksNoHooksIsDirect(t *testing.T) {
	m := Model{mods: nil}
	got, cmd := m.launchWithHooks(module.PreLaunchPayload{Source: "resume"}, exec.Command("claude"))
	if cmd == nil || got.(Model).pending != nil {
		t.Fatal("no hooks must mean a direct launch")
	}
}

func TestOnHookDoneWithoutPendingIsNoop(t *testing.T) {
	m := Model{}
	got, cmd := m.onHookDone(hookDoneMsg{mod: "x"})
	if cmd != nil || got.(Model).pending != nil {
		t.Fatal("stray hookDoneMsg must be a no-op")
	}
}
