package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
