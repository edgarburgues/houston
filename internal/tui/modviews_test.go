package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

func TestBuildModContribsViewConflicts(t *testing.T) {
	// b-mod's view loses to a-mod's ACTION on the same key; d-mod's view
	// loses to c-mod's earlier view; e-mod's ACTION loses to c-mod's earlier
	// VIEW — the unified first-claimant rule with no class asymmetry.
	amod := viewModule("a-mod", "e") // builtin missions key: dropped
	amod.Manifest.Actions = []module.Action{{ID: "act", Key: "J", Title: "t", Screen: "missions"}}
	mods := []module.Module{
		amod,
		viewModule("b-mod", "J"), // a-mod action claimed J first: dropped
		viewModule("c-mod", "I"), // fine
		viewModule("d-mod", "I"), // cross-module dup: dropped
		{Entry: module.Entry{Name: "e-mod", Enabled: true}, Manifest: module.Manifest{
			API: 1, Name: "e-mod",
			Actions: []module.Action{{ID: "late", Key: "I", Title: "t", Screen: "missions"}},
		}},
	}
	arefs, _, vrefs, vaccepted, warns := buildModContribs(mods)
	if len(vaccepted) != 1 || vaccepted[0].mod.Name != "c-mod" {
		t.Fatalf("accepted views: %+v", vaccepted)
	}
	if _, ok := vrefs["missions:I"]; !ok {
		t.Fatal("winning view missing from refs")
	}
	if _, ok := arefs["missions:I"]; ok {
		t.Fatal("an earlier module's view must beat a later module's action")
	}
	if len(warns) != 4 {
		t.Fatalf("want 4 warns, got: %v", warns)
	}
}

func TestBuildModContribsPrunesViewActionKeys(t *testing.T) {
	mod := viewModule("jira", "I")
	mod.Manifest.Views[0].Actions = []module.ViewAction{
		{ID: "open", Key: "enter", Title: "open"},   // fine: nothing on the page owns enter
		{ID: "bad-core", Key: "r", Title: "t"},      // view-page built-in: dropped
		{ID: "bad-global", Key: "5", Title: "t"},    // tab key: dropped
		{ID: "comment", Key: "c", Title: "comment"}, // fine
	}
	_, _, _, vaccepted, warns := buildModContribs([]module.Module{mod})
	if len(vaccepted) != 1 {
		t.Fatalf("view should survive: %v", warns)
	}
	got := vaccepted[0].view.Actions
	if len(got) != 2 || got[0].ID != "open" || got[1].ID != "comment" {
		t.Fatalf("pruned actions wrong: %+v", got)
	}
	if len(warns) != 2 {
		t.Fatalf("want 2 prune warnings, got %v", warns)
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

func rowsModule() module.Module {
	return module.Module{
		Entry: module.Entry{Name: "jira", Enabled: true},
		Manifest: module.Manifest{
			API: 1, Name: "jira",
			Views: []module.View{{
				ID: "list", Key: "I", Title: "Issues", Command: []string{"cmd"},
				Actions: []module.ViewAction{{ID: "open", Key: "enter", Title: "open"}},
			}},
		},
		Dir: ".",
	}
}

func TestRowsViewNavigationFilterAndActions(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := newModelMods(t, rowsModule())
	tm, _ := m.Update(runes("I"))
	m = tm.(Model)
	ref := m.mvRef
	st := m.mvStates[viewKey(ref)]
	// Before the first render lands there is nothing to act on: the page
	// action must no-op instead of dispatching a body-shaped payload.
	if _, cmd := m.updateModuleViewKeys(key(tea.KeyEnter)); cmd != nil {
		t.Fatal("an action on an unloaded view must no-op")
	}
	tm, _ = m.Update(modViewMsg{gen: st.gen, mod: "jira", id: "list", title: "Issues (3)", rows: []module.ViewRow{
		{ID: "A-1", Text: "A-1 alpha"}, {ID: "B-2", Text: "B-2 beta"}, {ID: "A-3", Text: "A-3 gamma"},
	}})
	m = tm.(Model)
	if len(st.rows) != 3 || !st.loaded {
		t.Fatalf("rows must land in the state: %+v", st)
	}

	// Cursor moves and clamps; g jumps home.
	m = drive(m, runes("j"), runes("j"), runes("j"), runes("j"))
	if st.cur != 2 {
		t.Fatalf("cursor should clamp at the last row, got %d", st.cur)
	}
	m = drive(m, runes("g"))
	if st.cur != 0 {
		t.Fatalf("g should jump to the first row, got %d", st.cur)
	}

	// Local filter narrows instantly without any exec.
	m = drive(m, runes("/"))
	if m.act != actViewFilter {
		t.Fatal("/ must open the filter input on a rows view")
	}
	if !strings.Contains(m.View(), "Filter:") {
		t.Fatal("an armed filter input must be visible in the frame")
	}
	m = drive(m, runes("a"), runes("l"))
	if got := filteredIdx(st); len(got) != 1 || got[0] != 0 {
		t.Fatalf("filter 'al' should keep only the alpha row: %v", got)
	}
	m = drive(m, key(tea.KeyEnter))
	if m.act != actNone {
		t.Fatal("enter must close the filter input")
	}
	view := m.viewModuleView()
	if !strings.Contains(view, "filter: al") || !strings.Contains(view, "A-1 alpha") {
		t.Fatal("the frame should show the filter line and the matching row")
	}
	if !strings.Contains(view, "enter open") {
		t.Fatal("the footer should advertise the view's own actions")
	}

	// The page action dispatches on the selected filtered row.
	tm, cmd := m.updateModuleViewKeys(key(tea.KeyEnter))
	m = tm.(Model)
	if cmd == nil || !strings.Contains(m.status, "[jira] open") {
		t.Fatalf("enter must dispatch the view action: cmd=%v status=%q", cmd, m.status)
	}

	// No matching row: the action no-ops like the built-ins.
	st.filter = "zzz"
	st.cur = 0
	if _, cmd := m.updateModuleViewKeys(key(tea.KeyEnter)); cmd != nil {
		t.Fatal("an action without a selectable row must no-op")
	}
}

func TestViewActMsgRefreshRerendersView(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := newModelMods(t, rowsModule())
	tm, _ := m.Update(runes("I"))
	m = tm.(Model)
	ref := m.mvRef
	st := m.mvStates[viewKey(ref)]
	st.inflight = false
	before := st.gen

	tm, cmd := m.Update(viewActMsg{ref: ref, id: "open", refresh: true})
	m = tm.(Model)
	if cmd == nil || st.gen != before+1 || !st.inflight {
		t.Fatalf("a refreshing action must re-render the view: gen=%d inflight=%v", st.gen, st.inflight)
	}
	if !strings.Contains(m.status, "[jira] open done") {
		t.Fatalf("status: %q", m.status)
	}

	// Errors report and never refresh.
	tm, cmd = m.Update(viewActMsg{ref: ref, id: "open", refresh: true, err: errors.New("boom")})
	m = tm.(Model)
	if cmd != nil || !strings.Contains(m.status, "boom") {
		t.Fatalf("a failed action must not refresh: cmd=%v status=%q", cmd, m.status)
	}
}

// TestViewStatusAndFooterIntegrity: action outcomes are visible AT the view,
// the footer never wraps however long the page-action hints get, and a
// failed render closes a filter input armed on the dying view.
func TestViewStatusAndFooterIntegrity(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	mod := rowsModule()
	mod.Manifest.Views[0].Actions = []module.ViewAction{
		{ID: "open", Key: "enter", Title: "open the selected issue in the default browser"},
		{ID: "comment", Key: "c", Title: "add a long-form comment to the selected issue"},
		{ID: "assign", Key: "a2", Title: "reassign the selected issue to another user"},
	}
	mod.Manifest.Views[0].Actions[2].Key = "z"
	m := newModelMods(t, mod)
	tm, _ := m.Update(runes("I"))
	m = tm.(Model)
	ref := m.mvRef
	st := m.mvStates[viewKey(ref)]
	tm, _ = m.Update(modViewMsg{gen: st.gen, mod: "jira", id: "list", title: "Issues (1)", rows: []module.ViewRow{{ID: "A-1", Text: "A-1 alpha"}}})
	m = tm.(Model)

	// A view action outcome must be visible in the view's own frame.
	tm, _ = m.Update(viewActMsg{ref: ref, id: "open", status: "opened A-1"})
	m = tm.(Model)
	if !strings.Contains(m.View(), "[jira] opened A-1") {
		t.Fatal("the view frame must show the action outcome")
	}

	// The frame never exceeds the terminal height, however long the hints.
	if got := len(strings.Split(m.View(), "\n")); got != m.height {
		t.Fatalf("frame height %d, terminal %d", got, m.height)
	}

	// A failed render while the filter input is armed must close the input
	// with the view.
	m = drive(m, runes("/"))
	st.gen++
	st.inflight = true
	tm, _ = m.Update(modViewMsg{gen: st.gen, mod: "jira", id: "list", err: errors.New("boom")})
	m = tm.(Model)
	if m.screen != screenMissions || m.act != actNone {
		t.Fatalf("failure must close the stranded input: screen=%v act=%v", m.screen, m.act)
	}
}

// TestClipCellsBudgetsDisplayWidth: module-controlled row text with
// double-width runes must clip by cells, or the pane wraps and the frame
// outgrows the terminal.
func TestClipCellsBudgetsDisplayWidth(t *testing.T) {
	wide := strings.Repeat("日", 120) // 120 CJK runes = 240 cells
	got := clipCells(wide, 76)
	if w := lipgloss.Width(got); w > 76 {
		t.Fatalf("clipCells produced %d cells for a 76 budget", w)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatal("clipped text should end in an ellipsis")
	}
	if clipCells("short", 76) != "short" {
		t.Fatal("text within budget must pass through")
	}

	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := newModelMods(t, rowsModule())
	tm, _ := m.Update(runes("I"))
	m = tm.(Model)
	st := m.mvStates[viewKey(m.mvRef)]
	tm, _ = m.Update(modViewMsg{gen: st.gen, mod: "jira", id: "list", title: "W", rows: []module.ViewRow{{ID: "w", Text: wide}}})
	m = tm.(Model)
	if got := len(strings.Split(m.View(), "\n")); got != m.height {
		t.Fatalf("wide rows must not grow the frame: %d lines for height %d", got, m.height)
	}
}
