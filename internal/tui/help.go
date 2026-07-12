package tui

// The which-key help overlay: a centered, grouped, scrollable panel
// generated from the command registry — the fix for help outgrowing a
// one-line footer once modules started contributing keys. Core categories
// come first in declaration order, then one section per module in name
// order. Any key that isn't overlay navigation closes the panel and runs
// its binding (which-key behavior), so help doubles as a launcher.

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// helpScreen maps the model's current screen to its registry id.
func (m Model) helpScreen() string {
	switch m.screen {
	case screenAccounts:
		return scrAccounts
	case screenModuleView:
		return scrModView
	default:
		return scrMissions
	}
}

// helpScreenNames titles the overlay per screen.
var helpScreenNames = map[string]string{
	scrMissions: "Missions",
	scrAccounts: "Accounts",
	scrModView:  "Module view",
}

type helpSection struct {
	title  string
	module bool // module sections render their title in the module color
	rows   []command
}

// helpSections groups a screen's commands: global commands (Tabs) and core
// categories in first-appearance order, then module sections sorted by
// module name (a module's actions and views share one section regardless of
// registry order).
func helpSections(reg []command, screen string) []helpSection {
	var out []helpSection
	idx := map[string]int{}
	mods := map[string][]command{}
	var modNames []string
	for _, c := range reg {
		if c.screen != screen && c.screen != scrGlobal {
			continue
		}
		if c.origin == originModule {
			if _, ok := mods[c.module]; !ok {
				modNames = append(modNames, c.module)
			}
			mods[c.module] = append(mods[c.module], c)
			continue
		}
		i, ok := idx[c.category]
		if !ok {
			i = len(out)
			idx[c.category] = i
			out = append(out, helpSection{title: c.category})
		}
		out[i].rows = append(out[i].rows, c)
	}
	sort.Strings(modNames)
	for _, n := range modNames {
		out = append(out, helpSection{title: n, module: true, rows: mods[n]})
	}
	return out
}

// helpLines builds the styled overlay content for the current screen plus
// the window height available to it and whether the panel runs slim (no
// title chrome, for very short terminals). Shared by the renderer and the
// scroll clamp so both always agree on the scroll range.
func (m Model) helpLines() ([]string, int, bool) {
	// The visible view's page actions join the overlay for as long as that
	// view is on screen — they are page-scoped, so the static registry
	// cannot carry them.
	secs := helpSections(append(append([]command{}, m.registry...), m.viewActionCommands()...), m.helpScreen())
	labelW := 0
	for _, s := range secs {
		for _, c := range s.rows {
			if w := lipgloss.Width(c.label); w > labelW {
				labelW = w
			}
		}
	}
	// Titles are clipped so a single row can never outgrow the panel's
	// horizontal budget (borders+padding 6, indent 2, label, gap 2) — module
	// titles run up to 40 runes and must not push the border off screen.
	maxTitle := m.width - 6 - 2 - labelW - 2
	if maxTitle < 8 {
		maxTitle = 8
	}
	var blocks [][]string
	total := 0
	for _, s := range secs {
		st := helpSectionStyle
		if s.module {
			st = helpModSectionStyle
		}
		lines := []string{st.Render(clip(s.title, maxTitle+labelW+2))}
		for _, c := range s.rows {
			// Pad AFTER styling with the plain label's width, so the ANSI
			// escapes never skew the column alignment.
			pad := strings.Repeat(" ", labelW-lipgloss.Width(c.label))
			lines = append(lines, "  "+keyStyle.Render(c.label)+pad+"  "+clip(c.title, maxTitle))
		}
		blocks = append(blocks, lines)
		total += len(lines) + 1
	}
	// Two balanced columns when they actually fit — measured against the
	// terminal, not guessed from a fixed width. Sections never split across
	// columns.
	var content string
	if len(blocks) > 1 && m.width >= 84 {
		split, run := len(blocks), 0
		for i, b := range blocks {
			run += len(b) + 1
			if run*2 >= total {
				split = i + 1
				break
			}
		}
		if split >= len(blocks) {
			split = len(blocks) - 1 // never leave the right column empty
		}
		twoCol := lipgloss.JoinHorizontal(lipgloss.Top, joinBlocks(blocks[:split]), "     ", joinBlocks(blocks[split:]))
		if lipgloss.Width(twoCol) <= m.width-6 {
			content = twoCol
		}
	}
	if content == "" {
		content = joinBlocks(blocks)
	}
	lines := strings.Split(content, "\n")
	slim := m.bodyH() < 9 // drop the title chrome before dropping content
	budget := m.bodyH() - 2
	if !slim {
		budget -= 2 // title line + blank line
	}
	winH := len(lines)
	if winH > budget {
		winH = budget - 1 // one row goes to the scroll position line
		if winH < 1 {
			winH = 1
		}
	}
	return lines, winH, slim
}

func joinBlocks(blocks [][]string) string {
	var parts []string
	for _, b := range blocks {
		parts = append(parts, strings.Join(b, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

// updateHelpKeys handles keys while the overlay is open: jk/arrows scroll a
// line, pgup/pgdn a page, esc/backspace/? close — and EVERY other key closes
// the overlay and redispatches to the screen underneath, so pressing the key
// you just looked up runs it immediately. The navigation set is restricted
// to keys no module can ever claim (j/k are core on every screen, pgup/pgdn
// and esc/backspace are multi-char names outside the manifest key grammar),
// so the which-key contract — every advertised command is runnable from
// here — holds for the whole registry, q and enter included.
func (m Model) updateHelpKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.modCancel()
		return m, tea.Quit
	case "?", "esc", "backspace":
		m.helpOpen = false
		return m, nil
	case "up", "k":
		m.helpScroll--
	case "down", "j":
		m.helpScroll++
	case "pgup":
		m.helpScroll -= 8
	case "pgdown":
		m.helpScroll += 8
	default:
		m.helpOpen = false
		return m.update(msg)
	}
	lines, winH, _ := m.helpLines()
	if max := len(lines) - winH; m.helpScroll > max {
		m.helpScroll = max
	}
	if m.helpScroll < 0 {
		m.helpScroll = 0
	}
	return m, nil
}

// viewHelp renders the overlay frame: header, centered panel, footer.
func (m Model) viewHelp() string {
	lines, winH, slim := m.helpLines()
	maxOff := len(lines) - winH
	if maxOff < 0 {
		maxOff = 0
	}
	// Clamp locally too: a resize while open can shrink the range under the
	// stored offset before the next key event re-clamps it.
	off := m.helpScroll
	if off > maxOff {
		off = maxOff
	}
	if off < 0 {
		off = 0
	}
	end := off + winH
	if end > len(lines) {
		end = len(lines)
	}
	contentW := 0
	for _, l := range lines {
		if w := lipgloss.Width(l); w > contentW {
			contentW = w
		}
	}
	var inner []string
	if !slim {
		inner = append(inner,
			lipgloss.PlaceHorizontal(contentW, lipgloss.Center, helpTitleStyle.Render("Help — "+helpScreenNames[m.helpScreen()])),
			"")
	}
	inner = append(inner, lines[off:end]...)
	if maxOff > 0 {
		inner = append(inner, dimStyle.Render(fmt.Sprintf("%d-%d of %d · j/k scroll", off+1, end, len(lines))))
	}
	panel := helpBorderStyle.Render(strings.Join(inner, "\n"))
	header := headerStyle.Width(m.width).Render("🚀 Houston · Help")
	body := lipgloss.Place(m.width, m.bodyH(), lipgloss.Center, lipgloss.Center, panel)
	footer := footerStyle.Render(clip("press a command key to run it · j/k scroll · esc close", m.width))
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}
