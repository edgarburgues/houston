package tui

// The command palette: ":" or ctrl+p opens a centered fuzzy finder over the
// registry — every runnable command of the current screen plus one entry per
// tab, with the key binding shown next to each item. Running an entry
// redispatches its key through the normal handlers (or switches to its tab),
// so the palette can never disagree with what the keys actually do.

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// palItem is one palette row: either a tab jump (tab >= 0) or a registry
// command executed by redispatching its first key.
type palItem struct {
	title  string
	label  string // key label shown next to the title
	module string // owning module for the dim suffix ("" for core)
	key    string // dispatch key when tab < 0
	tab    int    // tab index to switch to, -1 for key commands
}

// paletteItems builds the full candidate list for the current screen: tabs
// first, then the screen's commands and the globals — minus pure navigation
// (noise in a finder) and the palette itself.
func (m Model) paletteItems() []palItem {
	var out []palItem
	for i := range m.tabs {
		out = append(out, palItem{title: "tab: " + m.tabTitle(i), label: fmt.Sprintf("%d", i+1), tab: i})
	}
	screen := m.helpScreen()
	for _, c := range m.registry {
		if c.screen != screen && c.screen != scrGlobal {
			continue
		}
		if c.category == "Navigate" || c.category == "Tabs" || c.title == "command palette" {
			continue
		}
		out = append(out, palItem{title: c.title, label: c.label, module: c.module, key: c.keys[0], tab: -1})
	}
	return out
}

// fuzzyScore is a case-insensitive subsequence match; lower scores rank
// higher (earlier, tighter matches win). ok is false when query is not a
// subsequence of text.
func fuzzyScore(query, text string) (int, bool) {
	q := []rune(strings.ToLower(query))
	if len(q) == 0 {
		return 0, true
	}
	t := []rune(strings.ToLower(text))
	score, ti := 0, 0
	for _, qr := range q {
		found := false
		for ; ti < len(t); ti++ {
			if t[ti] == qr {
				score += ti
				ti++
				found = true
				break
			}
		}
		if !found {
			return 0, false
		}
	}
	return score, true
}

// palMatches filters and ranks the candidates against the query.
func (m Model) palMatches() []palItem {
	items := m.paletteItems()
	query := m.palInput.Value()
	if strings.TrimSpace(query) == "" {
		return items
	}
	type scored struct {
		it    palItem
		score int
	}
	var hits []scored
	for _, it := range items {
		hay := it.title + " " + it.module
		if s, ok := fuzzyScore(query, hay); ok {
			hits = append(hits, scored{it, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score < hits[j].score })
	out := make([]palItem, len(hits))
	for i, h := range hits {
		out[i] = h.it
	}
	return out
}

// openPalette arms the finder (and closes the overlay — they never stack).
func (m *Model) openPalette() {
	m.helpOpen = false
	m.palOpen = true
	m.palSel = 0
	m.palInput.SetValue("")
	m.palInput.Focus()
}

// keyMsgFor synthesizes the tea.KeyMsg whose String() equals the registry
// key, so a palette run is byte-identical to pressing the binding.
func keyMsgFor(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	}
	if rest, ok := strings.CutPrefix(k, "ctrl+"); ok && len(rest) == 1 && rest[0] >= 'a' && rest[0] <= 'z' {
		return tea.KeyMsg{Type: tea.KeyType(rest[0] - 'a' + 1)}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

// updatePaletteKeys drives the finder: type to filter, arrows (or ctrl+n/p)
// to select, enter runs, esc closes. Everything else lands in the input.
func (m Model) updatePaletteKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.modCancel()
		return m, tea.Quit
	case "esc":
		m.palOpen = false
		m.palInput.Blur()
		return m, nil
	case "enter":
		matches := m.palMatches()
		if m.palSel < 0 || m.palSel >= len(matches) {
			return m, nil
		}
		it := matches[m.palSel]
		m.palOpen = false
		m.palInput.Blur()
		if it.tab >= 0 {
			return m.switchTab(it.tab)
		}
		return m.update(keyMsgFor(it.key))
	case "up", "ctrl+p":
		if m.palSel > 0 {
			m.palSel--
		}
		return m, nil
	case "down", "ctrl+n":
		if m.palSel < len(m.palMatches())-1 {
			m.palSel++
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.palInput, cmd = m.palInput.Update(msg)
	// The result set changed under the cursor: snap back to the best match.
	m.palSel = 0
	return m, cmd
}

// viewPalette renders the finder over a quiet frame: input on top, ranked
// matches below, selection highlighted, key labels on the right.
func (m Model) viewPalette() string {
	w := 64
	if w > m.width-4 {
		w = m.width - 4
	}
	if w < 20 {
		w = 20
	}
	inner := w - 6 // border + padding
	matches := m.palMatches()
	sel := m.palSel
	if sel >= len(matches) {
		sel = len(matches) - 1
	}
	if sel < 0 {
		sel = 0
	}
	maxRows := m.bodyH() - 7
	if maxRows > 12 {
		maxRows = 12
	}
	if maxRows < 1 {
		maxRows = 1
	}
	start, end := windowBounds(sel, len(matches), maxRows)
	lines := []string{m.palInput.View(), ""}
	for i := start; i < end; i++ {
		it := matches[i]
		title := it.title
		if it.module != "" {
			title += "  · " + it.module
		}
		lw := lipgloss.Width(it.label)
		t := clip(title, inner-lw-2)
		gap := inner - lipgloss.Width(t) - lw
		if gap < 1 {
			gap = 1
		}
		if i == sel {
			lines = append(lines, selStyle.Width(inner).Render(t+strings.Repeat(" ", gap)+it.label))
		} else {
			lines = append(lines, t+strings.Repeat(" ", gap)+keyStyle.Render(it.label))
		}
	}
	if len(matches) == 0 {
		lines = append(lines, dimStyle.Render("(no matching command)"))
	} else if len(matches) > maxRows {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("%d of %d", sel+1, len(matches))))
	}
	panel := helpBorderStyle.Width(w - 2).Render(strings.Join(lines, "\n"))
	header := headerStyle.Width(m.width).Render("🚀 Houston · Palette")
	body := lipgloss.Place(m.width, m.bodyH(), lipgloss.Center, lipgloss.Position(0.2), panel)
	footer := footerStyle.Render(clip("enter run · ↑↓ select · esc close", m.width))
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}
