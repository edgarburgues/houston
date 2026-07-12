package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"houston/internal/module"
)

func tabModule(name string) module.Module {
	return module.Module{
		Entry: module.Entry{Name: name, Enabled: true},
		Manifest: module.Manifest{
			API: 1, Name: name,
			Views: []module.View{{ID: "page", Key: "I", Title: "Issues", Command: []string{"cmd"}, Tab: true}},
		},
		Dir: ".",
	}
}

func TestTabsBuildAndSwitch(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := newModelMods(t, tabModule("jira"))
	if len(m.tabs) != 3 {
		t.Fatalf("want Missions+Accounts+jira, got %d tabs", len(m.tabs))
	}
	if m.tabCur != 0 || m.screen != screenMissions {
		t.Fatal("the TUI must start on the Missions tab")
	}

	// Digit jumps to the module tab and dispatches its first (and only) fetch.
	tm, cmd := m.Update(runes("3"))
	m = tm.(Model)
	if m.tabCur != 2 || m.screen != screenModuleView || cmd == nil {
		t.Fatalf("digit switch failed: tab=%d screen=%v cmd=%v", m.tabCur, m.screen, cmd)
	}
	ref := m.tabs[2].ref

	// The render lands while we hop away — content is stored, screen is not
	// hijacked.
	m = drive(m, runes("1"))
	if m.tabCur != 0 || m.screen != screenMissions {
		t.Fatal("digit 1 must return to missions")
	}
	st := m.mvStates[viewKey(ref)]
	m = drive(m, modViewMsg{gen: st.gen, mod: "jira", id: "page", title: "Issues (17)", body: "SOP-1"})
	if m.screen != screenMissions {
		t.Fatal("a background render must not steal the screen")
	}

	// Returning reuses the retained render without a new fetch, and the tab
	// strip shows the live title.
	tm, cmd = m.Update(runes("3"))
	m = tm.(Model)
	if cmd != nil {
		t.Fatal("a loaded tab must not re-fetch on activation")
	}
	if !strings.Contains(m.View(), "Issues (17)") {
		t.Fatal("the tab strip should carry the rendered title")
	}

	// Brackets cycle with wraparound.
	m = drive(m, runes("]"))
	if m.tabCur != 0 {
		t.Fatalf("] from the last tab should wrap to 0, got %d", m.tabCur)
	}
	m = drive(m, runes("["))
	if m.tabCur != 2 {
		t.Fatalf("[ from the first tab should wrap to the last, got %d", m.tabCur)
	}

	// esc from a tab view goes home to Missions.
	m = drive(m, key(tea.KeyEsc))
	if m.tabCur != 0 || m.screen != screenMissions {
		t.Fatal("esc from a tab view must land on the Missions tab")
	}

	// A digit with no tab behind it is a consumed no-op.
	tm, cmd = m.Update(runes("9"))
	m = tm.(Model)
	if cmd != nil || m.tabCur != 0 {
		t.Fatal("an unmapped digit must be a reserved no-op")
	}
}

func TestPromotedViewKeyJumpsToTab(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := newModelMods(t, tabModule("jira"))
	tm, cmd := m.Update(runes("I"))
	m = tm.(Model)
	if m.tabCur != 2 || m.screen != screenModuleView {
		t.Fatalf("the view key must jump to its tab: tab=%d screen=%v", m.tabCur, m.screen)
	}
	if cmd == nil {
		t.Fatal("first activation must fetch")
	}
}

func TestAccountsTabStateRetained(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := newModel(t)
	// First activation loads and probes (probe cmd only when accounts exist —
	// the temp store has none, so no cmd, but accsSeen must arm).
	m = drive(m, runes("2"))
	if m.screen != screenAccounts || !m.accsSeen {
		t.Fatalf("first activation must load: screen=%v seen=%v", m.screen, m.accsSeen)
	}
	// Arm the two-step delete, switch tabs, come back: it must be disarmed.
	m.pendingDelete = "acct"
	m = drive(m, runes("1"), runes("2"))
	if m.pendingDelete != "" {
		t.Fatal("switching tabs must disarm the pending delete")
	}
}

func TestTabBarRendersOnCoreScreens(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := newModel(t)
	view := m.View()
	for _, want := range []string{"1 Missions", "2 Accounts", "3 missions · 0 programs"} {
		if !strings.Contains(view, want) {
			t.Errorf("missions frame should carry %q", want)
		}
	}
	view = drive(m, runes("2")).View()
	if !strings.Contains(view, "0 accounts") {
		t.Errorf("accounts frame should carry the account count")
	}
}
