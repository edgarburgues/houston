package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"houston/internal/module"
)

func TestPaletteOpenFilterRun(t *testing.T) {
	m := newModel(t)
	target := m.mid[m.midCur].Key()
	m = drive(m, runes(":"))
	if !m.palOpen {
		t.Fatal(": must open the palette")
	}
	// Typing filters; the top match for "pin" is the pin command.
	m = drive(m, runes("p"), runes("i"), runes("n"))
	matches := m.palMatches()
	if len(matches) == 0 || !strings.Contains(matches[0].title, "pin") {
		t.Fatalf("top match for 'pin' wrong: %+v", matches)
	}
	m = drive(m, key(tea.KeyEnter))
	if m.palOpen {
		t.Fatal("enter must close the palette")
	}
	if !m.st.MetaOf(target).Pinned {
		t.Fatal("running the entry must execute the binding")
	}
}

func TestPaletteTabJumpAndScoping(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := newModelMods(t, tabModule("jira"))
	m = drive(m, key(tea.KeyCtrlP))
	if !m.palOpen {
		t.Fatal("ctrl+p must open the palette")
	}
	for _, r := range "tab iss" {
		m = drive(m, runes(string(r)))
	}
	matches := m.palMatches()
	if len(matches) == 0 || matches[0].tab != 3 {
		t.Fatalf("top match for 'tab iss' should be the jira tab entry: %+v", matches)
	}
	m = drive(m, key(tea.KeyEnter))
	if m.tabCur != 3 || m.screen != screenModuleView {
		t.Fatalf("running a tab entry must switch: tab=%d screen=%v", m.tabCur, m.screen)
	}
	// Scoping: the accounts palette must not offer missions commands.
	m = drive(m, runes("1"), runes("2"), runes(":"))
	for _, it := range m.palMatches() {
		if it.title == "export transcript" {
			t.Fatal("missions commands must not leak into the accounts palette")
		}
	}
	// esc closes without running anything.
	m = drive(m, key(tea.KeyEsc))
	if m.palOpen {
		t.Fatal("esc must close the palette")
	}
}

func TestKeyMsgForRoundTrips(t *testing.T) {
	for _, k := range []string{"enter", "esc", "tab", "backspace", "up", "down", "left", "right", "pgup", "pgdown", "x", "P", "*", "/", "?", "[", " ", "ctrl+j"} {
		if got := keyMsgFor(k).String(); got != k {
			t.Errorf("keyMsgFor(%q).String() = %q", k, got)
		}
	}
}

func TestFuzzyScoreRanks(t *testing.T) {
	if _, ok := fuzzyScore("xyz", "archive"); ok {
		t.Fatal("non-subsequence must not match")
	}
	early, _ := fuzzyScore("pin", "pin / unpin")
	late, _ := fuzzyScore("pin", "add to program indeed")
	if early >= late {
		t.Fatalf("earlier tighter match must rank higher: %d vs %d", early, late)
	}
	if _, ok := fuzzyScore("", "anything"); !ok {
		t.Fatal("empty query matches everything")
	}
}

func TestPalettePageActionsAndSelectionStability(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	mod := rowsModule()
	mod.Manifest.Views[0].Tab = true
	m := newModelMods(t, mod)

	// The promoted view shows once (its tab entry), not twice.
	seen := 0
	for _, it := range m.paletteItems() {
		if strings.Contains(it.title, "Issues") {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("a promoted view must appear once in the palette, got %d entries", seen)
	}

	// On the view's page, its actions are findable and runnable.
	tm, _ := m.Update(runes("I"))
	m = tm.(Model)
	st := m.mvStates[viewKey(m.mvRef)]
	tm, _ = m.Update(modViewMsg{gen: st.gen, mod: "jira", id: "list", title: "Issues (1)", rows: []module.ViewRow{{ID: "A-1", Text: "A-1 alpha"}}})
	m = tm.(Model)
	m = drive(m, runes(":"))
	for _, r := range "open" {
		m = drive(m, runes(string(r)))
	}
	matches := m.palMatches()
	if len(matches) == 0 || matches[0].title != "open" {
		t.Fatalf("the view's page action must be findable: %+v", matches)
	}
	tm, cmd := m.Update(key(tea.KeyEnter))
	m = tm.(Model)
	if cmd == nil || !strings.Contains(m.status, "[jira] open") {
		t.Fatalf("running the palette entry must dispatch the page action: %q", m.status)
	}

	// Cursor movement inside the input must not reset the selection.
	m = drive(m, runes(":"), key(tea.KeyDown), key(tea.KeyDown))
	if m.palSel != 2 {
		t.Fatalf("precondition: palSel=%d", m.palSel)
	}
	m = drive(m, key(tea.KeyLeft))
	if m.palSel != 2 {
		t.Fatalf("a non-mutating input key must keep the selection, got %d", m.palSel)
	}
	m = drive(m, runes("x"))
	if m.palSel != 0 {
		t.Fatalf("a value change must snap the selection back, got %d", m.palSel)
	}
}
