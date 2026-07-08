package tui

// Pre-launch hooks: modules with a preLaunch handler run interactively, in
// lexicographic order, BEFORE claude gets the terminal. Each hook's exit code
// is its verdict — 0 continues the chain, a nonzero EXIT cancels the launch
// with a footer notice. tea.ExecProcess runs exactly one command, so the
// chain is message-driven: each hook's callback yields hookDoneMsg, whose
// handler starts the next hook or, after the last one, the pending claude
// command. Fail-open by policy, on BOTH failure shapes: a hook that cannot be
// built (resolveArgv) or started (missing binary — LookPath surfaces at Start
// as *exec.Error, not *exec.ExitError) is skipped with a warning. Only a
// hook that actually ran and exited nonzero cancels — a broken module must
// never brick launching.
//
// Chain identity: bubbletea restores the terminal (and its input reader)
// BEFORE the ExecProcess callback message is delivered, so a keypress
// buffered while a hook owned the terminal can reach Update ahead of
// hookDoneMsg. Two guards close that window: launch entry points refuse to
// start while a chain is in flight, and every hookDoneMsg carries the chain
// generation so a stale verdict can never drive (or cancel) a newer chain —
// the xformGen precedent.

import (
	"errors"
	"os"
	"os/exec"
	"strconv"

	tea "github.com/charmbracelet/bubbletea"

	"houston/internal/module"
)

// pendingLaunch is the launch parked while its pre-launch hooks run.
type pendingLaunch struct {
	gen      int
	claude   *exec.Cmd
	hooks    []module.Module // still to run, lexicographic order
	payload  module.PreLaunchPayload
	onLaunch func() // runs right before claude dispatches (e.g. TouchUse); may be nil
}

// hookDoneMsg reports one finished pre-launch hook of one specific chain.
type hookDoneMsg struct {
	gen int
	mod string
	err error // *exec.ExitError = veto; any other error = skip (fail-open)
}

// launchBusy reports (and footers) a chain already in flight — the guard that
// keeps a buffered keypress from parking a second launch over it.
func (m *Model) launchBusy() bool {
	if m.pending == nil {
		return false
	}
	m.status = "launch already in progress (pre-launch hooks running)"
	return true
}

// launchWithHooks starts claude, running any pre-launch hooks first. onLaunch
// (optional) runs only when claude actually dispatches — after every hook had
// its say. The no-hooks path is the previous direct ExecProcess.
func (m Model) launchWithHooks(payload module.PreLaunchPayload, claude *exec.Cmd, onLaunch func()) (tea.Model, tea.Cmd) {
	hooks := module.PreLaunchMods(m.mods)
	if len(hooks) == 0 {
		if onLaunch != nil {
			onLaunch()
		}
		return m, tea.ExecProcess(claude, func(err error) tea.Msg { return execDoneMsg{err} })
	}
	if payload.Cwd == "" {
		// The account-launch path starts claude in the process cwd.
		payload.Cwd, _ = os.Getwd()
	}
	m.launchGen++
	m.pending = &pendingLaunch{gen: m.launchGen, claude: claude, hooks: hooks, payload: payload, onLaunch: onLaunch}
	return m.runNextHook()
}

// runNextHook pops and executes the next hook; unbuildable hooks are skipped
// (fail-open) until one runs or the chain drains into the claude launch.
func (m Model) runNextHook() (tea.Model, tea.Cmd) {
	for len(m.pending.hooks) > 0 {
		mod := m.pending.hooks[0]
		m.pending.hooks = m.pending.hooks[1:]
		env := module.NewEnvelope(module.EventPreLaunch, mod, m.pending.payload)
		cmd, cleanup, err := module.ExecPreLaunch(mod, env)
		if err != nil {
			module.LogEvent(mod.Name, module.EventPreLaunch, err.Error(), nil)
			m.status = "[" + mod.Name + "] preLaunch skipped: " + err.Error()
			continue
		}
		name, gen := mod.Name, m.pending.gen
		return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
			cleanup()
			return hookDoneMsg{gen: gen, mod: name, err: err}
		})
	}
	pending := m.pending
	m.pending = nil
	if pending.onLaunch != nil {
		pending.onLaunch()
	}
	return m, tea.ExecProcess(pending.claude, func(err error) tea.Msg { return execDoneMsg{err} })
}

// onHookDone continues or cancels the parked launch. A vanished pending
// (quit raced the callback) or a stale generation (this verdict belongs to an
// abandoned chain) is a no-op — one chain's verdict must never drive another.
func (m Model) onHookDone(msg hookDoneMsg) (tea.Model, tea.Cmd) {
	if m.pending == nil || m.pending.gen != msg.gen {
		return m, nil
	}
	if msg.err != nil {
		var ee *exec.ExitError
		if errors.As(msg.err, &ee) {
			// The hook ran and voted to cancel: its contract, not a failure.
			m.pending = nil
			m.status = "[" + msg.mod + "] launch cancelled (exit " + strconv.Itoa(ee.ExitCode()) + ")"
			return m, nil
		}
		// Start failure (missing binary and kin): fail-open, like the CLI
		// path and the build-failure branch above — skip and keep going.
		module.LogEvent(msg.mod, module.EventPreLaunch, "interactive: "+msg.err.Error(), nil)
		m.status = "[" + msg.mod + "] preLaunch skipped: " + msg.err.Error()
	}
	return m.runNextHook()
}
