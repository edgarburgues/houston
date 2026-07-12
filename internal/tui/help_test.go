package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestHelpOverlayToggleAndWhichKey(t *testing.T) {
	m := newModel(t)
	m = drive(m, runes("?"))
	if !m.helpOpen {
		t.Fatal("? must open the overlay")
	}
	m = drive(m, key(tea.KeyEsc))
	if m.helpOpen {
		t.Fatal("esc must close the overlay")
	}
	// Which-key: a command key pressed from the overlay closes it AND runs
	// its binding underneath.
	target := m.mid[m.midCur].Key()
	m = drive(m, runes("?"), runes("*"))
	if m.helpOpen {
		t.Fatal("a command key must close the overlay")
	}
	if !m.st.MetaOf(target).Pinned {
		t.Fatal("the redispatched key must run its binding")
	}
}

func TestHelpOverlayScrollClamps(t *testing.T) {
	m := newModel(t)
	// Narrow forces one column; short forces scrolling.
	m = drive(m, tea.WindowSizeMsg{Width: 60, Height: 12}, runes("?"))
	lines, winH := m.helpLines()
	if len(lines) <= winH {
		t.Fatalf("scenario should overflow: %d lines in a %d window", len(lines), winH)
	}
	max := len(lines) - winH
	m = drive(m, runes("G"))
	if m.helpScroll != max {
		t.Errorf("G should land on the last window, got %d want %d", m.helpScroll, max)
	}
	m = drive(m, runes("j"))
	if m.helpScroll != max {
		t.Errorf("scrolling past the end must clamp, got %d", m.helpScroll)
	}
	m = drive(m, runes("g"))
	if m.helpScroll != 0 {
		t.Errorf("g should jump to the top, got %d", m.helpScroll)
	}
	m = drive(m, runes("k"))
	if m.helpScroll != 0 {
		t.Errorf("scrolling above the top must clamp, got %d", m.helpScroll)
	}
	if !strings.Contains(m.View(), "j/k scroll") {
		t.Errorf("an overflowing overlay should show the scroll position line")
	}
}

func TestHelpOverlayPerScreen(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir()) // the accounts screen loads the store
	m := newModel(t)
	view := drive(m, runes("?")).View()
	for _, want := range []string{"Help — Missions", "resume", "remove from program"} {
		if !strings.Contains(view, want) {
			t.Errorf("missions overlay should carry %q", want)
		}
	}
	view = drive(m, runes("A"), runes("?")).View()
	for _, want := range []string{"Help — Accounts", "launch session", "remove account"} {
		if !strings.Contains(view, want) {
			t.Errorf("accounts overlay should carry %q", want)
		}
	}
	if strings.Contains(view, "remove from program") {
		t.Errorf("missions commands must not leak into the accounts overlay")
	}
}
