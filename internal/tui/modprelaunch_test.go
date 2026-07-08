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

// exitErr manufactures a real *exec.ExitError with the given (single-digit)
// code — the shape of a hook that ran and voted.
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

// startErr manufactures the missing-binary failure shape (*exec.Error via
// LookPath, surfaced at Start) — the case that must fail OPEN.
func startErr(t *testing.T) error {
	t.Helper()
	err := exec.Command("missing-binary-hopefully-not-on-path-xyz").Run()
	if err == nil {
		t.Fatal("expected start failure")
	}
	if strings.Contains(err.Error(), "exit status") {
		t.Fatalf("wanted a start failure, got an exit: %v", err)
	}
	return err
}

func TestLaunchWithHooksChainsAndLaunches(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := Model{mods: []module.Module{hookModule("a", []string{"cmd"}), hookModule("b", []string{"cmd"})}}
	claude := exec.Command("claude")

	launched := false
	got, cmd := m.launchWithHooks(module.PreLaunchPayload{Source: "resume", Cwd: "x"}, claude,
		func() { launched = true })
	mm := got.(Model)
	if cmd == nil {
		t.Fatal("first hook must yield an ExecProcess command")
	}
	if mm.pending == nil || len(mm.pending.hooks) != 1 || mm.pending.hooks[0].Name != "b" {
		t.Fatalf("after first dispatch: %+v", mm.pending)
	}
	if launched {
		t.Fatal("onLaunch must wait for the hooks")
	}
	gen := mm.pending.gen

	// First hook succeeds: the second is dispatched.
	got, cmd = mm.onHookDone(hookDoneMsg{gen: gen, mod: "a"})
	mm = got.(Model)
	if cmd == nil || mm.pending == nil || len(mm.pending.hooks) != 0 {
		t.Fatalf("after second dispatch: %+v", mm.pending)
	}

	// Second succeeds: onLaunch fires, claude dispatches, pending clears.
	got, cmd = mm.onHookDone(hookDoneMsg{gen: gen, mod: "b"})
	mm = got.(Model)
	if cmd == nil {
		t.Fatal("claude launch command missing")
	}
	if mm.pending != nil {
		t.Fatal("pending must clear once claude is dispatched")
	}
	if !launched {
		t.Fatal("onLaunch must fire when claude dispatches")
	}
}

func TestLaunchWithHooksVeto(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := Model{mods: []module.Module{hookModule("gate", []string{"cmd"})}}
	launched := false
	got, _ := m.launchWithHooks(module.PreLaunchPayload{Source: "run", Cwd: "x"}, exec.Command("claude"),
		func() { launched = true })
	mm := got.(Model)

	got, cmd := mm.onHookDone(hookDoneMsg{gen: mm.pending.gen, mod: "gate", err: exitErr(t, 2)})
	mm = got.(Model)
	if cmd != nil {
		t.Fatal("a veto must not launch anything")
	}
	if mm.pending != nil {
		t.Fatal("pending must clear on veto")
	}
	if launched {
		t.Fatal("a vetoed launch must not stamp usage")
	}
	if !strings.Contains(mm.status, "launch cancelled") || !strings.Contains(mm.status, "gate") {
		t.Fatalf("status: %q", mm.status)
	}
}

func TestLaunchWithHooksFailOpenOnStartFailure(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	// The blocker shape: the hook could not START (missing binary). The chain
	// must keep draining into the claude launch — never fail closed.
	m := Model{mods: []module.Module{hookModule("nopython", []string{"cmd"})}}
	got, _ := m.launchWithHooks(module.PreLaunchPayload{Source: "resume", Cwd: "x"}, exec.Command("claude"), nil)
	mm := got.(Model)

	got, cmd := mm.onHookDone(hookDoneMsg{gen: mm.pending.gen, mod: "nopython", err: startErr(t)})
	mm = got.(Model)
	if cmd == nil {
		t.Fatal("claude must still launch after a start failure (fail-open)")
	}
	if mm.pending != nil {
		t.Fatal("pending must clear after draining")
	}
	if !strings.Contains(mm.status, "preLaunch skipped") {
		t.Fatalf("status: %q", mm.status)
	}
}

func TestLaunchWithHooksFailOpenOnUnbuildable(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	// "../escape" fails resolveArgv at build time: the hook is skipped and
	// the chain drains straight into the claude launch (fail-open).
	m := Model{mods: []module.Module{hookModule("broken", []string{"../escape"})}}
	got, cmd := m.launchWithHooks(module.PreLaunchPayload{Source: "account", Cwd: "x"}, exec.Command("claude"), nil)
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

func TestStaleHookVerdictIsDropped(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := Model{mods: []module.Module{hookModule("gate", []string{"cmd"})}}
	got, _ := m.launchWithHooks(module.PreLaunchPayload{Source: "resume", Cwd: "x"}, exec.Command("claude"), nil)
	mm := got.(Model)
	cur := mm.pending.gen

	// A verdict from an older, abandoned chain must not cancel this one.
	got, cmd := mm.onHookDone(hookDoneMsg{gen: cur - 1, mod: "gate", err: exitErr(t, 2)})
	mm = got.(Model)
	if cmd != nil {
		t.Fatal("stale verdict must be a no-op")
	}
	if mm.pending == nil || mm.pending.gen != cur {
		t.Fatal("current chain must survive a stale verdict")
	}
}

func TestLaunchBusyGuard(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := Model{mods: []module.Module{hookModule("gate", []string{"cmd"})}}
	got, _ := m.launchWithHooks(module.PreLaunchPayload{Source: "resume", Cwd: "x"}, exec.Command("claude"), nil)
	mm := got.(Model)
	if !mm.launchBusy() {
		t.Fatal("a chain in flight must report busy")
	}
	if !strings.Contains(mm.status, "already in progress") {
		t.Fatalf("status: %q", mm.status)
	}
	mm.pending = nil
	if mm.launchBusy() {
		t.Fatal("no chain, not busy")
	}
}

func TestLaunchWithHooksNoHooksIsDirect(t *testing.T) {
	m := Model{mods: nil}
	launched := false
	got, cmd := m.launchWithHooks(module.PreLaunchPayload{Source: "resume"}, exec.Command("claude"),
		func() { launched = true })
	if cmd == nil || got.(Model).pending != nil {
		t.Fatal("no hooks must mean a direct launch")
	}
	if !launched {
		t.Fatal("onLaunch must fire on the direct path too")
	}
}

func TestOnHookDoneWithoutPendingIsNoop(t *testing.T) {
	m := Model{}
	got, cmd := m.onHookDone(hookDoneMsg{mod: "x"})
	if cmd != nil || got.(Model).pending != nil {
		t.Fatal("stray hookDoneMsg must be a no-op")
	}
}
