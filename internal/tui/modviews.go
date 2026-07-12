package tui

// Module views: full-screen read-only pages contributed by modules, opened
// from the missions screen by their manifest key. Content is fetched OFF the
// event loop (tea.Cmd goroutine through the hardened Invoke) and stamped with
// a generation so a slow render can never paint over a newer one — the
// xformGen precedent. Keys inside a view: esc/backspace back, r refresh,
// arrows/jk/pgup/pgdn scroll. Built-in missions keys and this module's own
// action keys win conflicts at load time, like actions do.

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"houston/internal/module"
)

// moduleViewRef pairs a view with its owning module for dispatch.
type moduleViewRef struct {
	mod  module.Module
	view module.View
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

// openModuleView switches to the view screen and dispatches its first render.
func (m Model) openModuleView(ref moduleViewRef) (tea.Model, tea.Cmd) {
	m.screen = screenModuleView
	m.mvRef = ref
	m.mvTitle = ref.view.Title
	m.mvGen++
	m.mv.SetContent(dimStyle.Render("loading " + ref.view.Title + "…"))
	m.mv.GotoTop()
	return m, renderViewCmd(m.modCtx, ref, m.mvGen)
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

// onModViewMsg installs a finished render; stale generations are dropped.
func (m Model) onModViewMsg(msg modViewMsg) (tea.Model, tea.Cmd) {
	if m.screen != screenModuleView || msg.gen != m.mvGen {
		return m, nil
	}
	if msg.err != nil {
		m.screen = screenMissions
		m.status = actionFailStatus(msg.mod, msg.id, msg.err)
		return m, nil
	}
	m.mvTitle = msg.title
	if strings.TrimSpace(msg.body) == "" {
		m.mv.SetContent(dimStyle.Render("(the view returned nothing)"))
	} else {
		m.mv.SetContent(msg.body)
	}
	return m, nil
}

// updateModuleViewKeys handles keys while a module view owns the screen.
func (m Model) updateModuleViewKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.modCancel()
		return m, tea.Quit
	case "esc", "backspace":
		m.screen = screenMissions
		return m, nil
	case "r":
		m.mvGen++
		m.mv.SetContent(dimStyle.Render("refreshing " + m.mvRef.view.Title + "…"))
		return m, renderViewCmd(m.modCtx, m.mvRef, m.mvGen)
	case "up", "k":
		m.mv.ScrollUp(1)
	case "down", "j":
		m.mv.ScrollDown(1)
	case "pgup", "b":
		m.mv.HalfPageUp()
	case "pgdown", "f", " ":
		m.mv.HalfPageDown()
	case "g":
		m.mv.GotoTop()
	case "G":
		m.mv.GotoBottom()
	}
	return m, nil
}

// viewModuleView renders the full-screen page: title bar, scrollable body,
// key footer.
func (m Model) viewModuleView() string {
	header := headerStyle.Width(m.width).Render("▤ " + m.mvTitle + "  · " + m.mvRef.mod.Name)
	body := paneFocused.Width(m.width - 2).Height(m.bodyH() - 2).Render(m.mv.View())
	footer := footerStyle.Width(m.width).Render("↑↓/jk scroll · pgup/pgdn · g/G top/bottom · r refresh · esc back · q quit")
	return header + "\n" + body + "\n" + footer
}
