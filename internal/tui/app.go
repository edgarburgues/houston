// Package tui is Houston's terminal UI: a three-pane mission-control console
// (programs | missions | preview) over every discovered projects root (shared
// store + per-account dirs), deduped by real path, plus an accounts screen.
package tui

import (
	"context"
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
	"houston/internal/module"
	"houston/internal/resume"
	"houston/internal/store"
	"houston/internal/theme"
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
	lay       theme.Layout

	// modules
	mods         []module.Module            // enabled, loaded once in New, immutable
	modActions   map[string]moduleActionRef // "missions:J" → the winning module action
	warned       map[string]bool            // once-per-module-per-session passive-surface warnings
	helpMissions string                     // help footers with module actions appended, built once
	helpAccounts string

	// module transforms + preview sections (see modtransforms.go)
	modPatches     map[string]module.Patch     // merged presentation patches by mission key
	xformGen       int                         // generation of the newest transform dispatch
	xformStop      context.CancelFunc          // cancels the in-flight transform run; never nil
	initXform      tea.Cmd                     // startup transform dispatch, built in New
	xformFails     map[string]int              // consecutive transform failures per module
	prevCache      map[string][]module.Section // preview sections by mission key
	lastPreviewKey string                      // selection-identity tracker for the preview fetch

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

func New(root string, rescan func() ([]model.Mission, error), st *store.Store, missions []model.Mission, mods []module.Module) Model {
	ti := textinput.New()
	ti.Prompt = ""
	m := Model{
		root:       root,
		rescan:     rescan,
		st:         st,
		missions:   missions,
		input:      ti,
		focus:      focusMid,
		status:     "Houston ready · / search · enter resume · ? help",
		lay:        curLayout,
		mods:       mods,
		warned:     map[string]bool{},
		modPatches: map[string]module.Patch{},
		xformFails: map[string]int{},
		prevCache:  map[string][]module.Section{},
	}
	// Transform gen bookkeeping starts HERE, not in Init: Init has a value
	// receiver and Bubble Tea keeps the model passed to NewProgram, so any
	// mutation Init made to its copy would be discarded — the initial reply
	// would be dropped as stale and xformStop would still be nil when the
	// first rescan fires. xformStop is therefore always non-nil.
	m.xformGen = 1
	ctx, cancel := context.WithCancel(context.Background())
	m.xformStop = cancel
	if hasTransforms(mods) {
		m.initXform = transformCmd(ctx, mods, m.xformGen, module.ProjectRows(missions, st))
	}
	refs, accepted, warns := buildModActions(mods)
	m.modActions = refs
	m.helpMissions, m.helpAccounts = missionsHelp, accountsHelp
	for _, r := range accepted {
		entry := " · " + keyLabel(r.act.Key) + " " + r.act.Title
		if r.act.Screen == "accounts" {
			m.helpAccounts += entry
		} else {
			m.helpMissions += entry
		}
	}
	if len(warns) > 0 {
		// Dropped actions surface once at startup; ls/doctor show them too.
		m.status = warns[0]
		if len(warns) > 1 {
			m.status += fmt.Sprintf(" (+%d more)", len(warns)-1)
		}
	}
	return m
}

// Init returns the startup transform dispatch stashed by New. Value
// receiver: any mutation here would be lost (Bubble Tea keeps the model it
// was constructed with), so the gen bookkeeping must never live in Init.
func (m Model) Init() tea.Cmd { return m.initXform }

// ---- styling ----

// The palette and derived styles are package-level vars rebuilt exactly once
// by applyTheme, before tea.NewProgram runs: the ~30 render call sites stay
// untouched and reads are race-free because no frame exists yet. init seeds
// them from theme.Default(), so tests (and anything else that never calls
// Run) render exactly the pre-theme output.
var (
	cBlue, cGrey, cDim, cGreen, cYellow, cBg lipgloss.Color

	headerStyle, footerStyle, paneFocused, paneBlurred lipgloss.Style
	selStyle, dimStyle, titleStyle, keyStyle           lipgloss.Style
	labelStyle, valStyle, pinStyle, tagStyle           lipgloss.Style

	// curLayout is copied by New into Model.lay, so pane math never reads
	// mutable package state after startup.
	curLayout theme.Layout
)

func init() { applyTheme(theme.Default()) }

func applyTheme(t theme.Theme) {
	cBlue = lipgloss.Color(t.Colors.Accent)
	cGrey = lipgloss.Color(t.Colors.Grey)
	cDim = lipgloss.Color(t.Colors.Dim)
	cGreen = lipgloss.Color(t.Colors.Green)
	cYellow = lipgloss.Color(t.Colors.Yellow)
	cBg = lipgloss.Color(t.Colors.SelBg)

	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(cBlue).Padding(0, 1)
	footerStyle = lipgloss.NewStyle().Foreground(cGrey)
	paneFocused = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cBlue)
	paneBlurred = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cDim)
	selStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(cBg).Bold(true)
	dimStyle = lipgloss.NewStyle().Foreground(cDim)
	titleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("231"))
	keyStyle = lipgloss.NewStyle().Foreground(cYellow)
	labelStyle = lipgloss.NewStyle().Foreground(cGrey)
	valStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("231"))
	pinStyle = lipgloss.NewStyle().Foreground(cYellow)
	tagStyle = lipgloss.NewStyle().Foreground(cGreen)

	curLayout = t.Layout
}

// ---- layout helpers ----

// leftW clamps the themed width so a config typo can never squeeze the pane
// unreadable or starve the mission list.
func (m *Model) leftW() int {
	w := m.lay.LeftWidth
	if w < 16 {
		w = 16
	}
	if w > 60 {
		w = 60
	}
	return w
}

func (m *Model) rightW() int {
	w := m.width * m.lay.RightPercent / 100
	if w < m.lay.RightMin {
		w = m.lay.RightMin
	}
	return w
}

func (m *Model) midW() int  { return m.width - m.leftW() - m.rightW() }
func (m *Model) bodyH() int { return m.height - 2 }

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
		{kind: lkAll, label: "◷ All"},
		{kind: lkPinned, label: "★ Pinned"},
		{kind: lkArchived, label: "⌁ Archived"},
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
				ms, ok := byKey[k]
				if !ok || !m.match(ms) {
					continue
				}
				// hide applies in program views too: a hidden mission must
				// not reappear here wearing its patched title and badge.
				// sortKey does NOT — curated membership order is user intent.
				if m.modPatches[ms.Key()].Hide {
					continue
				}
				out = append(out, ms)
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
			if m.modPatches[ms.Key()].Hide {
				continue
			}
			if m.match(ms) {
				out = append(out, ms)
			}
		}
		// Pinned-first is inviolable (explicit user intent beats any module);
		// within each group, module sortKeys order the rows they cover ahead
		// of the rest, which keep the recency order.
		sort.SliceStable(out, func(i, j int) bool {
			pi, pj := m.st.MetaOf(out[i].Key()).Pinned, m.st.MetaOf(out[j].Key()).Pinned
			if pi != pj {
				return pi
			}
			si, sj := m.modPatches[out[i].Key()].SortKey, m.modPatches[out[j].Key()].SortKey
			switch {
			case si != "" && sj != "" && si != sj:
				return si < sj
			case si != "" && sj == "":
				return true
			case sj != "" && si == "":
				return false
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
		m.status = "couldn't save: " + err.Error()
		return true
	}
	return false
}

// ---- update ----

// Update delegates to update and then, as the single choke point, arms the
// module preview fetch — see armPreview for why no key handler could do it.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	nm, ok := next.(Model)
	if !ok {
		return next, cmd
	}
	if tick := nm.armPreview(); tick != nil {
		cmd = tea.Batch(cmd, tick)
	}
	return nm, cmd
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.preview = viewport.New(m.rightW()-2, m.bodyH()-2)
		m.ready = true
		m.refresh()
		return m, nil

	case execDoneMsg:
		if msg.err != nil {
			m.status = "claude failed: " + msg.err.Error()
		} else {
			m.status = "back from claude"
		}
		return m, nil

	case accProbeMsg:
		m.accProbes = map[string]usage.Probe{}
		for _, p := range msg.probes {
			m.accProbes[p.Account.ID] = p
		}
		m.accProbing = false
		return m, nil

	case modActionMsg:
		var cmd tea.Cmd
		if msg.refresh {
			cmd = m.rescanNow()
		}
		// The action's outcome wins the footer over any reindex note: the
		// user asked for it explicitly.
		switch {
		case msg.err != nil:
			m.status = actionFailStatus(msg.mod, msg.id, msg.err)
		case msg.status != "":
			m.status = "[" + msg.mod + "] " + msg.status
		default:
			m.status = "[" + msg.mod + "] " + msg.id + " done"
		}
		return m, cmd

	case xformMsg:
		if msg.gen != m.xformGen {
			return m, nil // a stale reply must never clobber a newer scan
		}
		m.noteModWarnings(msg.warnings)
		if msg.patches != nil { // nil = the run panicked; keep what we have
			m.acceptPatches(msg.patches, msg.warnings)
			m.rebuildMid()
			m.updatePreview()
		}
		return m, nil

	case previewTickMsg:
		// The debounce fired: exec only if the cursor still rests on the key
		// it was armed for, and only if the sections aren't cached already.
		ms, ok := m.selected()
		if !ok || ms.Key() != msg.key {
			return m, nil
		}
		if _, ok := m.prevCache[msg.key]; ok {
			return m, nil
		}
		return m, previewCmd(m.mods, msg.key, module.ProjectRows([]model.Mission{ms}, m.st)[0])

	case modPreviewMsg:
		m.noteModWarnings(msg.warnings)
		if ms, ok := m.selected(); ok && ms.Key() == msg.key {
			m.prevCache[msg.key] = msg.sections
			m.updatePreview()
		}
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
		m.xformStop() // never nil: any in-flight transform dies with the TUI
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
				m.status = "delete account " + a.ID + "? press d/x again to confirm"
				return m, nil
			}
			m.pendingDelete = ""
			if err := accounts.Remove(a.ID); err != nil {
				m.status = "couldn't delete: " + err.Error()
				return m, nil
			}
			m.accs, _ = accounts.Load()
			if m.accCur >= len(m.accs) {
				m.accCur = len(m.accs) - 1
			}
			if m.accCur < 0 {
				m.accCur = 0
			}
			m.status = "account removed: " + a.ID
		}
	case "enter":
		if a, ok := m.curAccount(); ok {
			accounts.TouchUse(a.ID, accounts.Now())
			cmd := launch.Cmd(a.ResolveConfigDir(), nil, "")
			return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })
		}
	}
	// Module actions route after the switch: built-in keys can never reach
	// here because colliding actions were dropped when the model was built.
	if ref, ok := m.modActions["accounts:"+key]; ok {
		return m.runModuleAction(ref)
	}
	return m, nil
}

func (m Model) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.xformStop() // never nil: any in-flight transform dies with the TUI
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
		m.startInput(actSearch, "Search: ")
		m.input.SetValue(m.query)
	case "esc":
		if m.query != "" {
			m.query = ""
			m.refresh()
			m.status = "filter cleared"
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
			m.status = "launching claude --resume " + shortID(ms.ID, 8) + " …"
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
				m.status = "archived/unarchived"
			}
			m.refresh()
		}
	case "t":
		if _, ok := m.selected(); ok {
			m.startInput(actTag, "Tag (empty = remove last): ")
		}
	case "n":
		if ms, ok := m.selected(); ok {
			m.startInput(actNote, "Note: ")
			m.input.SetValue(m.st.MetaOf(ms.Key()).Note)
		}
	case "p":
		if _, ok := m.selected(); ok {
			m.startInput(actAddProgram, "Add to program: ")
		}
	case "P":
		m.startInput(actNewProgram, "New program: ")
	case "x":
		// remove from current program
		if cur, ok := m.curLeft(); ok && cur.kind == lkProgram {
			if ms, ok := m.selected(); ok {
				if !m.noteSaveErr(m.st.RemoveFromProgram(cur.prog, ms.Key())) {
					m.status = "removed from program " + cur.prog
				}
				m.refresh()
			}
		}
	case "e":
		if ms, ok := m.selected(); ok {
			out := filepath.Join(store.Dir(), "exports", safeName(ms.Title, ms.ID)+".md")
			if p, err := export.Mission(ms, out); err != nil {
				m.status = "export failed: " + err.Error()
			} else {
				m.status = "exported → " + p
			}
		}
	case "r":
		return m, m.rescanNow()
	}
	// Module actions route after the switch: built-in keys can never reach
	// here because colliding actions were dropped when the model was built.
	if ref, ok := m.modActions["missions:"+msg.String()]; ok {
		return m.runModuleAction(ref)
	}
	return m, nil
}

// rescanNow re-runs the mission scan and rebuilds every pane — the r key's
// path, reused by refresh-after-action. It returns the module transform
// re-dispatch for the new scan set (nil without transform modules). The
// preview cache dies with the scan, and lastPreviewKey resets so the
// end-of-Update check re-fires even when the selected key is unchanged.
func (m *Model) rescanNow() tea.Cmd {
	if m.rescan == nil {
		return nil
	}
	ms, err := m.rescan()
	if err != nil {
		m.status = "reindex failed: " + err.Error()
		return nil
	}
	m.missions = ms
	m.prevCache = map[string][]module.Section{}
	m.lastPreviewKey = ""
	m.refresh()
	m.status = fmt.Sprintf("reindexed: %d missions", len(ms))
	return m.dispatchTransforms()
}

func (m Model) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.act = actNone
		m.input.Blur()
		if m.prompt == "Search: " {
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
					m.status = "added to " + val
				}
			}
		case actNewProgram:
			if val != "" {
				if !m.noteSaveErr(m.st.CreateProgram(val, "")) {
					m.status = "program created: " + val
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
		m.preview.SetContent(dimStyle.Render("No mission selected."))
		return
	}
	meta := m.st.MetaOf(ms.Key())
	var b strings.Builder
	row := func(k, v string) {
		b.WriteString(labelStyle.Render(fmt.Sprintf("%-9s", k)) + " " + valStyle.Render(v) + "\n")
	}
	b.WriteString(titleStyle.Bold(true).Render(ms.Title) + "\n\n")
	row("ID", ms.ID)
	row("Project", ms.Project)
	row("cwd", ms.Cwd)
	if ms.LastCwd != "" && ms.LastCwd != ms.Cwd {
		row("Worked in", ms.LastCwd)
	}
	row("Branch", ms.GitBranch)
	row("Version", ms.Version)
	if !ms.FirstTime.IsZero() {
		row("Period", ms.FirstTime.Local().Format("06-01-02 15:04")+" → "+ms.LastTime.Local().Format("06-01-02 15:04"))
	}
	row("Messages", fmt.Sprintf("%d (👤%d 🤖%d)", ms.MessageCount(), ms.UserMsgs, ms.AssistantMsgs))
	row("Tool calls", fmt.Sprintf("%d", ms.ToolCalls()))
	row("Size", fmt.Sprintf("%.1f MB", float64(ms.SizeBytes)/(1024*1024)))
	if ms.HasSubagents {
		row("Subagents", "yes")
	}
	if top := topTools(ms.Tools, 6); top != "" {
		row("Top tools", top)
	}
	if len(meta.Tags) > 0 {
		b.WriteString("\n" + tagStyle.Render("#"+strings.Join(meta.Tags, "  #")) + "\n")
	}
	if meta.Note != "" {
		b.WriteString("\n" + labelStyle.Render("Note: ") + valStyle.Render(meta.Note) + "\n")
	}
	if ms.FirstPrompt != "" {
		b.WriteString("\n" + labelStyle.Render("▸ First message") + "\n" + dimStyle.Render(clipMulti(ms.FirstPrompt, 600)) + "\n")
	}
	if ms.LastPrompt != "" {
		b.WriteString("\n" + labelStyle.Render("▸ Last message") + "\n" + dimStyle.Render(clipMulti(ms.LastPrompt, 600)) + "\n")
	}
	// Module preview sections come from the cache, never from an exec here:
	// updatePreview runs on the event loop and must stay synchronous. The
	// fetch is armed at the end of Update (armPreview) and lands in prevCache
	// through modPreviewMsg.
	for _, sec := range m.prevCache[ms.Key()] {
		head := "▸ " + sec.Module
		if sec.Title != "" {
			head += ": " + sec.Title
		}
		b.WriteString("\n" + labelStyle.Render(head) + "\n" + valStyle.Render(clipMulti(sec.Body, 1200)) + "\n")
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
		return "loading Houston…"
	}
	if m.screen == screenAccounts {
		return m.viewAccounts()
	}
	header := headerStyle.Width(m.width).Render(fmt.Sprintf("🚀 Houston   %d missions   ·   %d programs   ·   [A] accounts", len(m.missions), len(m.st.Programs)))

	left := m.viewLeft()
	mid := m.viewMid()
	right := m.viewPreview()
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, mid, right)

	var footer string
	if m.act != actNone {
		footer = keyStyle.Render(m.prompt) + m.input.View()
	} else if m.showHelp {
		footer = footerStyle.Render(clip(m.helpMissions, m.width))
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
		patch := m.modPatches[ms.Key()]
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
		// Title substitution and the badge happen on PLAIN text before the
		// clip — patches are ANSI-stripped and clamped at merge time, so a
		// module can never inject an escape sequence into the pane.
		title := ms.Title
		if patch.HasTitle {
			title = patch.Title
		}
		badge := ""
		if patch.Badge != "" {
			badge = " [" + patch.Badge + "]"
		}
		// Clip the plain title FIRST and style each segment afterwards: clip()
		// counts runes, so a pre-styled string would get its ANSI escapes counted
		// — and a cut mid-sequence bleeds color into the rest of the pane. The
		// tag mark and badge are budgeted in so the row never overflows the
		// pane and wraps; a pane too narrow for the badge drops the badge.
		room := w - len([]rune(pin+" "+date+" "+id+"  "))
		if len([]rune(badge))+len([]rune(tag)) > room {
			badge = ""
		}
		title = clip(title, room-len([]rune(tag))-len([]rune(badge)))
		if i == m.midCur && m.focus == focusMid {
			lines = append(lines, selStyle.Width(w).Render(pin+" "+date+" "+id+"  "+title+badge+tag))
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
		if badge != "" {
			badge = tagStyle.Render(badge)
		}
		lines = append(lines, pin+" "+dimStyle.Render(date)+" "+id+"  "+title+badge+tag)
	}
	if len(m.mid) == 0 {
		lines = append(lines, dimStyle.Render("(no missions)"))
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
	header := headerStyle.Width(m.width).Render(fmt.Sprintf("🚀 Houston · Accounts (%d)   ·   [esc] back", len(m.accs)))
	w := m.width - 2
	h := m.bodyH() - 2
	var lines []string
	if len(m.accs) == 0 {
		lines = append(lines,
			dimStyle.Render("No accounts yet."),
			"",
			labelStyle.Render("To add an account:"),
			"  "+keyStyle.Render("1) houston account add <label>"),
			"  "+keyStyle.Render("2) houston run")+dimStyle.Render("   (the first time it will /login in the browser)"),
		)
	} else {
		for i, a := range m.accs {
			var press string
			p, has := m.accProbes[a.ID]
			switch {
			case m.accProbing:
				press = dimStyle.Render("probing…")
			case has && p.OK:
				press = fmt.Sprintf("pressure %3.0f%%  (5h %.0f%% · 7d %.0f%%)", p.Pressure, p.U5, p.U7)
			case has:
				press = dimStyle.Render("no usage (" + p.Err + ")")
			default:
				press = dimStyle.Render("—")
			}
			last := a.LastUse
			if last == "" {
				last = "never"
			} else if len(last) >= 10 {
				last = last[:10]
			}
			raw := fmt.Sprintf("%-18s %-24s  %s", clip(a.ID, 18), clip(a.Label, 24), press)
			if i == m.accCur {
				lines = append(lines, selStyle.Width(w).Render(clip(raw, w)))
			} else {
				lines = append(lines, clip(raw, w)+dimStyle.Render("   last:"+last))
			}
		}
	}
	body := paneFocused.Width(w).Height(h).Render(padBox(lines, h))
	footer := footerStyle.Render(clip(m.helpAccounts, m.width))
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// Run boots the program. th is resolved by the caller (defaults merged with
// config.json); applyTheme must run before tea.NewProgram so the one-time
// style mutation can never race a render. mods are the enabled modules, in
// lexicographic name order (module.LoadEnabled).
func Run(root string, rescan func() ([]model.Mission, error), st *store.Store, missions []model.Mission, th theme.Theme, mods []module.Module) error {
	applyTheme(th)
	// Crash backstops: stale interactive-action envelopes and orphaned
	// install staging dirs are swept once per TUI start.
	module.SweepTmp()
	module.SweepStaging()
	p := tea.NewProgram(New(root, rescan, st, missions, mods), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
