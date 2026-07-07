package tui

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"houston/internal/model"
	"houston/internal/module"
	"houston/internal/store"
)

// switchCaseStrings extracts every string literal used as a case expression
// inside the named function in app.go — the ground truth the builtin key
// tables must mirror.
func switchCaseStrings(t *testing.T, fn string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "app.go", nil, 0)
	if err != nil {
		t.Fatalf("parse app.go: %v", err)
	}
	out := map[string]bool{}
	found := false
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn {
			continue
		}
		found = true
		ast.Inspect(fd, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
					s, err := strconv.Unquote(bl.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", bl.Value, err)
					}
					out[s] = true
				}
			}
			return true
		})
	}
	if !found {
		t.Fatalf("function %s not found in app.go", fn)
	}
	return out
}

// TestBuiltinKeyTablesMatchSwitches keeps the exported tables and the key
// switches from drifting apart: a case added without its table entry would
// let a module claim a built-in key, and a stale table entry would reserve a
// key no built-in uses.
func TestBuiltinKeyTablesMatchSwitches(t *testing.T) {
	tests := []struct {
		fn    string
		table map[string]bool
	}{
		{"updateKeys", BuiltinMissionsKeys},
		{"updateAccountsKeys", BuiltinAccountsKeys},
	}
	for _, tt := range tests {
		t.Run(tt.fn, func(t *testing.T) {
			got := switchCaseStrings(t, tt.fn)
			for k := range got {
				if !tt.table[k] {
					t.Errorf("switch case %q missing from the builtin table", k)
				}
			}
			for k := range tt.table {
				if !got[k] {
					t.Errorf("table key %q has no switch case", k)
				}
			}
		})
	}
}

func modWith(name string, actions ...module.Action) module.Module {
	return module.Module{
		Entry:    module.Entry{Name: name, Enabled: true},
		Manifest: module.Manifest{API: 1, Name: name, Actions: actions},
	}
}

func newModelMods(t *testing.T, mods ...module.Module) Model {
	t.Helper()
	st, err := store.LoadFrom(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	m := New("root", func() ([]model.Mission, error) { return fakeMissions(), nil }, st, fakeMissions(), mods)
	return drive(m, tea.WindowSizeMsg{Width: 100, Height: 30})
}

func TestBuildModActionsConflicts(t *testing.T) {
	amod := modWith("aaa",
		module.Action{ID: "wins", Key: "J", Title: "wins", Screen: "missions"},
	)
	bmod := modWith("bbb",
		module.Action{ID: "dropped-builtin", Key: "r", Title: "clashes with reindex", Screen: "missions"},
		module.Action{ID: "dropped-reserved", Key: "x", Title: "clashes with remove", Screen: "missions"},
		module.Action{ID: "dropped-shadowed", Key: "J", Title: "loses to aaa", Screen: "missions"},
		module.Action{ID: "ok", Key: "J", Title: "distinct screen is fine", Screen: "accounts"},
	)
	refs, accepted, warns := buildModActions([]module.Module{amod, bmod})
	if len(refs) != 2 || len(accepted) != 2 {
		t.Fatalf("want 2 surviving actions, got refs=%d accepted=%d (warns=%v)", len(refs), len(accepted), warns)
	}
	if got := refs["missions:J"]; got.mod.Name != "aaa" || got.act.ID != "wins" {
		t.Errorf("missions:J should belong to aaa/wins, got %s/%s", got.mod.Name, got.act.ID)
	}
	if got := refs["accounts:J"]; got.mod.Name != "bbb" || got.act.ID != "ok" {
		t.Errorf("accounts:J should belong to bbb/ok, got %s/%s", got.mod.Name, got.act.ID)
	}
	if len(warns) != 3 {
		t.Fatalf("want 3 drop warnings, got %v", warns)
	}
	for _, w := range warns {
		if !strings.Contains(w, "[bbb]") || !strings.Contains(w, "dropped") {
			t.Errorf("warning should name the module and the drop: %q", w)
		}
	}
}

func TestModuleKeyRoutesAfterBuiltins(t *testing.T) {
	m := newModelMods(t, modWith("demo",
		module.Action{ID: "hello", Key: "J", Title: "say hello", Screen: "missions"},
	))
	tm, cmd := m.Update(runes("J"))
	m = tm.(Model)
	if cmd == nil {
		t.Fatalf("a bound module key must dispatch a command")
	}
	if !strings.Contains(m.status, "[demo] hello") {
		t.Errorf("status should announce the action, got %q", m.status)
	}
	// An unbound key stays a no-op.
	if _, cmd := m.Update(runes("Z")); cmd != nil {
		t.Errorf("an unbound key must not dispatch anything")
	}
}

func TestModuleKeyNeedsASelection(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir()) // accounts.Load must not see the real store
	m := newModelMods(t, modWith("demo",
		module.Action{ID: "acc", Key: "u", Title: "account thing", Screen: "accounts"},
	))
	m = drive(m, runes("A")) // accounts screen, zero accounts
	if _, cmd := m.Update(runes("u")); cmd != nil {
		t.Errorf("no selected account: the action must no-op like the built-ins")
	}
}

func TestModActionMsgStatusAndRefresh(t *testing.T) {
	calls := 0
	st, err := store.LoadFrom(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	m := New("root", func() ([]model.Mission, error) { calls++; return fakeMissions(), nil }, st, nil, nil)
	m = drive(m, tea.WindowSizeMsg{Width: 100, Height: 30})

	m = drive(m, modActionMsg{mod: "demo", id: "hello", status: "opened PROJ-1", refresh: true})
	if calls != 1 {
		t.Errorf("refresh should rescan exactly once, got %d", calls)
	}
	if len(m.missions) != 3 {
		t.Errorf("the rescan should replace the mission set, got %d", len(m.missions))
	}
	if m.status != "[demo] opened PROJ-1" {
		t.Errorf("the reply status should win the footer, got %q", m.status)
	}

	m = drive(m, modActionMsg{mod: "demo", id: "hello"})
	if m.status != "[demo] hello done" {
		t.Errorf("an empty reply is a valid generic done, got %q", m.status)
	}

	m = drive(m, modActionMsg{mod: "demo", id: "hello", err: errors.New("exit status 3")})
	if !strings.Contains(m.status, "[demo] hello: exit status 3 (see houston module log)") {
		t.Errorf("failures must point at the module log, got %q", m.status)
	}
	if calls != 1 {
		t.Errorf("an error must not trigger a rescan, got %d calls", calls)
	}
}

func TestHelpFooterAdvertisesModuleActions(t *testing.T) {
	m := newModelMods(t, modWith("demo",
		module.Action{ID: "open", Key: "J", Title: "open Jira ticket", Screen: "missions"},
		module.Action{ID: "log", Key: "ctrl+j", Title: "log time to Jira", Screen: "missions", Interactive: true},
		module.Action{ID: "probe", Key: "u", Title: "account usage", Screen: "accounts"},
		module.Action{ID: "dead", Key: "r", Title: "shadowed by reindex", Screen: "missions"},
	))
	if !strings.HasSuffix(m.helpMissions, " · J open Jira ticket · ^J log time to Jira") {
		t.Errorf("missions footer should list the surviving actions: %q", m.helpMissions)
	}
	if !strings.HasSuffix(m.helpAccounts, " · u account usage") {
		t.Errorf("accounts footer should list the surviving actions: %q", m.helpAccounts)
	}
	if strings.Contains(m.helpMissions, "shadowed by reindex") {
		t.Errorf("a dropped action must never be advertised: %q", m.helpMissions)
	}
	if !strings.Contains(m.status, "dead") {
		t.Errorf("startup status should warn about the dropped action: %q", m.status)
	}
}

func TestInteractiveActionBuildFailureHitsFooter(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir()) // envelope tmp file and module log stay off the real store
	m := newModelMods(t, modWith("demo",
		module.Action{ID: "broken", Key: "J", Title: "broken", Screen: "missions", Interactive: true},
	))
	// Empty command: ExecAction fails before any process spawns.
	tm, cmd := m.Update(runes("J"))
	m = tm.(Model)
	if cmd != nil {
		t.Fatalf("a failed ExecAction must not hand bubbletea a command")
	}
	if !strings.Contains(m.status, "[demo] broken:") || !strings.Contains(m.status, "(see houston module log)") {
		t.Errorf("the failure must reach the footer with the log pointer, got %q", m.status)
	}
}
