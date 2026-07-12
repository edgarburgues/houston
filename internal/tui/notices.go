package tui

// The notices ring: every user-meaningful outcome the footer ever shows —
// errors, warnings, action results, heal reports — lands here with a
// timestamp, so one message can no longer destroy another (the footer is a
// single line; the ring remembers). The Notices core tab lists the history
// newest first with an unread counter in its strip label. Transient
// progress lines, prompts and hints stay status-only by design.

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type notice struct {
	at   time.Time
	text string
}

// noticesMax bounds the ring; plenty of history for a session, small enough
// to render and hold without thought.
const noticesMax = 200

// note records an outcome: the footer shows it now, the Notices tab keeps
// it. Consecutive duplicates collapse into the newest timestamp (a flapping
// module must not flood the ring).
func (m *Model) note(s string) {
	m.status = s
	if s == "" {
		return
	}
	if n := len(m.notices); n > 0 && m.notices[n-1].text == s {
		m.notices[n-1].at = time.Now()
		return
	}
	m.notices = append(m.notices, notice{at: time.Now(), text: s})
	if over := len(m.notices) - noticesMax; over > 0 {
		m.notices = append([]notice(nil), m.notices[over:]...)
		m.noticesSeen -= over
		if m.noticesSeen < 0 {
			m.noticesSeen = 0
		}
	}
}

// noticesUnread is the strip counter: outcomes recorded since the tab was
// last activated.
func (m Model) noticesUnread() int {
	if n := len(m.notices) - m.noticesSeen; n > 0 {
		return n
	}
	return 0
}

// updateNoticesKeys drives the history page: jk/arrows and pgup/pgdn move
// the window, g/G jump, esc goes home.
func (m Model) updateNoticesKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.modCancel()
		return m, tea.Quit
	case "esc", "backspace":
		m.screen = screenMissions
		m.tabCur = 0
		m.seedHint(scrMissions)
		return m, nil
	case "?":
		m.helpOpen = true
		m.helpScroll = 0
		return m, nil
	case "up", "k":
		m.ntScroll--
	case "down", "j":
		m.ntScroll++
	case "pgup", "b":
		m.ntScroll -= 10
	case "pgdown", "f", " ":
		m.ntScroll += 10
	case "g":
		m.ntScroll = 0
	case "G":
		m.ntScroll = 1 << 20 // clamped below
	}
	if max := len(m.notices) - (m.bodyH() - 2); m.ntScroll > max {
		m.ntScroll = max
	}
	if m.ntScroll < 0 {
		m.ntScroll = 0
	}
	return m, nil
}

// viewNotices renders the history, newest first, timestamps dimmed.
func (m Model) viewNotices() string {
	header := m.viewTabBar(fmt.Sprintf("%d notices", len(m.notices)))
	h := m.bodyH() - 2
	w := m.width - 4
	var lines []string
	if len(m.notices) == 0 {
		lines = append(lines, dimStyle.Render("No notices yet."),
			"",
			dimStyle.Render("Errors, warnings and action results land here as they happen,"),
			dimStyle.Render("so a busy footer can never lose one."))
	} else {
		for i := len(m.notices) - 1; i >= 0; i-- {
			n := m.notices[i]
			ts := n.at.Local().Format("15:04:05")
			lines = append(lines, dimStyle.Render(ts)+" "+clip(n.text, w-lipgloss.Width(ts)-1))
		}
		off := m.ntScroll
		if max := len(lines) - h; off > max {
			off = max
		}
		if off < 0 {
			off = 0
		}
		end := off + h
		if end > len(lines) {
			end = len(lines)
		}
		lines = lines[off:end]
	}
	box := paneFocused.Width(m.width - 2).Height(h).Render(padBox(lines, h))
	footer := footerStyle.Width(m.width).Render(hintFor(m.registry, scrNotices))
	return lipgloss.JoinVertical(lipgloss.Left, header, box, footer)
}

// noticesStrings exists for tests: the ring's texts oldest-first.
func (m Model) noticesStrings() []string {
	out := make([]string, len(m.notices))
	for i, n := range m.notices {
		out[i] = n.text
	}
	return out
}
