package tui

import (
	"strings"
	"testing"

	"houston/internal/module"
)

// TestRegistryCommandsWellFormed asserts every core command carries the
// fields the overlay and the hints render, and that no key is bound twice
// on the same screen — a duplicate would advertise two meanings for one
// keypress.
func TestRegistryCommandsWellFormed(t *testing.T) {
	seen := map[string]string{}
	for _, c := range coreCommands() {
		if len(c.keys) == 0 || c.label == "" || c.title == "" || c.category == "" || c.screen == "" {
			t.Errorf("incomplete command: %+v", c)
		}
		if c.origin != originCore || c.module != "" {
			t.Errorf("core command with module origin fields: %+v", c)
		}
		for _, k := range c.keys {
			id := c.screen + ":" + k
			if prev, dup := seen[id]; dup {
				t.Errorf("key %q bound twice on %s (%q and %q)", k, c.screen, prev, c.label)
			}
			seen[id] = c.label
		}
	}
}

// TestModuleCommandsProjectRefs locks the module → registry projection: the
// screen mapping, the per-module category and the ctrl-key label.
func TestModuleCommandsProjectRefs(t *testing.T) {
	mod := module.Module{Entry: module.Entry{Name: "demo", Enabled: true},
		Manifest: module.Manifest{API: 1, Name: "demo"}}
	acts := []moduleActionRef{
		{mod: mod, act: module.Action{ID: "a", Key: "J", Title: "open ticket", Screen: "missions"}},
		{mod: mod, act: module.Action{ID: "b", Key: "ctrl+j", Title: "log time", Screen: "accounts"}},
	}
	views := []moduleViewRef{
		{mod: mod, view: module.View{ID: "v", Key: "I", Title: "issues page"}},
	}
	got := moduleCommands(acts, views)
	if len(got) != 3 {
		t.Fatalf("want 3 commands, got %d", len(got))
	}
	if got[0].screen != scrMissions || got[0].category != "demo" || got[0].origin != originModule {
		t.Errorf("action projection wrong: %+v", got[0])
	}
	if got[1].screen != scrAccounts || got[1].label != "^J" {
		t.Errorf("accounts/ctrl projection wrong: %+v", got[1])
	}
	if got[2].screen != scrMissions || got[2].title != "issues page" || got[2].module != "demo" {
		t.Errorf("view projection wrong: %+v", got[2])
	}
}

// TestHintForStaysCurated locks the footer one-liners the goldens and the
// ready status build on: hints are the curated subset, never the full list.
func TestHintForStaysCurated(t *testing.T) {
	reg := coreCommands()
	if got := hintFor(reg, scrMissions); got != "/ search · enter resume · ? help · : command palette" {
		t.Errorf("missions hint drifted: %q", got)
	}
	acc := hintFor(reg, scrAccounts)
	for _, want := range []string{"enter launch session", "d/x remove account", "? help", ": command palette"} {
		if !strings.Contains(acc, want) {
			t.Errorf("accounts hint should carry %q: %q", want, acc)
		}
	}
	// Hints must never outgrow a normal terminal again — that was the
	// original sin the overlay fixed.
	for _, scr := range []string{scrMissions, scrAccounts, scrModView} {
		if got := hintFor(reg, scr); len([]rune(got)) > 90 {
			t.Errorf("%s hint too long (%d runes): %q", scr, len([]rune(got)), got)
		}
	}
	mv := hintFor(reg, scrModView)
	for _, want := range []string{"r refresh", "esc back", "? help"} {
		if !strings.Contains(mv, want) {
			t.Errorf("module-view hint should carry %q: %q", want, mv)
		}
	}
	// Module commands never reach the hint line — the overlay owns them.
	mod := module.Module{Entry: module.Entry{Name: "demo"}, Manifest: module.Manifest{API: 1, Name: "demo"}}
	withMod := append(reg, moduleCommands([]moduleActionRef{
		{mod: mod, act: module.Action{ID: "a", Key: "J", Title: "open ticket", Screen: "missions"}},
	}, nil)...)
	if got := hintFor(withMod, scrMissions); strings.Contains(got, "open ticket") {
		t.Errorf("module commands must not join the footer hint: %q", got)
	}
}
