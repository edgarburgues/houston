package tui

// Module views: full-screen read-only pages contributed by modules, opened
// from the missions screen by their manifest key or promoted to persistent
// tabs with "tab": true (tabs.go). Content is fetched OFF the event loop
// (tea.Cmd goroutine through the hardened Invoke) into a per-view retained
// state — viewport, last title, generation — so tab switches and re-opens
// never re-execute a handler; r refreshes the visible view and a stale
// render can never paint over a newer one (the xformGen precedent). Keys
// inside a view: esc/backspace back to Missions, r refresh, arrows/jk and
// pgup/pgdn scroll, ? help. Built-in missions keys and this module's own
// action keys win conflicts at load time, like actions do.

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"houston/internal/module"
)

// moduleViewRef pairs a view with its owning module for dispatch.
type moduleViewRef struct {
	mod  module.Module
	view module.View
}

// viewKey identifies a view's retained state across renders and tabs.
func viewKey(ref moduleViewRef) string { return ref.mod.Name + "/" + ref.view.ID }

// modViewState is one view's retained screen state. Held by pointer in
// Model.mvStates: Bubble Tea copies the model by value on every Update and
// the state must survive those copies.
type modViewState struct {
	vp       viewport.Model
	title    string // last title the handler returned ("" until loaded)
	gen      int    // generation of the newest dispatched render
	loaded   bool   // a render landed; activations reuse it
	inflight bool   // a render is being fetched; don't double-dispatch
}

// modViewMsg carries one finished view render.
type modViewMsg struct {
	gen         int
	mod, id     string
	title, body string
	err         error
}

// buildModViews filters the enabled modules' views into the runtime key map
// ("missions:I" → ref). actionKeys are the keys module actions already
// claimed — views share the missions key space with them.
func buildModViews(mods []module.Module, actionKeys map[string]bool) (map[string]moduleViewRef, []moduleViewRef, []string) {
	refs := map[string]moduleViewRef{}
	var accepted []moduleViewRef
	var warns []string
	for _, mod := range mods {
		for _, v := range mod.Manifest.Views {
			if BuiltinMissionsKeys[v.Key] {
				warns = append(warns, fmt.Sprintf("[%s] view %s dropped: %q is a built-in missions key", mod.Name, v.ID, v.Key))
				continue
			}
			if actionKeys[v.Key] {
				warns = append(warns, fmt.Sprintf("[%s] view %s dropped: key %q already claimed by a module action", mod.Name, v.ID, v.Key))
				continue
			}
			id := "missions:" + v.Key
			if prev, taken := refs[id]; taken {
				warns = append(warns, fmt.Sprintf("[%s] view %s dropped: key %q already claimed by module %s", mod.Name, v.ID, v.Key, prev.mod.Name))
				continue
			}
			ref := moduleViewRef{mod: mod, view: v}
			refs[id] = ref
			accepted = append(accepted, ref)
		}
	}
	return refs, accepted, warns
}

// ensureViewState returns the view's retained state, creating it (and the
// map, for bare test models) on first use. Pointer receiver: callers hold
// the model copy the assignment must land on.
func (m *Model) ensureViewState(ref moduleViewRef) *modViewState {
	if m.mvStates == nil {
		m.mvStates = map[string]*modViewState{}
	}
	k := viewKey(ref)
	st, ok := m.mvStates[k]
	if !ok {
		st = &modViewState{vp: viewport.New(m.width-4, m.bodyH()-2)}
		m.mvStates[k] = st
	}
	return st
}

// viewFetchCmd dispatches a render for the view unless its retained state is
// already loaded or in flight. Callers own the screen/tab bookkeeping.
func (m *Model) viewFetchCmd(ref moduleViewRef) tea.Cmd {
	st := m.ensureViewState(ref)
	if st.loaded || st.inflight {
		return nil
	}
	st.gen++
	st.inflight = true
	st.vp.SetContent(dimStyle.Render("loading " + ref.view.Title + "…"))
	st.vp.GotoTop()
	return renderViewCmd(m.modCtx, ref, st.gen)
}

// openModuleView shows a view: promoted views jump to their tab, the rest
// open the transient full-screen page. Retained content shows instantly; a
// first visit fetches.
func (m Model) openModuleView(ref moduleViewRef) (tea.Model, tea.Cmd) {
	if idx, ok := m.tabIdx[viewKey(ref)]; ok {
		return m.switchTab(idx)
	}
	m.screen = screenModuleView
	m.mvRef = ref
	m.status = hintFor(m.registry, scrModView)
	return m, m.viewFetchCmd(ref)
}

// renderViewCmd fetches one view render off the event loop; recovers like
// every module goroutine.
func renderViewCmd(ctx context.Context, ref moduleViewRef, gen int) tea.Cmd {
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				msg = modViewMsg{gen: gen, mod: ref.mod.Name, id: ref.view.ID, err: fmt.Errorf("panic: %v", r)}
			}
		}()
		title, body, err := module.RunView(ctx, ref.mod, ref.view)
		return modViewMsg{gen: gen, mod: ref.mod.Name, id: ref.view.ID, title: title, body: body, err: err}
	}
}

// onModViewMsg lands a finished render in the view's retained state; stale
// generations are dropped. A failure on the visible view falls back to the
// missions tab with the footer notice; one on a background view keeps its
// state unloaded (the next activation retries) and still notes the failure.
func (m Model) onModViewMsg(msg modViewMsg) (tea.Model, tea.Cmd) {
	key := msg.mod + "/" + msg.id
	st, ok := m.mvStates[key]
	if !ok || msg.gen != st.gen {
		return m, nil
	}
	st.inflight = false
	if msg.err != nil {
		st.loaded = false
		if m.screen == screenModuleView && viewKey(m.mvRef) == key {
			m.screen = screenMissions
			m.tabCur = 0
		}
		m.status = actionFailStatus(msg.mod, msg.id, msg.err)
		return m, nil
	}
	st.loaded = true
	st.title = msg.title
	if strings.TrimSpace(msg.body) == "" {
		st.vp.SetContent(dimStyle.Render("(the view returned nothing)"))
	} else {
		st.vp.SetContent(msg.body)
	}
	return m, nil
}

// updateModuleViewKeys handles keys while a module view owns the screen.
func (m Model) updateModuleViewKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	st := m.ensureViewState(m.mvRef)
	switch msg.String() {
	case "q", "ctrl+c":
		m.modCancel()
		return m, tea.Quit
	case "esc", "backspace":
		// esc always goes home: transient views were opened from Missions,
		// and from a tab view home is the Missions tab.
		m.screen = screenMissions
		m.tabCur = 0
		m.status = hintFor(m.registry, scrMissions)
		return m, nil
	case "?":
		m.helpOpen = true
		m.helpScroll = 0
		return m, nil
	case "r":
		// Refresh keeps the current content on screen (no flash): the footer
		// says what's happening and the reply swaps the body in.
		st.gen++
		st.inflight = true
		m.status = "refreshing " + m.mvRef.view.Title + "…"
		return m, renderViewCmd(m.modCtx, m.mvRef, st.gen)
	case "up", "k":
		st.vp.ScrollUp(1)
	case "down", "j":
		st.vp.ScrollDown(1)
	case "pgup", "b":
		st.vp.HalfPageUp()
	case "pgdown", "f", " ":
		st.vp.HalfPageDown()
	case "g":
		st.vp.GotoTop()
	case "G":
		st.vp.GotoBottom()
	}
	return m, nil
}

// viewModuleView renders the page: the tab strip when the view is a tab, a
// title bar when transient; scrollable body; hint footer.
func (m Model) viewModuleView() string {
	st := m.mvStates[viewKey(m.mvRef)]
	body := ""
	title := m.mvRef.view.Title
	if st != nil {
		body = st.vp.View()
		if st.title != "" {
			title = st.title
		}
	}
	var header string
	if _, ok := m.tabIdx[viewKey(m.mvRef)]; ok {
		header = m.viewTabBar(m.mvRef.mod.Name)
	} else {
		header = headerStyle.Width(m.width).Render("▤ " + title + "  · " + m.mvRef.mod.Name)
	}
	box := paneFocused.Width(m.width - 2).Height(m.bodyH() - 2).Render(body)
	footer := footerStyle.Width(m.width).Render(hintFor(m.registry, scrModView))
	return header + "\n" + box + "\n" + footer
}
