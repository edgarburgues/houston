package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"houston/internal/module"
)

func viewModule(name, key string) module.Module {
	return module.Module{
		Entry: module.Entry{Name: name, Enabled: true},
		Manifest: module.Manifest{
			API: 1, Name: name,
			Views: []module.View{{ID: "page", Key: key, Title: "Page", Command: []string{"cmd"}}},
		},
		Dir: ".",
	}
}

func TestBuildModViewsConflicts(t *testing.T) {
	mods := []module.Module{
		viewModule("a-mod", "e"), // builtin missions key: dropped
		viewModule("b-mod", "J"), // action-claimed: dropped
		viewModule("c-mod", "I"), // fine
		viewModule("d-mod", "I"), // cross-module dup: dropped
	}
	refs, accepted, warns := buildModViews(mods, map[string]bool{"J": true})
	if len(accepted) != 1 || accepted[0].mod.Name != "c-mod" {
		t.Fatalf("accepted: %+v", accepted)
	}
	if _, ok := refs["missions:I"]; !ok {
		t.Fatal("winning view missing from refs")
	}
	if len(warns) != 3 {
		t.Fatalf("warns: %v", warns)
	}
}

func TestModuleViewLifecycle(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := Model{}
	m.modCtx, m.modCancel = context.WithCancel(context.Background())
	ref := moduleViewRef{mod: viewModule("jira", "I"), view: module.View{ID: "page", Key: "I", Title: "Issues", Command: []string{"cmd"}}}

	got, cmd := m.openModuleView(ref)
	mm := got.(Model)
	if mm.screen != screenModuleView || cmd == nil {
		t.Fatal("open must switch screen and dispatch a fetch")
	}
	gen := mm.mvGen

	// Stale render (older gen) must be dropped.
	got, _ = mm.onModViewMsg(modViewMsg{gen: gen - 1, title: "old", body: "old"})
	mm = got.(Model)
	if mm.mvTitle == "old" {
		t.Fatal("stale render applied")
	}

	// Current render lands.
	got, _ = mm.onModViewMsg(modViewMsg{gen: gen, title: "Issues (17)", body: "STIC-1 hello"})
	mm = got.(Model)
	if mm.mvTitle != "Issues (17)" {
		t.Fatalf("title: %q", mm.mvTitle)
	}
	if !strings.Contains(mm.viewModuleView(), "Issues (17)") {
		t.Fatal("view must render the title")
	}

	// Failure returns to missions with a footer notice.
	got, _ = mm.openModuleView(ref)
	mm = got.(Model)
	got, _ = mm.onModViewMsg(modViewMsg{gen: mm.mvGen, mod: "jira", id: "page", err: errors.New("boom")})
	mm = got.(Model)
	if mm.screen != screenMissions || !strings.Contains(mm.status, "boom") {
		t.Fatalf("failure handling: screen=%v status=%q", mm.screen, mm.status)
	}

	// esc returns to missions.
	got, _ = mm.openModuleView(ref)
	mm = got.(Model)
	got, _ = mm.updateModuleViewKeys(tea.KeyMsg{Type: tea.KeyEsc})
	mm = got.(Model)
	if mm.screen != screenMissions {
		t.Fatal("esc must return to missions")
	}
}
