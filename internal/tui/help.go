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
// the window height available to it. Shared by the renderer and the scroll
// clamp so both always agree on the scroll range.
func (m Model) helpLines() ([]string, int) {
	secs := helpSections(m.registry, m.helpScreen())
	labelW := 0
	for _, s := range secs {
		for _, c := range s.rows {
			if w := lipgloss.Width(c.label); w > labelW {
				labelW = w
			}
		}
	}
	var blocks [][]string
	total := 0
	for _, s := range secs {
		st := helpSectionStyle
		if s.module {
			st = helpModSectionStyle
		}
		lines := []string{st.Render(s.title)}
		for _, c := range s.rows {
			// Pad AFTER styling with the plain label's width, so the ANSI
			// escapes never skew the column alignment.
			pad := strings.Repeat(" ", labelW-lipgloss.Width(c.label))
			lines = append(lines, "  "+keyStyle.Render(c.label)+pad+"  "+c.title)
		}
		blocks = append(blocks, lines)
		total += len(lines) + 1
	}
	// Two balanced columns when the terminal is wide enough; sections never
	// split across columns.
	var content string
	if m.width >= 84 && len(blocks) > 1 {
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
		content = lipgloss.JoinHorizontal(lipgloss.Top, joinBlocks(blocks[:split]), "     ", joinBlocks(blocks[split:]))
	} else {
		content = joinBlocks(blocks)
	}
	lines := strings.Split(content, "\n")
	budget := m.bodyH() - 4 // panel borders + title line + blank line
	winH := len(lines)
	if winH > budget {
		winH = budget - 1 // one row goes to the scroll position line
		if winH < 1 {
			winH = 1
		}
	}
	return lines, winH
}

func joinBlocks(blocks [][]string) string {
	var parts []string
	for _, b := range blocks {
		parts = append(parts, strings.Join(b, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

// updateHelpKeys handles keys while the overlay is open: jk/arrows and
// pgup/pgdn scroll, g/G jump, esc/q/enter/? close — and any other key closes
// the overlay AND redispatches to the screen underneath, so pressing the key
// you just looked up runs it immediately.
func (m Model) updateHelpKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.modCancel()
		return m, tea.Quit
	case "?", "esc", "q", "enter", "backspace":
		m.helpOpen = false
		return m, nil
	case "up", "k":
		m.helpScroll--
	case "down", "j":
		m.helpScroll++
	case "pgup", "b":
		m.helpScroll -= 8
	case "pgdown", "f", " ":
		m.helpScroll += 8
	case "g":
		m.helpScroll = 0
	case "G":
		m.helpScroll = 1 << 20 // clamped below
	default:
		m.helpOpen = false
		return m.update(msg)
	}
	lines, winH := m.helpLines()
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
	lines, winH := m.helpLines()
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
	inner := []string{
		lipgloss.PlaceHorizontal(contentW, lipgloss.Center, helpTitleStyle.Render("Help — "+helpScreenNames[m.helpScreen()])),
		"",
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
