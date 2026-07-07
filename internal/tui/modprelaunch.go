package tui

// Pre-launch hooks: modules with a preLaunch handler run interactively, in
// lexicographic order, BEFORE claude gets the terminal. Each hook's exit code
// is its verdict — 0 continues the chain, anything else cancels the launch
// with a footer notice. tea.ExecProcess runs exactly one command, so the
// chain is message-driven: each hook's callback yields hookDoneMsg, whose
// handler starts the next hook or, after the last one, the pending claude
// command. Fail-open by policy: a hook that cannot even be BUILT is skipped
// with a warning — a broken module must never brick launching.

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
	claude  *exec.Cmd
	hooks   []module.Module // still to run, lexicographic order
	payload module.PreLaunchPayload
}

// hookDoneMsg reports one finished pre-launch hook.
type hookDoneMsg struct {
	mod string
	err error // non-nil = nonzero exit or start failure → cancel the launch
}

// launchWithHooks starts claude, running any pre-launch hooks first. The
// no-hooks path is exactly the previous direct ExecProcess.
func (m Model) launchWithHooks(payload module.PreLaunchPayload, claude *exec.Cmd) (tea.Model, tea.Cmd) {
	hooks := module.PreLaunchMods(m.mods)
	if len(hooks) == 0 {
		return m, tea.ExecProcess(claude, func(err error) tea.Msg { return execDoneMsg{err} })
	}
	if payload.Cwd == "" {
		// The account-launch path starts claude in the process cwd.
		payload.Cwd, _ = os.Getwd()
	}
	m.pending = &pendingLaunch{claude: claude, hooks: hooks, payload: payload}
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
		name := mod.Name
		return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
			cleanup()
			return hookDoneMsg{mod: name, err: err}
		})
	}
	claude := m.pending.claude
	m.pending = nil
	return m, tea.ExecProcess(claude, func(err error) tea.Msg { return execDoneMsg{err} })
}

// onHookDone continues or cancels the parked launch. A vanished pending
// (quit raced the callback) is a no-op.
func (m Model) onHookDone(msg hookDoneMsg) (tea.Model, tea.Cmd) {
	if m.pending == nil {
		return m, nil
	}
	if msg.err != nil {
		m.pending = nil
		var ee *exec.ExitError
		if errors.As(msg.err, &ee) {
			// The hook's contract, not a failure: it asked to cancel.
			m.status = "[" + msg.mod + "] launch cancelled (exit " + strconv.Itoa(ee.ExitCode()) + ")"
		} else {
			module.LogEvent(msg.mod, module.EventPreLaunch, "interactive: "+msg.err.Error(), nil)
			m.status = "[" + msg.mod + "] preLaunch failed: " + msg.err.Error()
		}
		return m, nil
	}
	return m.runNextHook()
}
