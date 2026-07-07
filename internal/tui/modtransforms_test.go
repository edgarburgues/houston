package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"houston/internal/model"
	"houston/internal/module"
	"houston/internal/store"
)

// helperCmd re-execs this test binary into TestHelperProcess with a mode —
// the cross-platform handler stand-in (module/runner_test.go's idiom: no
// pwsh/sh scripts in CI, identical behavior on Windows and POSIX).
func helperCmd(mode string) []string {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return []string{exe, "-test.run=^TestHelperProcess$", "--", mode}
}

// TestHelperProcess is not a test: it is the re-exec target for the handler
// stand-ins. It only acts when invoked with a "--"-separated mode.
func TestHelperProcess(t *testing.T) {
	args := os.Args
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep+1 >= len(args) {
		return
	}
	mode := args[sep+1]
	defer os.Exit(0)
	switch mode {
	case "transform-first":
		// Patch the first mission of the projection: badge + title.
		var env struct {
			Payload struct {
				Missions []struct {
					Key string `json:"key"`
				} `json:"missions"`
			} `json:"payload"`
		}
		if err := json.NewDecoder(os.Stdin).Decode(&env); err != nil || len(env.Payload.Missions) == 0 {
			fmt.Print(`{"patches":[]}`)
			return
		}
		io.Copy(io.Discard, os.Stdin)
		fmt.Printf(`{"patches":[{"key":%q,"badge":"GEN","title":"transformed"}]}`, env.Payload.Missions[0].Key)
	case "preview-one":
		io.Copy(io.Discard, os.Stdin)
		fmt.Print(`{"sections":[{"title":"Jira","body":"PROJ-1 In Review"}]}`)
	}
}

// xformModule builds an in-memory enabled module whose missions transform is
// a helper re-exec. The generous timeout keeps slow CI from flaking the
// test-binary respawn.
func xformModule(t *testing.T, name, mode string) module.Module {
	t.Helper()
	mod := module.Module{
		Entry:    module.Entry{Name: name, Enabled: true},
		Manifest: module.Manifest{API: 1, Name: name},
		Dir:      t.TempDir(),
	}
	mod.Manifest.Transforms.Missions = &module.Handler{Command: helperCmd(mode), TimeoutMs: 10000}
	return mod
}

func previewModule(t *testing.T, name, mode string) module.Module {
	t.Helper()
	mod := module.Module{
		Entry:    module.Entry{Name: name, Enabled: true},
		Manifest: module.Manifest{API: 1, Name: name},
		Dir:      t.TempDir(),
	}
	mod.Manifest.Transforms.Preview = &module.Handler{Command: helperCmd(mode), TimeoutMs: 10000}
	return mod
}

// missionKeys returns the fakeMissions keys by recency: a newest, c oldest.
func missionKeys(m Model) (a, b, c string) {
	for _, ms := range m.missions {
		switch ms.ID {
		case "aaaa1111":
			a = ms.Key()
		case "bbbb2222":
			b = ms.Key()
		case "cccc3333":
			c = ms.Key()
		}
	}
	return
}

func assertOrder(t *testing.T, mid []model.Mission, want ...string) {
	t.Helper()
	var got []string
	for _, ms := range mid {
		got = append(got, ms.Key())
	}
	if len(got) != len(want) {
		t.Fatalf("mid rows: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mid order: got %v, want %v", got, want)
		}
	}
}

// TestStartupTransformAndRescanRedispatch is the concurrency-critical path:
// the gen bookkeeping lives in New (value-receiver Init cannot mutate the
// model Bubble Tea keeps), so the startup reply must arrive as gen 1 and the
// first r rescan must find a non-nil xformStop instead of panicking.
func TestStartupTransformAndRescanRedispatch(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := newModelMods(t, xformModule(t, "badger", "transform-first"))
	first := m.missions[0].Key()

	init := m.Init()
	if init == nil {
		t.Fatal("New must stash the startup transform dispatch for Init")
	}
	msg := init()
	xm, ok := msg.(xformMsg)
	if !ok || xm.gen != 1 {
		t.Fatalf("startup dispatch: %T %+v", msg, msg)
	}
	m = drive(m, msg)
	p := m.modPatches[first]
	if p.Badge != "GEN" || !p.HasTitle || p.Title != "transformed" {
		t.Fatalf("gen-1 patches not applied: %+v", p)
	}
	if v := m.viewMid(); !strings.Contains(v, "[GEN]") || strings.Contains(v, "Pokewalker") {
		t.Fatalf("badge/title substitution missing from the pane:\n%s", v)
	}

	tm, cmd := m.Update(runes("r")) // must not panic: xformStop set in New
	m = tm.(Model)
	if m.xformGen != 2 {
		t.Fatalf("rescan must bump the generation, got %d", m.xformGen)
	}
	if cmd == nil {
		t.Fatal("rescan must re-dispatch the transforms")
	}
	msg = cmd()
	xm, ok = msg.(xformMsg)
	if !ok || xm.gen != 2 {
		t.Fatalf("re-dispatch: %T %+v", msg, msg)
	}
	m = drive(m, msg)
	if m.modPatches[first].Badge != "GEN" {
		t.Fatal("the gen-2 reply must apply")
	}
}

func TestStaleGenXformDropped(t *testing.T) {
	m := newModel(t) // xformGen == 1 from New
	keyA, _, _ := missionKeys(m)
	m = drive(m, xformMsg{gen: 1, patches: map[string]module.Patch{keyA: {Key: keyA, Badge: "OK"}}})
	if m.modPatches[keyA].Badge != "OK" {
		t.Fatal("a current-gen reply must apply")
	}
	m = drive(m, xformMsg{gen: 7, patches: map[string]module.Patch{keyA: {Key: keyA, Badge: "STALE"}}})
	if m.modPatches[keyA].Badge != "OK" {
		t.Fatalf("a stale-gen reply must leave state untouched: %+v", m.modPatches[keyA])
	}
}

func TestHidePatchInDefaultViews(t *testing.T) {
	m := newModel(t)
	keyA, keyB, keyC := missionKeys(m)
	m.modPatches = map[string]module.Patch{keyB: {Key: keyB, Hide: true}}
	m.rebuildMid()
	assertOrder(t, m.mid, keyA, keyC)
}

func TestHidePatchInProgramViewsAndSortKeyIgnored(t *testing.T) {
	m := newModel(t)
	keyA, keyB, keyC := missionKeys(m)
	if err := m.st.CreateProgram("prog", ""); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{keyC, keyA, keyB} { // curated order ≠ recency
		if err := m.st.AddToProgram("prog", k); err != nil {
			t.Fatal(err)
		}
	}
	m.refresh()
	m.leftCur = 3 // All, Pinned, Archived, then the program
	m.modPatches = map[string]module.Patch{
		keyB: {Key: keyB, SortKey: "0"}, // would sort first if honored
		keyA: {Key: keyA, Hide: true},   // must vanish here too
	}
	m.rebuildMid()
	assertOrder(t, m.mid, keyC, keyB) // curated order kept, hidden row gone
}

func TestSortKeyOrdersWithinGroups(t *testing.T) {
	m := newModel(t)
	keyA, keyB, keyC := missionKeys(m)
	m.modPatches = map[string]module.Patch{
		keyB: {Key: keyB, SortKey: "2"},
		keyC: {Key: keyC, SortKey: "1"},
	}
	m.rebuildMid()
	// sortKey'd rows ascend ahead of the rest, which keep recency order.
	assertOrder(t, m.mid, keyC, keyB, keyA)

	// Pinned-first is inviolable: a pinned row without a sortKey still beats
	// every sortKey'd unpinned row.
	if err := m.st.TogglePin(keyA); err != nil {
		t.Fatal(err)
	}
	m.rebuildMid()
	assertOrder(t, m.mid, keyA, keyC, keyB)
}

func TestMatchRunsOnOriginalTitles(t *testing.T) {
	m := newModel(t)
	keyA, _, _ := missionKeys(m)
	m.modPatches = map[string]module.Patch{keyA: {Key: keyA, HasTitle: true, Title: "ZZZ renamed"}}
	m.query = "pokewalker"
	m.rebuildMid()
	assertOrder(t, m.mid, keyA) // the original title still matches
	m.query = "zzz"
	m.rebuildMid()
	if len(m.mid) != 0 {
		t.Fatal("patched titles are presentation, not search identity")
	}
}

// TestViewMidBadgeClipBudget: the badge is budgeted into the pre-style clip,
// so a row never overflows the pane and wraps (which would corrupt the
// column layout); a pane too narrow for the badge drops the badge instead.
func TestViewMidBadgeClipBudget(t *testing.T) {
	tests := []struct {
		name      string
		width     int
		wantBadge bool
	}{
		{"wide-shows-badge", 120, true},
		{"narrow-drops-badge", 100, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := store.LoadFrom(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := New("root", nil, st, fakeMissions(), nil, nil)
			m = drive(m, tea.WindowSizeMsg{Width: tt.width, Height: 30})
			key := m.mid[0].Key()
			m.modPatches[key] = module.Patch{
				Key: key, HasTitle: true,
				Title: strings.Repeat("t", 300),
				Badge: "0123456789ABCDEF", // the 16-rune wire maximum
			}
			m.rebuildMid()
			pane := m.viewMid()
			lines := strings.Split(pane, "\n")
			if len(lines) != m.bodyH() {
				t.Fatalf("pane is %d lines, want %d — an overflowing row wrapped", len(lines), m.bodyH())
			}
			for i, ln := range lines {
				if n := len([]rune(ln)); n > m.midW() {
					t.Fatalf("line %d is %d runes, pane is %d:\n%q", i, n, m.midW(), ln)
				}
			}
			if got := strings.Contains(pane, "[0123456789ABCDEF]"); got != tt.wantBadge {
				t.Fatalf("badge shown = %v, want %v:\n%s", got, tt.wantBadge, pane)
			}
		})
	}
}

func TestPreviewSectionsFetchAndRearm(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := newModelMods(t, previewModule(t, "prevmod", "preview-one"))
	sel, ok := m.selected()
	if !ok {
		t.Fatal("no selection")
	}
	key := sel.Key()
	if m.lastPreviewKey != key {
		t.Fatalf("the first sized render must arm the preview, got %q", m.lastPreviewKey)
	}

	// The debounce fired with the cursor still on the key → fetch dispatched.
	tm, cmd := m.Update(previewTickMsg{key: key})
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("a tick on a live selection must dispatch the fetch")
	}
	msg := cmd()
	pm, isPreview := msg.(modPreviewMsg)
	if !isPreview || pm.key != key || len(pm.sections) != 1 {
		t.Fatalf("fetch reply: %T %+v", msg, msg)
	}
	m = drive(m, msg)
	if len(m.prevCache[key]) != 1 {
		t.Fatal("sections must land in the cache")
	}
	if v := m.preview.View(); !strings.Contains(v, "prevmod: Jira") || !strings.Contains(v, "PROJ-1 In Review") {
		t.Fatalf("preview must render the cached section:\n%s", v)
	}

	// Cached key: the next tick must not re-exec the handler.
	if _, cmd := m.Update(previewTickMsg{key: key}); cmd != nil {
		t.Fatal("cached sections must not re-exec the handler")
	}
	// Stale tick (the cursor moved on before the debounce fired): no exec.
	if _, cmd := m.Update(previewTickMsg{key: "gone/away"}); cmd != nil {
		t.Fatal("a stale tick must not exec")
	}
	// Stale reply: dropped, not cached.
	m = drive(m, modPreviewMsg{key: "gone/away", sections: []module.Section{{Module: "x"}}})
	if _, cached := m.prevCache["gone/away"]; cached {
		t.Fatal("a stale reply must be dropped")
	}

	// Rescan: the cache dies with the scan and lastPreviewKey re-arms for
	// the SAME selected key, so the sections come back.
	tm, cmd = m.Update(runes("r"))
	m = tm.(Model)
	if len(m.prevCache) != 0 {
		t.Fatal("rescan must clear the preview cache")
	}
	if m.lastPreviewKey != key {
		t.Fatalf("rescan must re-arm the unchanged selection, got %q", m.lastPreviewKey)
	}
	if cmd == nil {
		t.Fatal("rescan must schedule a fresh debounce tick")
	}
	msg = cmd() // the debounce timer; unwrap in case it arrived batched
	if bm, isBatch := msg.(tea.BatchMsg); isBatch && len(bm) == 1 {
		msg = bm[0]()
	}
	tick, isTick := msg.(previewTickMsg)
	if !isTick || tick.key != key {
		t.Fatalf("re-armed tick: %T %+v", msg, msg)
	}
	if _, cmd := m.Update(tick); cmd == nil {
		t.Fatal("the re-armed tick must re-fetch the cleared sections")
	}
}

// TestTransformFailureRetention: a blip must not flash every badge off — the
// previous generation's keys survive up to 3 consecutive failed generations,
// then drop; a clean reply replaces the map immediately.
func TestTransformFailureRetention(t *testing.T) {
	m := newModelMods(t, xformModule(t, "flaky", "transform-first"))
	keyA, _, _ := missionKeys(m)
	good := func() tea.Msg {
		return xformMsg{gen: 1, patches: map[string]module.Patch{keyA: {Key: keyA, Badge: "OK"}}}
	}
	fail := func() tea.Msg {
		return xformMsg{gen: 1, patches: map[string]module.Patch{},
			warnings: []string{"[flaky] transform: exit status 1"}}
	}

	m = drive(m, good())
	for i := 1; i <= maxXformFails; i++ {
		m = drive(m, fail())
		if m.modPatches[keyA].Badge != "OK" {
			t.Fatalf("failure %d must keep the previous patches", i)
		}
	}
	m = drive(m, fail())
	if _, kept := m.modPatches[keyA]; kept {
		t.Fatal("the 4th consecutive failure must drop the retained patches")
	}

	// A clean generation resets the streak and replaces the map outright: a
	// module that legitimately stops patching a key takes effect at once.
	m = drive(m, good(), fail())
	if m.modPatches[keyA].Badge != "OK" {
		t.Fatal("the streak must restart after a clean generation")
	}
	m = drive(m, xformMsg{gen: 1, patches: map[string]module.Patch{}})
	if len(m.modPatches) != 0 {
		t.Fatalf("a clean empty reply must drop patches immediately: %+v", m.modPatches)
	}
}

func TestPassiveWarningsOncePerModule(t *testing.T) {
	m := newModel(t)
	m = drive(m, xformMsg{gen: 1, warnings: []string{"[noisy] transform: boom"}})
	if m.status != "[noisy] transform: boom" {
		t.Fatalf("first warning must reach the footer, got %q", m.status)
	}
	m.status = "quiet"
	m = drive(m, xformMsg{gen: 1, warnings: []string{"[noisy] transform: boom again"}})
	if m.status != "quiet" {
		t.Fatalf("a module's second warning must stay out of the footer, got %q", m.status)
	}
}
