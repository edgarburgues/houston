package tui

// The tab shell: everything the TUI shows is a tab — Missions and Accounts
// always, plus one tab per module view promoted with "tab": true in its
// manifest. Digits 1-9 jump, [ and ] cycle, from every screen (dispatched
// before the screen handlers). Each tab retains its state (cursor, scroll,
// last render) so switching never re-executes a module handler; a module
// tab fetches once on first activation and again only on r.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"houston/internal/accounts"
	"houston/internal/usage"
)

type tabKind int

const (
	tabMissions tabKind = iota
	tabAccounts
	tabModView
)

// tabRef is one entry of the tab strip. title is fixed for the core tabs;
// module tabs take their live title from the view's retained state.
type tabRef struct {
	kind  tabKind
	title string
	ref   moduleViewRef // when kind == tabModView
}

// buildTabs assembles the strip: the two core tabs plus every accepted view
// with the tab flag, in accepted (module-lexicographic) order. The returned
// index maps a view's state key to its tab so its missions key can jump to
// the tab instead of opening a transient page.
func buildTabs(views []moduleViewRef) ([]tabRef, map[string]int) {
	tabs := []tabRef{{kind: tabMissions, title: "Missions"}, {kind: tabAccounts, title: "Accounts"}}
	idx := map[string]int{}
	for _, r := range views {
		if !r.view.Tab {
			continue
		}
		idx[viewKey(r)] = len(tabs)
		tabs = append(tabs, tabRef{kind: tabModView, ref: r})
	}
	return tabs, idx
}

// updateGlobalKeys dispatches the tab-switching keys before any screen
// handler. The bool reports whether the key was consumed; digits without a
// tab are consumed no-ops (the keys are reserved on every screen either way).
func (m Model) updateGlobalKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	switch msg.String() {
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		n := int(msg.String()[0]-'0') - 1
		if n < len(m.tabs) {
			nm, cmd := m.switchTab(n)
			return nm, cmd, true
		}
		m.pendingDelete = "" // reserved no-op still disarms the two-step delete
		return m, nil, true
	case "[":
		nm, cmd := m.switchTab((m.tabCur - 1 + len(m.tabs)) % len(m.tabs))
		return nm, cmd, true
	case "]":
		nm, cmd := m.switchTab((m.tabCur + 1) % len(m.tabs))
		return nm, cmd, true
	}
	return m, nil, false
}

// seedHint replaces the footer with a screen hint only when nothing real is
// showing: the ready line, another hint and transient prompts are fair game,
// but errors, warnings and action results survive tab switches — a hint must
// never destroy a message the user hasn't read (the F5 notices ring will
// archive these properly).
func (m *Model) seedHint(screen string) {
	s := m.status
	overwritable := s == "" ||
		strings.HasPrefix(s, "Houston ready") ||
		strings.HasPrefix(s, "delete account ") ||
		s == hintFor(m.registry, scrMissions) ||
		s == hintFor(m.registry, scrAccounts) ||
		s == hintFor(m.registry, scrModView)
	if overwritable {
		m.status = hintFor(m.registry, screen)
	}
}

// switchTab activates a tab. Core tabs keep their existing state (Accounts
// loads and probes on first activation, and again after a claude session
// invalidated the figures); module tabs reuse their retained render and
// fetch only when they have none. Selecting the current tab is idempotent
// except that it leaves a transient module view, which is exactly what a
// tab key should do there.
func (m Model) switchTab(n int) (tea.Model, tea.Cmd) {
	if n < 0 || n >= len(m.tabs) {
		return m, nil
	}
	m.pendingDelete = "" // leaving (or re-picking) a tab disarms the delete
	m.tabCur = n
	switch t := m.tabs[n]; t.kind {
	case tabMissions:
		m.screen = screenMissions
		m.seedHint(scrMissions)
	case tabAccounts:
		m.screen = screenAccounts
		m.seedHint(scrAccounts)
		if !m.accsSeen || m.accStale {
			first := !m.accsSeen
			m.accsSeen, m.accStale = true, false
			m.accs, _ = accounts.Load()
			if first {
				m.accCur = 0
				m.accProbes = map[string]usage.Probe{}
			}
			if m.accCur >= len(m.accs) {
				m.accCur = len(m.accs) - 1
			}
			if m.accCur < 0 {
				m.accCur = 0
			}
			if len(m.accs) > 0 {
				m.accProbing = true
				return m, probeAccountsCmd(m.accs)
			}
		}
	case tabModView:
		m.screen = screenModuleView
		m.mvRef = t.ref
		m.seedHint(scrModView)
		return m, m.viewFetchCmd(t.ref)
	}
	return m, nil
}

// tabTitle is the strip label: fixed for core tabs, the last rendered title
// for module tabs (so a handler's "My Jira issues (17)" counts live there).
func (m Model) tabTitle(i int) string {
	t := m.tabs[i]
	if t.kind != tabModView {
		return t.title
	}
	if st, ok := m.mvStates[viewKey(t.ref)]; ok && st.title != "" {
		return st.title
	}
	return t.ref.view.Title
}

// viewTabBar renders the one-line strip with right-aligned context info.
// Labels are budgeted against the width segment by segment (a styled string
// must never be cut mid-escape), and the window of rendered tabs is anchored
// on the ACTIVE tab: when the strip overflows, neighbors are added right
// then left while they fit and the cut sides end in an ellipsis — the tab
// the user is on is always visible and highlighted.
func (m Model) viewTabBar(info string) string {
	brand := "🚀 Houston "
	avail := m.width - lipgloss.Width(brand)
	labels := make([]string, len(m.tabs))
	widths := make([]int, len(m.tabs))
	for i := range m.tabs {
		labels[i] = fmt.Sprintf(" %d %s ", i+1, clip(m.tabTitle(i), 20))
		widths[i] = lipgloss.Width(labels[i])
	}
	// marks is the width the ellipses need for a candidate window.
	marks := func(lo, hi int) int {
		n := 0
		if lo > 0 {
			n++
		}
		if hi < len(m.tabs)-1 {
			n++
		}
		return n
	}
	lo, hi := m.tabCur, m.tabCur
	used := widths[m.tabCur]
	for {
		if hi+1 < len(m.tabs) && used+1+widths[hi+1]+marks(lo, hi+1) <= avail {
			hi++
			used += 1 + widths[hi]
			continue
		}
		if lo > 0 && used+1+widths[lo-1]+marks(lo-1, hi) <= avail {
			lo--
			used += 1 + widths[lo]
			continue
		}
		break
	}
	var b strings.Builder
	b.WriteString(tabBrandStyle.Render(brand))
	rendered := lipgloss.Width(brand)
	if lo > 0 {
		b.WriteString(dimStyle.Render("…"))
		rendered++
	}
	for i := lo; i <= hi; i++ {
		if i > lo {
			b.WriteString(dimStyle.Render("│"))
			rendered++
		}
		label := labels[i]
		if widths[i] > avail { // degenerate: even the active label alone overflows
			label = clip(label, avail)
		}
		if i == m.tabCur {
			b.WriteString(tabActiveStyle.Render(label))
		} else {
			b.WriteString(tabInactiveStyle.Render(label))
		}
		rendered += lipgloss.Width(label)
	}
	if hi < len(m.tabs)-1 {
		b.WriteString(dimStyle.Render("…"))
		rendered++
	}
	if info != "" {
		if gap := m.width - rendered - lipgloss.Width(info) - 1; gap >= 1 {
			b.WriteString(strings.Repeat(" ", gap) + dimStyle.Render(info) + " ")
		}
	}
	return b.String()
}
