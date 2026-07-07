package module

// This file owns the wire contract: the envelope Houston writes to a
// handler's stdin, the payload projections, and the per-surface run functions
// that exec handlers via Invoke and sanitize their replies. Mission
// projections deliberately exclude Search (a multi-megabyte haystack) and
// Path; titles still carry conversation text (the first-prompt fallback for
// unnamed sessions), a fact the security docs call out.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"houston/internal/accounts"
	"houston/internal/model"
	"houston/internal/store"
)

// HoustonVersion is stamped by main (the same ldflags value `houston version`
// prints) so handlers can gate on it; source builds stay "dev".
var HoustonVersion = "dev"

// Event names, one per surface. New surfaces get new names — the api version
// only bumps for envelope/reply breaks.
const (
	EventAction    = "action.invoke"
	EventTransform = "missions.transform"
	EventPreview   = "preview.append"
	EventSegment   = "statusline.segment"
)

// Envelope is the single JSON object a handler receives on stdin (interactive
// actions read it from $HOUSTON_EVENT_FILE instead). Handlers must ignore
// unknown fields; Settings is the opaque passthrough of the user's
// config.json modules.<name>.settings.
type Envelope struct {
	API      int             `json:"api"`
	Event    string          `json:"event"`
	Module   string          `json:"module"`
	Houston  HoustonInfo     `json:"houston"`
	Settings json.RawMessage `json:"settings,omitempty"`
	Payload  any             `json:"payload"`
}

// HoustonInfo tells a handler who is calling it.
type HoustonInfo struct {
	Version  string `json:"version"`
	OS       string `json:"os"`
	StoreDir string `json:"storeDir"`
}

// NewEnvelope assembles the envelope for one handler exec.
func NewEnvelope(event string, m Module, payload any) Envelope {
	return Envelope{
		API:    1,
		Event:  event,
		Module: m.Name,
		Houston: HoustonInfo{
			Version:  HoustonVersion,
			OS:       runtime.GOOS,
			StoreDir: accounts.StoreDir(),
		},
		Settings: m.Settings,
		Payload:  payload,
	}
}

// MissionRow is the wire projection of a mission. Tags/pinned/archived come
// from the store's Meta, not the Mission itself.
type MissionRow struct {
	Key           string    `json:"key"`
	ID            string    `json:"id"`
	Project       string    `json:"project"`
	Title         string    `json:"title"`
	Cwd           string    `json:"cwd"`
	GitBranch     string    `json:"gitBranch,omitempty"`
	Tags          []string  `json:"tags,omitempty"`
	Pinned        bool      `json:"pinned"`
	Archived      bool      `json:"archived"`
	LastTime      time.Time `json:"lastTime"`
	UserMsgs      int       `json:"userMsgs"`
	AssistantMsgs int       `json:"assistantMsgs"`
}

// AccountRow is the wire projection of an account.
type AccountRow struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	ConfigDir string `json:"configDir"`
	LastUse   string `json:"lastUse,omitempty"`
}

// AccountRowOf projects one account for the wire.
func AccountRowOf(a accounts.Account) AccountRow {
	return AccountRow{ID: a.ID, Label: a.Label, ConfigDir: a.ResolveConfigDir(), LastUse: a.LastUse}
}

// ProjectRows projects missions for the wire. Never copies Mission.Search or
// Mission.Path. A nil store yields empty meta.
func ProjectRows(ms []model.Mission, st *store.Store) []MissionRow {
	rows := make([]MissionRow, 0, len(ms))
	for _, m := range ms {
		var meta model.Meta
		if st != nil {
			meta = st.MetaOf(m.Key())
		}
		rows = append(rows, MissionRow{
			Key:           m.Key(),
			ID:            m.ID,
			Project:       m.Project,
			Title:         m.Title,
			Cwd:           m.Cwd,
			GitBranch:     m.GitBranch,
			Tags:          meta.Tags,
			Pinned:        meta.Pinned,
			Archived:      meta.Archived,
			LastTime:      m.LastTime,
			UserMsgs:      m.UserMsgs,
			AssistantMsgs: m.AssistantMsgs,
		})
	}
	return rows
}

// ActionPayload is the action.invoke payload: the selected mission on the
// missions screen, the selected account on the accounts screen.
type ActionPayload struct {
	Screen  string      `json:"screen"`
	Action  string      `json:"action"`
	Mission *MissionRow `json:"mission,omitempty"`
	Account *AccountRow `json:"account,omitempty"`
}

// TransformPayload is the missions.transform payload: the full deduped scan,
// capped at maxTransformRows most-recent rows.
type TransformPayload struct {
	Generation int          `json:"generation"`
	Truncated  bool         `json:"truncated"`
	Missions   []MissionRow `json:"missions"`
}

// PreviewPayload is the preview.append payload.
type PreviewPayload struct {
	Mission MissionRow `json:"mission"`
}

// maxTransformRows caps the transform payload; the scan arrives most-recent
// first, so the cap drops the oldest tail.
const maxTransformRows = 2000

// Reply size caps per surface. Over cap is a hard failure — truncated JSON
// must never parse.
const (
	CapReply   = 1 << 20 // transforms, previews, non-interactive actions
	CapSegment = 8 << 10 // statusline segments
)

// Patch is one mission's merged presentation override. HasTitle
// distinguishes "title set to empty" from "title untouched" — the wire uses
// pointers for the same reason. Patches are presentation, not identity:
// search still runs on original titles.
type Patch struct {
	Key      string
	Title    string
	Badge    string
	SortKey  string
	Hide     bool
	HasTitle bool
}

type patchWire struct {
	Key     string  `json:"key"`
	Title   *string `json:"title"`
	Badge   *string `json:"badge"`
	SortKey *string `json:"sortKey"`
	Hide    *bool   `json:"hide"`
}

type transformReply struct {
	Patches []patchWire `json:"patches"`
	Notice  string      `json:"notice"`
}

// RunTransforms execs every missions-transform handler sequentially in the
// given (lexicographic) order — each receives the ORIGINAL projection, so a
// broken module cannot poison another's input — and merges the sparse patches
// per key, later-processed module winning per field. Within one module the
// first patch per key wins; keys not present in rows are ignored. The
// returned warnings are footer-ready strings (failures and notices); the
// keep-patches-for-3-failures retention is the caller's business.
func RunTransforms(ctx context.Context, mods []Module, gen int, rows []MissionRow) (map[string]Patch, []string) {
	known := make(map[string]bool, len(rows))
	for _, r := range rows {
		known[r.Key] = true
	}
	payload := TransformPayload{Generation: gen, Missions: rows}
	if len(rows) > maxTransformRows {
		payload.Missions = rows[:maxTransformRows]
		payload.Truncated = true
	}
	merged := map[string]Patch{}
	var warns []string
	for _, m := range mods {
		h := m.Manifest.Transforms.Missions
		if h == nil {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		env := NewEnvelope(EventTransform, m, payload)
		raw, err := Invoke(ctx, m, h.Command, env, CapReply, m.Manifest.ResolveTimeout(SurfaceTransform, h.TimeoutMs))
		if err != nil {
			warns = append(warns, fmt.Sprintf("[%s] transform: %v", m.Name, err))
			continue
		}
		var rep transformReply
		if err := DecodeReply(raw, &rep); err != nil {
			LogEvent(m.Name, EventTransform, err.Error(), nil)
			warns = append(warns, fmt.Sprintf("[%s] transform: %v", m.Name, err))
			continue
		}
		seen := map[string]bool{}
		for _, p := range rep.Patches {
			if !known[p.Key] || seen[p.Key] {
				continue
			}
			seen[p.Key] = true
			cur := merged[p.Key]
			cur.Key = p.Key
			if p.Title != nil {
				cur.Title = CleanLine(*p.Title, 200)
				cur.HasTitle = true
			}
			if p.Badge != nil {
				cur.Badge = CleanLine(*p.Badge, 16)
			}
			if p.SortKey != nil {
				cur.SortKey = *p.SortKey
			}
			if p.Hide != nil {
				cur.Hide = *p.Hide
			}
			merged[p.Key] = cur
		}
		if rep.Notice != "" {
			warns = append(warns, "["+m.Name+"] "+CleanLine(rep.Notice, 120))
		}
	}
	return merged, warns
}

// Section is one module's preview contribution for the selected mission.
type Section struct {
	Module string
	Title  string
	Body   string
}

type previewReply struct {
	Sections []struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	} `json:"sections"`
	Notice string `json:"notice"`
}

// Preview body caps: at most 3 sections per module, 8 KiB of plain text each.
const (
	maxPreviewSections = 3
	maxPreviewBody     = 8 << 10
)

// RunPreviews execs every preview handler for the selected mission and
// returns the sanitized sections plus footer-ready warnings.
func RunPreviews(ctx context.Context, mods []Module, row MissionRow) ([]Section, []string) {
	var secs []Section
	var warns []string
	for _, m := range mods {
		h := m.Manifest.Transforms.Preview
		if h == nil {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		env := NewEnvelope(EventPreview, m, PreviewPayload{Mission: row})
		raw, err := Invoke(ctx, m, h.Command, env, CapReply, m.Manifest.ResolveTimeout(SurfacePreview, h.TimeoutMs))
		if err != nil {
			warns = append(warns, fmt.Sprintf("[%s] preview: %v", m.Name, err))
			continue
		}
		var rep previewReply
		if err := DecodeReply(raw, &rep); err != nil {
			LogEvent(m.Name, EventPreview, err.Error(), nil)
			warns = append(warns, fmt.Sprintf("[%s] preview: %v", m.Name, err))
			continue
		}
		for i, s := range rep.Sections {
			if i >= maxPreviewSections {
				break
			}
			secs = append(secs, Section{
				Module: m.Name,
				Title:  CleanLine(s.Title, 40),
				Body:   cleanBody(s.Body, maxPreviewBody),
			})
		}
		if rep.Notice != "" {
			warns = append(warns, "["+m.Name+"] "+CleanLine(rep.Notice, 120))
		}
	}
	return secs, warns
}

// ActionReply is a non-interactive action's outcome. Refresh is already OR'd
// with the manifest's refreshAfter.
type ActionReply struct {
	Status  string
	Refresh bool
}

type actionReplyWire struct {
	Status  string `json:"status"`
	Refresh bool   `json:"refresh"`
	Notice  string `json:"notice"`
}

// RunAction execs a non-interactive action with the full Invoke hardening.
// An empty reply is valid (generic "done"). The optional notice fills the
// footer when status is empty — status wins, both surface as "[name] …". ctx
// lets the TUI's quit-time root cancel reach an in-flight handler.
func RunAction(ctx context.Context, m Module, a Action, env Envelope) (ActionReply, error) {
	raw, err := Invoke(ctx, m, a.Command, env, CapReply, m.Manifest.ResolveTimeout(SurfaceAction, a.TimeoutMs))
	if err != nil {
		return ActionReply{}, err
	}
	var w actionReplyWire
	if err := DecodeReply(raw, &w); err != nil {
		LogEvent(m.Name, EventAction, err.Error(), nil)
		return ActionReply{}, err
	}
	status := CleanLine(w.Status, 120)
	if status == "" {
		status = CleanLine(w.Notice, 120)
	}
	return ActionReply{Status: status, Refresh: w.Refresh || a.RefreshAfter}, nil
}

// TmpDir holds interactive-action envelope files — inside the store, NOT
// os.TempDir(): envelopes carry conversation-derived titles and settings that
// may hold tokens, and a crash must not strand them somewhere Houston never
// revisits.
func TmpDir() string { return filepath.Join(accounts.StoreDir(), "tmp") }

// ExecAction builds the *exec.Cmd for an interactive action: a PLAIN command
// — nil stdio so tea.ExecProcess attaches the real terminal, no SysProcAttr,
// no timeout, no caps (the Invoke hardening would leave the user staring at a
// frozen alt-screen while a headless handler waits for a console it cannot
// have, and a new process group would swallow its Ctrl-C). The envelope goes
// to a 0600 file named in $HOUSTON_EVENT_FILE; there is no reply protocol —
// the exit code is the result. The returned cleanup removes the envelope and
// must run in the ExecProcess callback; SweepTmp is the crash backstop.
func ExecAction(m Module, a Action, env Envelope) (*exec.Cmd, func(), error) {
	if err := os.MkdirAll(TmpDir(), 0o700); err != nil {
		return nil, nil, err
	}
	f, err := os.CreateTemp(TmpDir(), "envelope-*.json")
	if err != nil {
		return nil, nil, err
	}
	name := f.Name()
	cleanup := func() { os.Remove(name) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return nil, nil, err
	}
	if err := json.NewEncoder(f).Encode(env); err != nil {
		f.Close()
		cleanup()
		return nil, nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return nil, nil, err
	}
	bin, args, err := resolveArgv(m, a.Command)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = m.Dir
	cmd.Env = append(handlerEnv(m, EventAction), "HOUSTON_EVENT_FILE="+name)
	return cmd, cleanup, nil
}

// sweepAge: an envelope older than this belongs to a crashed session — a live
// interactive action from another Houston is younger by construction.
const sweepAge = time.Hour

// SweepTmp removes stale interactive-action envelopes; best-effort, called at
// TUI start.
func SweepTmp() {
	ents, _ := os.ReadDir(TmpDir())
	for _, e := range ents {
		p := filepath.Join(TmpDir(), e.Name())
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > sweepAge {
			os.Remove(p)
		}
	}
}

// SweepStaging removes orphaned install staging dirs; best-effort, called at
// TUI start. Same staleness guard as SweepTmp: a concurrent `module add`
// stages here for seconds and sweeping it would race the installer's rename
// into a silently incomplete module — an orphan from a crashed install is
// old by construction.
func SweepStaging() {
	ents, _ := os.ReadDir(Dir())
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".staging-") {
			continue
		}
		p := filepath.Join(Dir(), e.Name())
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > sweepAge {
			os.RemoveAll(p)
		}
	}
}

// StartupMaintenance is the TUI-start housekeeping: stale interactive-action
// envelopes, orphaned install staging dirs, and the modules.log trim. One
// entry point so a sweep can't silently un-wire from tui.Run.
func StartupMaintenance() {
	SweepTmp()
	SweepStaging()
	TrimLog()
}

// CleanLine sanitizes module text destined for a single line of UI: first
// line only, ANSI escapes and control runes stripped, clamped to maxRunes. A
// module must never inject an escape sequence into a pane or the statusline.
func CleanLine(s string, maxRunes int) string {
	var b strings.Builder
	n := 0
	esc, csi := false, false
	for _, r := range s {
		if esc {
			if csi {
				if r >= 0x40 && r <= 0x7e { // CSI final byte
					esc, csi = false, false
				}
				continue
			}
			if r == '[' {
				csi = true
				continue
			}
			esc = false // single-char escape: swallow the introducer pair
			continue
		}
		if r == '\n' || r == '\r' {
			break
		}
		if n >= maxRunes {
			break
		}
		switch {
		case r == 0x1b:
			esc = true
		case r == '\t':
			b.WriteRune(' ')
			n++
		case r < 0x20 || r == 0x7f:
			// drop
		default:
			b.WriteRune(r)
			n++
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// cleanBody sanitizes multi-line preview text: ANSI and control stripped,
// newlines kept, tabs become two spaces, clamped to maxBytes.
func cleanBody(s string, maxBytes int) string {
	var b strings.Builder
	esc, csi := false, false
	for _, r := range s {
		if b.Len() >= maxBytes {
			break
		}
		if esc {
			if csi {
				if r >= 0x40 && r <= 0x7e {
					esc, csi = false, false
				}
				continue
			}
			if r == '[' {
				csi = true
				continue
			}
			esc = false
			continue
		}
		switch {
		case r == 0x1b:
			esc = true
		case r == '\n':
			b.WriteRune('\n')
		case r == '\t':
			b.WriteString("  ")
		case r == '\r', r < 0x20 && r != '\n', r == 0x7f:
			// drop
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimRight(b.String(), "\n ")
}
