// Package tui is Houston's terminal UI: a three-pane mission-control console
// (programs | missions | preview) over every discovered projects root (shared
// store + per-account dirs), deduped by real path, plus an accounts screen.
package tui

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"houston/internal/accounts"
	"houston/internal/export"
	"houston/internal/launch"
	"houston/internal/model"
	"houston/internal/resume"
	"houston/internal/store"
	"houston/internal/usage"
)

type focus int

const (
	focusLeft focus = iota
	focusMid
)

type screen int

const (
	screenMissions screen = iota
	screenAccounts
)

type action int

const (
	actNone action = iota
	actSearch
	actTag
	actNote
	actAddProgram
	actNewProgram
)

type leftKind int

const (
	lkAll leftKind = iota
	lkPinned
	lkArchived
	lkProgram
)

type leftItem struct {
	kind  leftKind
	label string
	prog  string // program name when kind == lkProgram
}

// Model is the bubbletea application state.
type Model struct {
	root     string // display label only
	rescan   func() ([]model.Mission, error)
	st       *store.Store
	missions []model.Mission

	left      []leftItem
	leftCur   int
	mid       []model.Mission
	midCur    int
	focus     focus
	preview   viewport.Model
	input     textinput.Model
	act       action
	prompt    string
	query     string
	status    string
	showHelp  bool
	width     int
	height    int
	ready     bool

	// accounts screen
	screen     screen
	accs       []accounts.Account
	accProbes  map[string]usage.Probe
	accCur     int
	accProbing bool
	// pendingDelete arms the two-step account delete: the id whose removal the
	// next d/x press confirms. Any other key disarms it.
	pendingDelete string
}

type accProbeMsg struct{ probes []usage.Probe }

func New(root string, rescan func() ([]model.Mission, error), st *store.Store, missions []model.Mission) Model {
	ti := textinput.New()
	ti.Prompt = ""
	m := Model{
		root:     root,
		rescan:   rescan,
		st:       st,
		missions: missions,
		input:    ti,
		focus:    focusMid,
		status:   "Houston listo · / buscar · enter resume · ? ayuda",
	}
	return m
}

func (m Model) Init() tea.Cmd { return nil }

// ---- styling ----

var (
	cBlue   = lipgloss.Color("39")
	cGrey   = lipgloss.Color("245")
	cDim    = lipgloss.Color("240")
	cGreen  = lipgloss.Color("42")
	cYellow = lipgloss.Color("220")
	cBg     = lipgloss.Color("236")

	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(cBlue).Padding(0, 1)
	footerStyle = lipgloss.NewStyle().Foreground(cGrey)
	paneFocused = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cBlue)
	paneBlurred = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cDim)
	selStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(cBg).Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(cDim)
	titleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("231"))
	keyStyle    = lipgloss.NewStyle().Foreground(cYellow)
	labelStyle  = lipgloss.NewStyle().Foreground(cGrey)
	valStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("231"))
	pinStyle    = lipgloss.NewStyle().Foreground(cYellow)
	tagStyle    = lipgloss.NewStyle().Foreground(cGreen)
)

// ---- layout helpers ----

func (m *Model) leftW() int  { return 26 }
func (m *Model) rightW() int { w := m.width * 40 / 100; if w < 36 { w = 36 }; return w }
func (m *Model) midW() int   { return m.width - m.leftW() - m.rightW() }
func (m *Model) bodyH() int  { return m.height - 2 }

func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = strings.ReplaceAll(s, "\t", " ") // replace in the result too, not just for measuring
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

// shortID safely truncates an id to n bytes. Mission ids aren't guaranteed to be
// 36-char UUIDs (a short legacy/hand-copied filename like "ab.jsonl" yields a
// 2-char id), so a naive id[:n] would panic and take down the whole TUI.
func shortID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---- mission filtering ----

func (m *Model) rebuildLeft() {
	items := []leftItem{
		{kind: lkAll, label: "◷ Todas"},
		{kind: lkPinned, label: "★ Fijadas"},
		{kind: lkArchived, label: "⌁ Archivadas"},
	}
	for _, p := range m.st.Programs {
		items = append(items, leftItem{kind: lkProgram, label: "▸ " + p.Name, prog: p.Name})
	}
	m.left = items
	if m.leftCur >= len(m.left) {
		m.leftCur = len(m.left) - 1
	}
	if m.leftCur < 0 {
		m.leftCur = 0
	}
}

// curLeft returns the selected left-pane item. Keys can arrive before the first
// WindowSizeMsg has built the pane, so callers handle !ok instead of indexing
// an empty slice.
func (m *Model) curLeft() (leftItem, bool) {
	if m.leftCur >= 0 && m.leftCur < len(m.left) {
		return m.left[m.leftCur], true
	}
	return leftItem{}, false
}

func (m *Model) match(ms model.Mission) bool {
	if m.query == "" {
		return true
	}
	q := strings.ToLower(m.query)
	meta := m.st.MetaOf(ms.Key())
	hay := strings.ToLower(ms.Title + " " + ms.Cwd + " " + ms.Project + " " + ms.ID + " " + strings.Join(meta.Tags, " ") + " " + ms.Search)
	for _, term := range strings.Fields(q) {
		if !strings.Contains(hay, term) {
			return false
		}
	}
	return true
}

func (m *Model) rebuildMid() {
	cur, ok := m.curLeft()
	if !ok {
		return
	}
	var out []model.Mission
	switch cur.kind {
	case lkProgram:
		p := m.st.ProgramByName(cur.prog)
		if p != nil {
			byKey := map[string]model.Mission{}
			for _, ms := range m.missions {
				byKey[ms.Key()] = ms
			}
			for _, k := range p.Missions {
				if ms, ok := byKey[k]; ok && m.match(ms) {
					out = append(out, ms)
				}
			}
		}
	default:
		for _, ms := range m.missions {
			meta := m.st.MetaOf(ms.Key())
			switch cur.kind {
			case lkAll:
				if meta.Archived {
					continue
				}
			case lkPinned:
				if !meta.Pinned {
					continue
				}
			case lkArchived:
				if !meta.Archived {
					continue
				}
			}
			if m.match(ms) {
				out = append(out, ms)
			}
		}
		sort.SliceStable(out, func(i, j int) bool {
			pi, pj := m.st.MetaOf(out[i].Key()).Pinned, m.st.MetaOf(out[j].Key()).Pinned
			if pi != pj {
				return pi
			}
			return out[i].LastTime.After(out[j].LastTime)
		})
	}
	m.mid = out
	if m.midCur >= len(m.mid) {
		m.midCur = len(m.mid) - 1
	}
	if m.midCur < 0 {
		m.midCur = 0
	}
}

func (m *Model) refresh() {
	m.rebuildLeft()
	m.rebuildMid()
	m.updatePreview()
}

func (m *Model) selected() (model.Mission, bool) {
	if m.midCur >= 0 && m.midCur < len(m.mid) {
		return m.mid[m.midCur], true
	}
	return model.Mission{}, false
}

// noteSaveErr surfaces a store write failure in the status line instead of
// silently claiming success. Returns true if there was an error.
func (m *Model) noteSaveErr(err error) bool {
	if err != nil {
		m.status = "no pude guardar: " + err.Error()
		return true
	}
	return false
}

// ---- update ----

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.preview = viewport.New(m.rightW()-2, m.bodyH()-2)
		m.ready = true
		m.refresh()
		return m, nil

	case execDoneMsg:
		if msg.err != nil {
			m.status = "claude falló: " + msg.err.Error()
		} else {
			m.status = "volviste de claude"
		}
		return m, nil

	case accProbeMsg:
		m.accProbes = map[string]usage.Probe{}
		for _, p := range msg.probes {
			m.accProbes[p.Account.ID] = p
		}
		m.accProbing = false
		return m, nil

	case tea.KeyMsg:
		if m.act != actNone {
			return m.updateInput(msg)
		}
		if m.screen == screenAccounts {
			return m.updateAccountsKeys(msg)
		}
		return m.updateKeys(msg)
	}
	return m, nil
}

func probeAccountsCmd(accs []accounts.Account) tea.Cmd {
	return func() tea.Msg { return accProbeMsg{usage.ProbeAll(accs, 8*time.Second)} }
}

func (m Model) enterAccounts() (tea.Model, tea.Cmd) {
	m.accs, _ = accounts.Load()
	m.screen = screenAccounts
	m.accCur = 0
	m.accProbes = map[string]usage.Probe{}
	if len(m.accs) > 0 {
		m.accProbing = true
		return m, probeAccountsCmd(m.accs)
	}
	return m, nil
}

func (m Model) curAccount() (accounts.Account, bool) {
	if m.accCur >= 0 && m.accCur < len(m.accs) {
		return m.accs[m.accCur], true
	}
	return accounts.Account{}, false
}

func (m Model) updateAccountsKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key != "d" && key != "x" {
		m.pendingDelete = "" // any other key disarms a pending delete
	}
	switch key {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "esc", "A", "tab":
		m.screen = screenMissions
	case "up", "k":
		if m.accCur > 0 {
			m.accCur--
		}
	case "down", "j":
		if m.accCur < len(m.accs)-1 {
			m.accCur++
		}
	case "r":
		if len(m.accs) > 0 {
			m.accProbing = true
			return m, probeAccountsCmd(m.accs)
		}
	case "d", "x":
		// Two-step: one stray keypress must not drop an account ('x' even means
		// something else on the missions screen).
		if a, ok := m.curAccount(); ok {
			if m.pendingDelete != a.ID {
				m.pendingDelete = a.ID
				m.status = "¿eliminar la cuenta " + a.ID + "? pulsa d/x otra vez para confirmar"
				return m, nil
			}
			m.pendingDelete = ""
			if err := accounts.Remove(a.ID); err != nil {
				m.status = "no pude eliminar: " + err.Error()
				return m, nil
			}
			m.accs, _ = accounts.Load()
			if m.accCur >= len(m.accs) {
				m.accCur = len(m.accs) - 1
			}
			if m.accCur < 0 {
				m.accCur = 0
			}
			m.status = "cuenta eliminada: " + a.ID
		}
	case "enter":
		if a, ok := m.curAccount(); ok {
			accounts.TouchUse(a.ID, accounts.Now())
			cmd := launch.Cmd(a.ResolveConfigDir(), nil, "")
			return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })
		}
	}
	return m, nil
}

func (m Model) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.showHelp = !m.showHelp
	case "A":
		return m.enterAccounts()
	case "tab":
		if m.focus == focusLeft {
			m.focus = focusMid
		} else {
			m.focus = focusLeft
		}
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "left", "h":
		m.focus = focusLeft
	case "right", "l":
		m.focus = focusMid
	case "/":
		m.startInput(actSearch, "Buscar: ")
		m.input.SetValue(m.query)
	case "esc":
		if m.query != "" {
			m.query = ""
			m.refresh()
			m.status = "filtro limpiado"
		}
	case "pgdown", "f":
		m.preview.HalfPageDown()
	case "pgup", "b":
		m.preview.HalfPageUp()
	case "enter":
		if ms, ok := m.selected(); ok {
			cmd, err := resume.Command(ms)
			if err != nil {
				m.status = err.Error()
				return m, nil
			}
			m.status = "lanzando claude --resume " + shortID(ms.ID, 8) + " …"
			return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })
		}
	case "*":
		if ms, ok := m.selected(); ok {
			m.noteSaveErr(m.st.TogglePin(ms.Key()))
			m.refresh()
		}
	case "a":
		if ms, ok := m.selected(); ok {
			if !m.noteSaveErr(m.st.ToggleArchive(ms.Key())) {
				m.status = "archivado/desarchivado"
			}
			m.refresh()
		}
	case "t":
		if _, ok := m.selected(); ok {
			m.startInput(actTag, "Tag (vacío=quitar último): ")
		}
	case "n":
		if ms, ok := m.selected(); ok {
			m.startInput(actNote, "Nota: ")
			m.input.SetValue(m.st.MetaOf(ms.Key()).Note)
		}
	case "p":
		if _, ok := m.selected(); ok {
			m.startInput(actAddProgram, "Añadir a programa: ")
		}
	case "P":
		m.startInput(actNewProgram, "Nuevo programa: ")
	case "x":
		// remove from current program
		if cur, ok := m.curLeft(); ok && cur.kind == lkProgram {
			if ms, ok := m.selected(); ok {
				if !m.noteSaveErr(m.st.RemoveFromProgram(cur.prog, ms.Key())) {
					m.status = "quitada del programa " + cur.prog
				}
				m.refresh()
			}
		}
	case "e":
		if ms, ok := m.selected(); ok {
			out := filepath.Join(store.Dir(), "exports", safeName(ms.Title, ms.ID)+".md")
			if p, err := export.Mission(ms, out); err != nil {
				m.status = "export falló: " + err.Error()
			} else {
				m.status = "exportado → " + p
			}
		}
	case "r":
		if m.rescan != nil {
			if ms, err := m.rescan(); err == nil {
				m.missions = ms
				m.refresh()
				m.status = fmt.Sprintf("reindexado: %d misiones", len(ms))
			} else {
				m.status = "reindex falló: " + err.Error()
			}
		}
	}
	return m, nil
}

func (m Model) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.act = actNone
		m.input.Blur()
		if m.prompt == "Buscar: " {
			// keep query as-is
		}
		return m, nil
	case "enter":
		val := strings.TrimSpace(m.input.Value())
		act := m.act
		m.act = actNone
		m.input.Blur()
		ms, ok := m.selected()
		switch act {
		case actSearch:
			// already applied live
		case actTag:
			if ok {
				if val == "" {
					tags := m.st.MetaOf(ms.Key()).Tags
					var err error
					if len(tags) > 0 {
						err = m.st.RemoveTag(ms.Key(), tags[len(tags)-1])
					}
					m.noteSaveErr(err)
				} else {
					m.noteSaveErr(m.st.AddTag(ms.Key(), val))
				}
			}
		case actNote:
			if ok {
				m.noteSaveErr(m.st.SetNote(ms.Key(), val))
			}
		case actAddProgram:
			if ok && val != "" {
				var err error
				if m.st.ProgramByName(val) == nil {
					err = m.st.CreateProgram(val, "")
				}
				if err == nil {
					err = m.st.AddToProgram(val, ms.Key())
				}
				if !m.noteSaveErr(err) {
					m.status = "añadida a " + val
				}
			}
		case actNewProgram:
			if val != "" {
				if !m.noteSaveErr(m.st.CreateProgram(val, "")) {
					m.status = "programa creado: " + val
				}
			}
		}
		m.refresh()
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.act == actSearch {
		m.query = m.input.Value()
		m.rebuildMid()
		m.updatePreview()
	}
	return m, cmd
}

func (m *Model) startInput(a action, prompt string) {
	m.act = a
	m.prompt = prompt
	m.input.SetValue("")
	m.input.Focus()
}

func (m *Model) moveCursor(d int) {
	if m.focus == focusLeft {
		m.leftCur += d
		if m.leftCur < 0 {
			m.leftCur = 0
		}
		if m.leftCur >= len(m.left) {
			m.leftCur = len(m.left) - 1
		}
		m.midCur = 0
		m.rebuildMid()
		m.updatePreview()
	} else {
		m.midCur += d
		if m.midCur < 0 {
			m.midCur = 0
		}
		if m.midCur >= len(m.mid) {
			m.midCur = len(m.mid) - 1
		}
		m.updatePreview()
	}
}

type execDoneMsg struct{ err error }

// ---- preview ----

func (m *Model) updatePreview() {
	if !m.ready {
		return
	}
	ms, ok := m.selected()
	if !ok {
		m.preview.SetContent(dimStyle.Render("Sin misión seleccionada."))
		return
	}
	meta := m.st.MetaOf(ms.Key())
	var b strings.Builder
	row := func(k, v string) {
		b.WriteString(labelStyle.Render(fmt.Sprintf("%-9s", k)) + " " + valStyle.Render(v) + "\n")
	}
	b.WriteString(titleStyle.Bold(true).Render(ms.Title) + "\n\n")
	row("ID", ms.ID)
	row("Proyecto", ms.Project)
	row("cwd", ms.Cwd)
	if ms.LastCwd != "" && ms.LastCwd != ms.Cwd {
		row("Trabajo en", ms.LastCwd)
	}
	row("Rama", ms.GitBranch)
	row("Versión", ms.Version)
	if !ms.FirstTime.IsZero() {
		row("Periodo", ms.FirstTime.Local().Format("06-01-02 15:04")+" → "+ms.LastTime.Local().Format("06-01-02 15:04"))
	}
	row("Mensajes", fmt.Sprintf("%d (👤%d 🤖%d)", ms.MessageCount(), ms.UserMsgs, ms.AssistantMsgs))
	row("Tool calls", fmt.Sprintf("%d", ms.ToolCalls()))
	row("Tamaño", fmt.Sprintf("%.1f MB", float64(ms.SizeBytes)/(1024*1024)))
	if ms.HasSubagents {
		row("Subagentes", "sí")
	}
	if top := topTools(ms.Tools, 6); top != "" {
		row("Top tools", top)
	}
	if len(meta.Tags) > 0 {
		b.WriteString("\n" + tagStyle.Render("#"+strings.Join(meta.Tags, "  #")) + "\n")
	}
	if meta.Note != "" {
		b.WriteString("\n" + labelStyle.Render("Nota: ") + valStyle.Render(meta.Note) + "\n")
	}
	if ms.FirstPrompt != "" {
		b.WriteString("\n" + labelStyle.Render("▸ Primer mensaje") + "\n" + dimStyle.Render(clipMulti(ms.FirstPrompt, 600)) + "\n")
	}
	if ms.LastPrompt != "" {
		b.WriteString("\n" + labelStyle.Render("▸ Último mensaje") + "\n" + dimStyle.Render(clipMulti(ms.LastPrompt, 600)) + "\n")
	}
	b.WriteString("\n" + dimStyle.Render(resume.Hint(ms)) + "\n")
	m.preview.SetContent(b.String())
}

func clipMulti(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

func topTools(t map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var s []kv
	for k, v := range t {
		s = append(s, kv{k, v})
	}
	sort.Slice(s, func(i, j int) bool { return s[i].v > s[j].v })
	var parts []string
	for i, x := range s {
		if i >= n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s·%d", x.k, x.v))
	}
	return strings.Join(parts, " ")
}

// ---- view ----

func (m Model) View() string {
	if !m.ready {
		return "cargando Houston…"
	}
	if m.screen == screenAccounts {
		return m.viewAccounts()
	}
	header := headerStyle.Width(m.width).Render(fmt.Sprintf("🚀 Houston   %d misiones   ·   %d programas   ·   [A] cuentas", len(m.missions), len(m.st.Programs)))

	left := m.viewLeft()
	mid := m.viewMid()
	right := m.viewPreview()
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, mid, right)

	var footer string
	if m.act != actNone {
		footer = keyStyle.Render(m.prompt) + m.input.View()
	} else if m.showHelp {
		footer = footerStyle.Render(clip("↑↓/jk mover · tab/←→ panel · / buscar · enter resume · * fijar · a archivar · t tag · n nota · p→prog · P nuevo · x quitar · e export · A cuentas · r reindex · q salir", m.width))
	} else {
		footer = footerStyle.Render(clip(m.status, m.width))
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m Model) viewLeft() string {
	w := m.leftW() - 2
	h := m.bodyH() - 2
	var lines []string
	for i, it := range m.left {
		line := clip(it.label, w)
		if i == m.leftCur && m.focus == focusLeft {
			line = selStyle.Width(w).Render(clip(it.label, w))
		} else if i == m.leftCur {
			line = lipgloss.NewStyle().Foreground(cBlue).Render(line)
		}
		lines = append(lines, line)
	}
	content := padBox(lines, h)
	st := paneBlurred
	if m.focus == focusLeft {
		st = paneFocused
	}
	return st.Width(w).Height(h).Render(content)
}

func (m Model) viewMid() string {
	w := m.midW() - 2
	h := m.bodyH() - 2
	var lines []string
	start, end := windowBounds(m.midCur, len(m.mid), h)
	for i := start; i < end; i++ {
		ms := m.mid[i]
		meta := m.st.MetaOf(ms.Key())
		pin := " "
		if meta.Pinned {
			pin = "★"
		}
		date := "     "
		if !ms.LastTime.IsZero() {
			date = ms.LastTime.Local().Format("01-02")
		}
		tag := ""
		if len(meta.Tags) > 0 {
			tag = " #"
		}
		id := shortID(ms.ID, 6)
		// Clip the plain title FIRST and style each segment afterwards: clip()
		// counts runes, so a pre-styled string would get its ANSI escapes counted
		// — and a cut mid-sequence bleeds color into the rest of the pane. The
		// tag mark is budgeted in so the row never overflows the pane and wraps.
		title := clip(ms.Title, w-len([]rune(pin+" "+date+" "+id+"  "))-len([]rune(tag)))
		if i == m.midCur && m.focus == focusMid {
			lines = append(lines, selStyle.Width(w).Render(pin+" "+date+" "+id+"  "+title+tag))
			continue
		}
		if meta.Archived {
			title = dimStyle.Render(title)
		}
		if pin == "★" {
			pin = pinStyle.Render(pin)
		}
		if tag != "" {
			tag = tagStyle.Render(tag)
		}
		lines = append(lines, pin+" "+dimStyle.Render(date)+" "+id+"  "+title+tag)
	}
	if len(m.mid) == 0 {
		lines = append(lines, dimStyle.Render("(sin misiones)"))
	}
	content := padBox(lines, h)
	st := paneBlurred
	if m.focus == focusMid {
		st = paneFocused
	}
	return st.Width(w).Height(h).Render(content)
}

func (m Model) viewPreview() string {
	w := m.rightW() - 2
	h := m.bodyH() - 2
	st := paneBlurred
	return st.Width(w).Height(h).Render(m.preview.View())
}

// padBox pads/truncates a slice of rendered lines to exactly h rows.
func padBox(lines []string, h int) string {
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func windowBounds(cur, n, h int) (int, int) {
	if n <= h {
		return 0, n
	}
	start := cur - h/2
	if start < 0 {
		start = 0
	}
	end := start + h
	if end > n {
		end = n
		start = end - h
	}
	return start, end
}

func safeName(title, id string) string {
	if title == "" {
		return id
	}
	s := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, title)
	s = strings.TrimSpace(s)
	// Truncate by runes, not bytes: a byte slice can split a multi-byte rune
	// (accents, emoji) and yield an invalid-UTF-8 filename.
	if r := []rune(s); len(r) > 50 {
		s = string(r[:50])
	}
	// shortID, not id[:8]: ids aren't guaranteed 36-char UUIDs (a legacy/hand-copied
	// "ab.jsonl" gives a 2-char id), and a raw slice would panic on export.
	return s + "-" + shortID(id, 8)
}

func (m Model) viewAccounts() string {
	header := headerStyle.Width(m.width).Render(fmt.Sprintf("🚀 Houston · Cuentas (%d)   ·   [esc] volver", len(m.accs)))
	w := m.width - 2
	h := m.bodyH() - 2
	var lines []string
	if len(m.accs) == 0 {
		lines = append(lines,
			dimStyle.Render("Sin cuentas todavía."),
			"",
			labelStyle.Render("Para añadir una cuenta:"),
			"  "+keyStyle.Render("1) houston account add <etiqueta>"),
			"  "+keyStyle.Render("2) houston run")+dimStyle.Render("   (la primera vez hará /login en el navegador)"),
		)
	} else {
		for i, a := range m.accs {
			var press string
			p, has := m.accProbes[a.ID]
			switch {
			case m.accProbing:
				press = dimStyle.Render("sondeando…")
			case has && p.OK:
				press = fmt.Sprintf("presión %3.0f%%  (5h %.0f%% · 7d %.0f%%)", p.Pressure, p.U5, p.U7)
			case has:
				press = dimStyle.Render("sin uso (" + p.Err + ")")
			default:
				press = dimStyle.Render("—")
			}
			last := a.LastUse
			if last == "" {
				last = "nunca"
			} else if len(last) >= 10 {
				last = last[:10]
			}
			raw := fmt.Sprintf("%-18s %-24s  %s", clip(a.ID, 18), clip(a.Label, 24), press)
			if i == m.accCur {
				lines = append(lines, selStyle.Width(w).Render(clip(raw, w)))
			} else {
				lines = append(lines, clip(raw, w)+dimStyle.Render("   últ:"+last))
			}
		}
	}
	body := paneFocused.Width(w).Height(h).Render(padBox(lines, h))
	footer := footerStyle.Render(clip("↑↓ mover · enter lanzar sesión · r sondear uso · d/x eliminar · esc volver · q salir", m.width))
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// Run boots the program.
func Run(root string, rescan func() ([]model.Mission, error), st *store.Store, missions []model.Mission) error {
	p := tea.NewProgram(New(root, rescan, st, missions), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
