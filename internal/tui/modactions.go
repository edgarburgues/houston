package tui

// Module TUI actions: key routing, load-time conflict enforcement and the
// help-footer entries. Built-ins win by construction — a module action whose
// key appears in a builtin table is dropped when the model is built, so the
// lookup after each key switch can never fire for a built-in key and the
// footer never advertises a dead binding.

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"houston/internal/model"
	"houston/internal/module"
)

// BuiltinMissionsKeys and BuiltinAccountsKeys mirror the case sets of
// updateKeys and updateAccountsKeys, derived from the command registry so
// the switches, the conflict tables and the help overlay share one source.
// Conditionally-consumed keys ("x" only acts inside a program view) stay
// reserved: partial consumption must not make a binding intermittently
// module-owned. A drift test asserts each table equals its switch's cases,
// and `module ls`/doctor consult them to flag shadowed keys.
var (
	BuiltinMissionsKeys = builtinKeyTable(scrMissions)
	BuiltinAccountsKeys = builtinKeyTable(scrAccounts)
)

// moduleActionRef pairs an action with its owning module for dispatch.
type moduleActionRef struct {
	mod module.Module
	act module.Action
}

// modActionMsg reports a module action's outcome back to Update.
type modActionMsg struct {
	mod, id, status string
	refresh         bool
	err             error
}

// buildModActions filters the enabled modules' actions into the runtime key
// map ("missions:J" → ref). mods arrive in lexicographic name order, so the
// first claimant of a key wins; built-in keys always win. Dropped actions
// come back as footer-ready warnings, surviving ones in deterministic order
// for the help footers.
func buildModActions(mods []module.Module) (map[string]moduleActionRef, []moduleActionRef, []string) {
	refs := map[string]moduleActionRef{}
	var accepted []moduleActionRef
	var warns []string
	for _, mod := range mods {
		for _, a := range mod.Manifest.Actions {
			builtin := BuiltinMissionsKeys
			if a.Screen == "accounts" {
				builtin = BuiltinAccountsKeys
			}
			if builtin[a.Key] {
				warns = append(warns, fmt.Sprintf("[%s] action %s dropped: %q is a built-in %s key", mod.Name, a.ID, a.Key, a.Screen))
				continue
			}
			id := a.Screen + ":" + a.Key
			if prev, taken := refs[id]; taken {
				warns = append(warns, fmt.Sprintf("[%s] action %s dropped: key %q already claimed by module %s", mod.Name, a.ID, a.Key, prev.mod.Name))
				continue
			}
			ref := moduleActionRef{mod: mod, act: a}
			refs[id] = ref
			accepted = append(accepted, ref)
		}
	}
	return refs, accepted, warns
}

// keyLabel renders a binding for the help footer: ctrl+j → ^J.
func keyLabel(k string) string {
	if rest, ok := strings.CutPrefix(k, "ctrl+"); ok {
		return "^" + strings.ToUpper(rest)
	}
	return k
}

// runModuleAction dispatches one module action for the current selection
// (silent no-op without one, like the built-ins). Non-interactive runs go
// through RunAction — the full Invoke hardening — inside a tea.Cmd goroutine;
// interactive ones get a PLAIN exec.Cmd so tea.ExecProcess hands them the
// real terminal, with the envelope in a store tmp file the callback cleans
// up (SweepTmp is the crash backstop).
func (m Model) runModuleAction(ref moduleActionRef) (tea.Model, tea.Cmd) {
	if m.launchBusy() {
		// A buffered keypress must not queue another ExecProcess over a
		// pre-launch chain in flight.
		return m, nil
	}
	payload := module.ActionPayload{Screen: ref.act.Screen, Action: ref.act.ID}
	if ref.act.Screen == "accounts" {
		a, ok := m.curAccount()
		if !ok {
			return m, nil
		}
		row := module.AccountRowOf(a)
		payload.Account = &row
	} else {
		ms, ok := m.selected()
		if !ok {
			return m, nil
		}
		row := module.ProjectRows([]model.Mission{ms}, m.st)[0]
		payload.Mission = &row
	}
	env := module.NewEnvelope(module.EventAction, ref.mod, payload)
	m.status = "[" + ref.mod.Name + "] " + ref.act.ID + "…"
	if !ref.act.Interactive {
		return m, runActionCmd(m.modCtx, ref.mod, ref.act, env)
	}
	cmd, cleanup, err := module.ExecAction(ref.mod, ref.act, env)
	if err != nil {
		module.LogEvent(ref.mod.Name, module.EventAction, err.Error(), nil)
		m.status = actionFailStatus(ref.mod.Name, ref.act.ID, err)
		return m, nil
	}
	name, id, refresh := ref.mod.Name, ref.act.ID, ref.act.RefreshAfter
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		cleanup()
		if err != nil {
			// Interactive runs bypass Invoke, so log here to keep the
			// footer's log pointer truthful.
			module.LogEvent(name, module.EventAction, "interactive: "+err.Error(), nil)
			return modActionMsg{mod: name, id: id, err: err}
		}
		return modActionMsg{mod: name, id: id, refresh: refresh}
	})
}

// runActionCmd runs a non-interactive action off the event loop. Like every
// module-spawned goroutine it recovers: a bug reachable through a module
// path must never crash the TUI. ctx is the model's root module context, so
// the quit-time cancel reaches an in-flight handler.
func runActionCmd(ctx context.Context, mod module.Module, act module.Action, env module.Envelope) tea.Cmd {
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				msg = modActionMsg{mod: mod.Name, id: act.ID, err: fmt.Errorf("panic: %v", r)}
			}
		}()
		rep, err := module.RunAction(ctx, mod, act, env)
		if err != nil {
			return modActionMsg{mod: mod.Name, id: act.ID, err: err}
		}
		return modActionMsg{mod: mod.Name, id: act.ID, status: rep.Status, refresh: rep.Refresh}
	}
}

// actionFailStatus is the footer line for a failed explicit invocation —
// always shown, per the failure-policy table.
func actionFailStatus(mod, id string, err error) string {
	return fmt.Sprintf("[%s] %s: %v (see houston module log)", mod, id, err)
}
