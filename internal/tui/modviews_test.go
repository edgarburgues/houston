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
	st := mm.mvStates[viewKey(ref)]
	if st == nil || !st.inflight {
		t.Fatal("open must create the retained state and mark the fetch in flight")
	}
	gen := st.gen

	// Stale render (older gen) must be dropped.
	got, _ = mm.onModViewMsg(modViewMsg{gen: gen - 1, mod: "jira", id: "page", title: "old", body: "old"})
	mm = got.(Model)
	if mm.mvStates[viewKey(ref)].title == "old" {
		t.Fatal("stale render applied")
	}

	// Current render lands in the state and reaches the frame.
	got, _ = mm.onModViewMsg(modViewMsg{gen: gen, mod: "jira", id: "page", title: "Issues (17)", body: "STIC-1 hello"})
	mm = got.(Model)
	if st := mm.mvStates[viewKey(ref)]; !st.loaded || st.inflight || st.title != "Issues (17)" {
		t.Fatalf("landed state wrong: %+v", st)
	}
	if !strings.Contains(mm.viewModuleView(), "Issues (17)") {
		t.Fatal("view must render the landed title")
	}

	// Re-opening reuses the retained render: no new fetch.
	got, cmd = mm.openModuleView(ref)
	mm = got.(Model)
	if cmd != nil {
		t.Fatal("a loaded view must not re-fetch on open")
	}

	// A failure on the visible view returns to missions with a footer notice
	// and leaves the state unloaded so the next open retries.
	st = mm.mvStates[viewKey(ref)]
	st.gen++
	got, _ = mm.onModViewMsg(modViewMsg{gen: st.gen, mod: "jira", id: "page", err: errors.New("boom")})
	mm = got.(Model)
	if mm.screen != screenMissions || !strings.Contains(mm.status, "boom") {
		t.Fatalf("failure handling: screen=%v status=%q", mm.screen, mm.status)
	}
	got, cmd = mm.openModuleView(ref)
	mm = got.(Model)
	if cmd == nil {
		t.Fatal("a failed view must re-fetch on the next open")
	}

	// esc returns to missions.
	got, _ = mm.updateModuleViewKeys(tea.KeyMsg{Type: tea.KeyEsc})
	mm = got.(Model)
	if mm.screen != screenMissions || mm.tabCur != 0 {
		t.Fatal("esc must return to the missions tab")
	}
}
