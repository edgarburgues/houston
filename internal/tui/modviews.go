package tui

// Module views: full-screen pages contributed by modules, opened from the
// missions screen by their manifest key or promoted to persistent tabs with
// "tab": true (tabs.go). A body reply is a read-only page scrolled by
// Houston; a rows reply is an interactive list where Houston owns cursor,
// scroll and the local / filter — navigating never execs, only the view's
// declared page actions do (view.invoke, dispatched to the view's own
// command with the selected row). Content lands OFF the event loop (tea.Cmd
// goroutine through the hardened Invoke) in a per-view retained state that
// survives tab switches; r refreshes, and a stale render can never paint
// over a newer one (the xformGen precedent). Keys inside a view:
// esc/backspace back to Missions, r refresh, arrows/jk and pgup/pgdn
// scroll or move the cursor, / filters rows, ? help.

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"houston/internal/module"
)

// moduleViewRef pairs a view with its owning module for dispatch. The view
// is the PRUNED copy built by buildModContribs (page actions whose keys the
// view screen owns are dropped there, with a warning).
type moduleViewRef struct {
	mod  module.Module
	view module.View
}

// viewKey identifies a view's retained state across renders and tabs.
func viewKey(ref moduleViewRef) string { return ref.mod.Name + "/" + ref.view.ID }

// viewInstance is one entry of the navigation stack: a view plus the row
// context it was opened FOR (nil on root views). "opens" actions push
// instances; esc pops.
type viewInstance struct {
	ref moduleViewRef
	row *module.ViewRow
}

// instKey identifies an instance's retained state: the same detail view
// opened for two different rows is two states.
func instKey(in viewInstance) string {
	if in.row != nil && in.row.ID != "" {
		return viewKey(in.ref) + "#" + in.row.ID
	}
	return viewKey(in.ref)
}

// modViewState is one view's retained screen state. Held by pointer in
// Model.mvStates: Bubble Tea copies the model by value on every Update and
// the state must survive those copies.
type modViewState struct {
	vp       viewport.Model
	title    string           // last title the handler returned ("" until loaded)
	gen      int              // generation of the newest dispatched render
	loaded   bool             // a render landed; activations reuse it
	inflight bool             // a render is being fetched; don't double-dispatch
	rows     []module.ViewRow // rows mode when non-empty
	cur      int              // cursor position within the FILTERED rows
	filter   string           // local case-insensitive substring filter
}

// filteredIdx maps the visible list to st.rows indexes under the local
// filter. Pure and cheap: rows are capped and this runs per keypress, not
// per exec.
func filteredIdx(st *modViewState) []int {
	if st.filter == "" {
		idx := make([]int, len(st.rows))
		for i := range idx {
			idx[i] = i
		}
		return idx
	}
	f := strings.ToLower(st.filter)
	var out []int
	for i, r := range st.rows {
		if strings.Contains(strings.ToLower(r.Text), f) || strings.Contains(strings.ToLower(r.ID), f) {
			out = append(out, i)
		}
	}
	return out
}

// modViewMsg carries one finished view render. skey is the instance state
// key; when empty (older tests) it derives from mod/id, the root form.
type modViewMsg struct {
	gen         int
	skey        string
	mod, id     string
	title, body string
	rows        []module.ViewRow
	err         error
}

// viewActMsg reports a view action's outcome (view.invoke) back to Update.
type viewActMsg struct {
	in      viewInstance
	id      string
	status  string
	refresh bool
	err     error
}

// buildModContribs claims every module contribution key in ONE pass per
// module in lexicographic order: within a module its actions claim before
// its views; across modules the earlier module keeps a key whatever the
// class — the documented first-claimant rule, with no action-beats-view
// asymmetry. Built-ins (tab keys included) always win. Each accepted view
// also gets its page actions pruned against the view screen's own keys.
func buildModContribs(mods []module.Module) (map[string]moduleActionRef, []moduleActionRef, map[string]moduleViewRef, []moduleViewRef, map[string]moduleViewRef, []string) {
	actions := map[string]moduleActionRef{}
	views := map[string]moduleViewRef{}
	all := map[string]moduleViewRef{} // every accepted view (internal too), for "opens" navigation
	var accActs []moduleActionRef
	var accViews []moduleViewRef
	var warns []string
	claimed := map[string]string{} // "screen:key" → owning module
	pageKeys := BuiltinViewPageKeys
	for _, mod := range mods {
		for _, a := range mod.Manifest.Actions {
			builtin := BuiltinMissionsKeys
			if a.Screen == "accounts" {
				builtin = BuiltinAccountsKeys
			}
			if builtin[a.Key] {
				warns = append(warns, fmt.Sprintf("[%s] action %s dropped: %q is a built-in %s key", mod.Name, a.ID, a.Key, a.Screen))
				continue
			}
			ck := a.Screen + ":" + a.Key
			if owner := claimed[ck]; owner != "" {
				warns = append(warns, fmt.Sprintf("[%s] action %s dropped: key %q already claimed by module %s", mod.Name, a.ID, a.Key, owner))
				continue
			}
			claimed[ck] = mod.Name
			ref := moduleActionRef{mod: mod, act: a}
			actions[ck] = ref
			accActs = append(accActs, ref)
		}
		for _, v := range mod.Manifest.Views {
			// Internal views (no key) skip the missions key space entirely:
			// they are reachable only through another view's "opens" action.
			if v.Key != "" {
				if BuiltinMissionsKeys[v.Key] {
					warns = append(warns, fmt.Sprintf("[%s] view %s dropped: %q is a built-in missions key", mod.Name, v.ID, v.Key))
					continue
				}
				ck := "missions:" + v.Key
				if owner := claimed[ck]; owner != "" {
					warns = append(warns, fmt.Sprintf("[%s] view %s dropped: key %q already claimed by module %s", mod.Name, v.ID, v.Key, owner))
					continue
				}
				claimed[ck] = mod.Name
			}
			var kept []module.ViewAction
			for _, va := range v.Actions {
				if pageKeys[va.Key] {
					warns = append(warns, fmt.Sprintf("[%s] view %s action %s dropped: %q is a built-in view-page key", mod.Name, v.ID, va.ID, va.Key))
					continue
				}
				kept = append(kept, va)
			}
			v.Actions = kept
			ref := moduleViewRef{mod: mod, view: v}
			all[mod.Name+"/"+v.ID] = ref
			if v.Key != "" {
				views["missions:"+v.Key] = ref
				accViews = append(accViews, ref)
			}
		}
	}
	return actions, accActs, views, accViews, all, warns
}

// ensureViewState returns the view's retained state, creating it (and the
// map, for bare test models) on first use. Pointer receiver: callers hold
// the model copy the assignment must land on.
func (m *Model) ensureViewState(in viewInstance) *modViewState {
	if m.mvStates == nil {
		m.mvStates = map[string]*modViewState{}
	}
	k := instKey(in)
	st, ok := m.mvStates[k]
	if !ok {
		st = &modViewState{vp: viewport.New(m.width-4, m.bodyH()-2)}
		m.mvStates[k] = st
	}
	return st
}

// mvTop is the visible view instance (zero value when no stack — callers on
// the module-view screen always have one).
func (m Model) mvTop() viewInstance {
	if n := len(m.mvStack); n > 0 {
		return m.mvStack[n-1]
	}
	return viewInstance{}
}

// viewFetchCmd dispatches a render for the view unless its retained state is
// already loaded or in flight. Callers own the screen/tab bookkeeping.
func (m *Model) viewFetchCmd(in viewInstance) tea.Cmd {
	st := m.ensureViewState(in)
	if st.loaded || st.inflight {
		return nil
	}
	st.gen++
	st.inflight = true
	st.vp.SetContent(dimStyle.Render("loading " + in.ref.view.Title + "…"))
	st.vp.GotoTop()
	return renderViewCmd(m.modCtx, in, st.gen)
}

// openModuleView shows a view: promoted views jump to their tab, the rest
// open the transient full-screen page. Retained content shows instantly; a
// first visit fetches.
func (m Model) openModuleView(ref moduleViewRef) (tea.Model, tea.Cmd) {
	if idx, ok := m.tabIdx[viewKey(ref)]; ok {
		return m.switchTab(idx)
	}
	m.screen = screenModuleView
	m.mvStack = []viewInstance{{ref: ref}}
	m.seedViewHint()
	return m, m.viewFetchCmd(m.mvTop())
}

// renderViewCmd fetches one view render off the event loop; recovers like
// every module goroutine.
func renderViewCmd(ctx context.Context, in viewInstance, gen int) tea.Cmd {
	ref, skey := in.ref, instKey(in)
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				msg = modViewMsg{gen: gen, skey: skey, mod: ref.mod.Name, id: ref.view.ID, err: fmt.Errorf("panic: %v", r)}
			}
		}()
		title, body, rows, err := module.RunView(ctx, ref.mod, ref.view, in.row)
		return modViewMsg{gen: gen, skey: skey, mod: ref.mod.Name, id: ref.view.ID, title: title, body: body, rows: rows, err: err}
	}
}

// onModViewMsg lands a finished render in the view's retained state; stale
// generations are dropped. A failure on the visible view falls back to the
// missions tab with the footer notice; one on a background view keeps its
// state unloaded (the next activation retries) and still notes the failure.
func (m Model) onModViewMsg(msg modViewMsg) (tea.Model, tea.Cmd) {
	key := msg.skey
	if key == "" {
		key = msg.mod + "/" + msg.id
	}
	st, ok := m.mvStates[key]
	if !ok || msg.gen != st.gen {
		return m, nil
	}
	st.inflight = false
	if msg.err != nil {
		st.loaded = false
		if m.screen == screenModuleView && instKey(m.mvTop()) == key {
			// Leaving the dead view must also close a filter input armed on
			// it — otherwise the prompt strands over Missions, silently
			// editing the failed view's retained filter.
			if m.act == actViewFilter {
				m.act = actNone
				m.input.Blur()
			}
			m.screen = screenMissions
			m.tabCur = 0
			m.mvStack = nil
		}
		m.note(actionFailStatus(msg.mod, msg.id, msg.err))
		return m, nil
	}
	st.loaded = true
	st.title = msg.title
	st.rows = msg.rows
	if m.screen == screenModuleView && instKey(m.mvTop()) == key {
		// Content just landed on the visible view: refresh the rich hint
		// ("/ filter" appears once rows exist, "refreshing …" retires).
		m.seedViewHint()
	}
	if len(st.rows) > 0 {
		if idxs := filteredIdx(st); st.cur >= len(idxs) {
			st.cur = len(idxs) - 1
		}
		if st.cur < 0 {
			st.cur = 0
		}
		return m, nil
	}
	if strings.TrimSpace(msg.body) == "" {
		st.vp.SetContent(dimStyle.Render("(the view returned nothing)"))
	} else {
		st.vp.SetContent(msg.body)
	}
	return m, nil
}

// updateModuleViewKeys handles keys while a module view owns the screen. On
// a rows view the movement keys drive the cursor; on a body view they
// scroll the page. Unmatched keys fall through to the view's own page
// actions — navigation never execs, actions do.
func (m Model) updateModuleViewKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	top := m.mvTop()
	st := m.ensureViewState(top)
	rows := len(st.rows) > 0
	switch msg.String() {
	case "q", "ctrl+c":
		m.modCancel()
		return m, tea.Quit
	case "esc", "backspace":
		// esc pops a pushed sub-page first; from the root it goes home
		// (transient views were opened from Missions, and from a tab view
		// home is the Missions tab).
		if len(m.mvStack) > 1 {
			m.mvStack = m.mvStack[:len(m.mvStack)-1]
			m.seedViewHint()
			return m, nil
		}
		m.screen = screenMissions
		m.tabCur = 0
		m.mvStack = nil
		m.seedHint(scrMissions)
		return m, nil
	case "?":
		m.helpOpen = true
		m.helpScroll = 0
		return m, nil
	case "/":
		if rows {
			m.startInput(actViewFilter, "Filter: ")
			m.input.SetValue(st.filter)
		}
		return m, nil
	case "r":
		// Refresh keeps the current content on screen (no flash): the footer
		// says what's happening and the reply swaps the body in.
		st.gen++
		st.inflight = true
		m.status = "refreshing " + top.ref.view.Title + "…"
		return m, renderViewCmd(m.modCtx, top, st.gen)
	case "up", "k":
		if rows {
			if st.cur > 0 {
				st.cur--
			}
		} else {
			st.vp.ScrollUp(1)
		}
	case "down", "j":
		if rows {
			if st.cur < len(filteredIdx(st))-1 {
				st.cur++
			}
		} else {
			st.vp.ScrollDown(1)
		}
	case "pgup", "b":
		if rows {
			st.cur -= 10
			if st.cur < 0 {
				st.cur = 0
			}
		} else {
			st.vp.HalfPageUp()
		}
	case "pgdown", "f", " ":
		if rows {
			if n := len(filteredIdx(st)); n > 0 {
				st.cur += 10
				if st.cur >= n {
					st.cur = n - 1
				}
			}
		} else {
			st.vp.HalfPageDown()
		}
	case "g":
		if rows {
			st.cur = 0
		} else {
			st.vp.GotoTop()
		}
	case "G":
		if rows {
			if n := len(filteredIdx(st)); n > 0 {
				st.cur = n - 1
			}
		} else {
			st.vp.GotoBottom()
		}
	default:
		for _, va := range top.ref.view.Actions {
			if va.Key == msg.String() {
				if va.Opens != "" {
					return m.openSubView(va.Opens)
				}
				return m.runViewAction(va)
			}
		}
	}
	return m, nil
}

// openSubView pushes another view of the same module as a sub-page: on a
// rows view the selected (filtered) row becomes the new page's context, on
// a body view the current context passes through. Pure navigation — no
// handler runs here; the fetch (if the instance has no retained render)
// is a normal view.render with payload.row set.
func (m Model) openSubView(id string) (tea.Model, tea.Cmd) {
	top := m.mvTop()
	st := m.ensureViewState(top)
	row := top.row
	if len(st.rows) > 0 {
		idxs := filteredIdx(st)
		if len(idxs) == 0 {
			return m, nil
		}
		cur := st.cur
		if cur >= len(idxs) {
			cur = len(idxs) - 1
		}
		if cur < 0 {
			cur = 0
		}
		r := st.rows[idxs[cur]]
		row = &r
	}
	target, ok := m.mvAll[top.ref.mod.Name+"/"+id]
	if !ok {
		return m, nil
	}
	in := viewInstance{ref: target, row: row}
	m.mvStack = append(m.mvStack, in)
	m.seedViewHint()
	return m, m.viewFetchCmd(in)
}

// runViewAction dispatches one page action for the visible view: rows views
// act on the selected (filtered) row and no-op without one, body views
// invoke with no row. Non-interactive runs go through the hardened Invoke
// in a tea.Cmd goroutine; interactive ones get the plain terminal via
// tea.ExecProcess, envelope in a store tmp file.
func (m Model) runViewAction(va module.ViewAction) (tea.Model, tea.Cmd) {
	if m.launchBusy() {
		return m, nil
	}
	in := m.mvTop()
	ref := in.ref
	st := m.ensureViewState(in)
	if !st.loaded {
		// Nothing rendered yet (first load in flight, or the last render
		// failed): there is nothing to act on, and dispatching now would
		// send a body-view-shaped payload for what may be a rows view.
		return m, nil
	}
	// Rows views act on the selected (filtered) row; body sub-pages act on
	// their navigation context (the row they were opened FOR) — that is what
	// makes "comment from the detail page" work.
	row := in.row
	if len(st.rows) > 0 {
		idxs := filteredIdx(st)
		if len(idxs) == 0 {
			return m, nil
		}
		cur := st.cur
		if cur >= len(idxs) {
			cur = len(idxs) - 1
		}
		if cur < 0 {
			cur = 0
		}
		r := st.rows[idxs[cur]]
		row = &r
	}
	env := module.NewEnvelope(module.EventViewInvoke, ref.mod, module.ViewInvokePayload{View: ref.view.ID, Action: va.ID, Row: row})
	m.status = "[" + ref.mod.Name + "] " + va.ID + "…"
	if !va.Interactive {
		return m, runViewActionCmd(m.modCtx, in, va, env)
	}
	cmd, cleanup, err := module.ExecViewAction(ref.mod, ref.view, env)
	if err != nil {
		module.LogEvent(ref.mod.Name, module.EventViewInvoke, err.Error(), nil)
		m.note(actionFailStatus(ref.mod.Name, va.ID, err))
		return m, nil
	}
	id, refresh := va.ID, va.RefreshAfter
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		cleanup()
		if err != nil {
			module.LogEvent(ref.mod.Name, module.EventViewInvoke, "interactive: "+err.Error(), nil)
			return viewActMsg{in: in, id: id, err: err}
		}
		return viewActMsg{in: in, id: id, refresh: refresh}
	})
}

// runViewActionCmd runs a non-interactive view action off the event loop;
// recovers like every module goroutine.
func runViewActionCmd(ctx context.Context, in viewInstance, va module.ViewAction, env module.Envelope) tea.Cmd {
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				msg = viewActMsg{in: in, id: va.ID, err: fmt.Errorf("panic: %v", r)}
			}
		}()
		rep, err := module.RunViewAction(ctx, in.ref.mod, in.ref.view, va, env)
		if err != nil {
			return viewActMsg{in: in, id: va.ID, err: err}
		}
		return viewActMsg{in: in, id: va.ID, status: rep.Status, refresh: rep.Refresh}
	}
}

// onViewActMsg lands a view action's outcome: the footer reports it, and a
// refresh re-renders the VIEW (not the mission list) through the normal
// generation machinery.
func (m Model) onViewActMsg(msg viewActMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.err != nil:
		m.note(actionFailStatus(msg.in.ref.mod.Name, msg.id, msg.err))
	case msg.status != "":
		m.note("[" + msg.in.ref.mod.Name + "] " + msg.status)
	default:
		m.note("[" + msg.in.ref.mod.Name + "] " + msg.id + " done")
	}
	if msg.err != nil || !msg.refresh {
		return m, nil
	}
	st := m.ensureViewState(msg.in)
	st.gen++
	st.inflight = true
	return m, renderViewCmd(m.modCtx, msg.in, st.gen)
}

// viewFooterHint is the page footer: the view's own actions first (plus the
// / filter on rows views), then the core hint.
func (m Model) viewFooterHint() string {
	base := hintFor(m.registry, scrModView)
	top := m.mvTop()
	var parts []string
	for _, va := range top.ref.view.Actions {
		parts = append(parts, keyLabel(va.Key)+" "+va.Title)
	}
	if st := m.mvStates[instKey(top)]; st != nil && len(st.rows) > 0 {
		parts = append(parts, "/ filter")
	}
	if len(parts) == 0 {
		return base
	}
	return strings.Join(parts, " · ") + " · " + base
}

// viewActionCommands projects the visible view's page actions as registry
// commands so the ? overlay advertises them while the view is on screen.
func (m Model) viewActionCommands() []command {
	if m.screen != screenModuleView {
		return nil
	}
	top := m.mvTop()
	var out []command
	for _, va := range top.ref.view.Actions {
		out = append(out, command{
			keys: []string{va.Key}, label: keyLabel(va.Key), title: va.Title,
			screen: scrModView, category: top.ref.mod.Name, origin: originModule, module: top.ref.mod.Name,
		})
	}
	return out
}

// viewModuleView renders the page: the tab strip when the view is a tab, a
// title bar when transient; a cursor list for rows views or the scrollable
// body; the action-aware hint footer.
func (m Model) viewModuleView() string {
	top := m.mvTop()
	st := m.mvStates[instKey(top)]
	title := top.ref.view.Title
	if st != nil && st.title != "" {
		title = st.title
	}
	// Sub-pages carry a breadcrumb back to their root.
	if len(m.mvStack) > 1 {
		title = m.mvStack[0].ref.view.Title + " › " + title
	}
	var header string
	if _, ok := m.tabIdx[viewKey(m.mvStack[0].ref)]; len(m.mvStack) > 0 && ok && len(m.mvStack) == 1 {
		header = m.viewTabBar(top.ref.mod.Name)
	} else {
		header = headerStyle.Width(m.width).Render("▤ " + title + "  · " + top.ref.mod.Name)
	}
	h := m.bodyH() - 2
	w := m.width - 4
	var content string
	if st != nil && len(st.rows) > 0 {
		idxs := filteredIdx(st)
		cur := st.cur
		if cur >= len(idxs) {
			cur = len(idxs) - 1
		}
		if cur < 0 {
			cur = 0
		}
		var lines []string
		listH := h
		if st.filter != "" {
			lines = append(lines, dimStyle.Render(clipCells(fmt.Sprintf("filter: %s — %d/%d (/ edits, empty clears)", st.filter, len(idxs), len(st.rows)), w)))
			listH--
		}
		start, end := windowBounds(cur, len(idxs), listH)
		for i := start; i < end; i++ {
			line := clipCells(st.rows[idxs[i]].Text, w)
			if i == cur {
				line = selStyle.Width(w).Render(line)
			}
			lines = append(lines, line)
		}
		if len(idxs) == 0 {
			lines = append(lines, dimStyle.Render("(no rows match)"))
		}
		content = padBox(lines, h)
	} else if st != nil {
		content = st.vp.View()
	}
	box := paneFocused.Width(m.width - 2).Height(h).Render(content)
	// The footer is the status line (seedViewHint keeps the rich hint there
	// when nothing real is showing), clipped to one line — action outcomes,
	// failures and refresh progress are visible AT the view, per the failure
	// table. The filter input renders here too: an armed input must never be
	// invisible.
	var footer string
	if m.act == actViewFilter {
		footer = keyStyle.Render(m.prompt) + m.input.View()
	} else {
		footer = footerStyle.Render(clip(m.status, m.width))
	}
	return header + "\n" + box + "\n" + footer
}
