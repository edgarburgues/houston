package module

// `houston module test <name>` is the module-author feedback loop: it runs
// every contribution the manifest declares against synthetic fixtures (or
// live scan data with --live/--mission), through the SAME Invoke policy the
// real surfaces use — hardening, caps and the timeout resolution chain — then
// prints the envelope, the raw stdout/stderr, a field-by-field verdict of the
// reply and the wall time against the timeout. Any failure makes the exit
// code nonzero, so module repos can run it in CI. Interactive actions run for
// real, attached to this terminal with the envelope file: that IS the test.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"houston/internal/accounts"
	"houston/internal/config"
	"houston/internal/scan"
	"houston/internal/store"
)

// TestOpts selects which contributions `module test` runs and what data
// feeds the envelopes.
type TestOpts struct {
	Event   string    // event name or shorthand (action/transform/preview/segment); "" = all
	Live    bool      // real scan/store/accounts data instead of the fixtures
	Mission string    // key of the mission to select (implies Live); default most recent
	Out     io.Writer // report destination; nil = os.Stdout
}

// RunTest implements `houston module test` and returns the process exit
// code: 0 when every contribution passed, 1 otherwise. The module is loaded
// straight from its directory, registry entry or not — testing a module
// BEFORE enabling it is the point of the command.
func RunTest(name string, opts TestOpts) int {
	w := opts.Out
	if w == nil {
		w = os.Stdout
	}
	if err := SafeName(name); err != nil {
		fmt.Fprintln(w, "error:", err)
		return 1
	}
	event, err := canonicalEvent(opts.Event)
	if err != nil {
		fmt.Fprintln(w, "error:", err)
		return 1
	}
	m, err := loadForTest(name)
	if err != nil {
		fmt.Fprintf(w, "error: module %s: %v\n", name, err)
		return 1
	}
	data := fixtureFeed()
	source := "synthetic fixtures — two missions, one account (use --live for real data)"
	if opts.Live || opts.Mission != "" {
		if data, err = liveFeed(opts.Mission); err != nil {
			fmt.Fprintln(w, "error:", err)
			return 1
		}
		source = fmt.Sprintf("live data — %d mission(s), selected %s", len(data.rows), data.mission.Key)
	}
	fmt.Fprintf(w, "testing module %q (%s)\n", m.Name, m.Dir)
	fmt.Fprintln(w, "input: "+source)

	ran, failed := 0, 0
	record := func(ok bool) {
		ran++
		if !ok {
			failed++
		}
	}
	if event == "" || event == EventAction {
		for _, a := range m.Manifest.Actions {
			record(runActionTest(w, m, a, data))
		}
	}
	if h := m.Manifest.Transforms.Missions; h != nil && (event == "" || event == EventTransform) {
		payload := TransformPayload{Generation: 1, Missions: data.rows}
		if len(data.rows) > maxTransformRows {
			payload.Missions = data.rows[:maxTransformRows]
			payload.Truncated = true
		}
		known := make(map[string]bool, len(payload.Missions))
		for _, r := range payload.Missions {
			known[r.Key] = true
		}
		record(runHandlerTest(w, m, "transforms.missions", EventTransform, h.Command, payload,
			m.Manifest.ResolveTimeout(SurfaceTransform, h.TimeoutMs), CapReply, known))
	}
	if h := m.Manifest.Transforms.Preview; h != nil && (event == "" || event == EventPreview) {
		record(runHandlerTest(w, m, "transforms.preview", EventPreview, h.Command, PreviewPayload{Mission: data.mission},
			m.Manifest.ResolveTimeout(SurfacePreview, h.TimeoutMs), CapReply, nil))
	}
	if s := m.Manifest.Statusline; s != nil && (event == "" || event == EventSegment) {
		record(runHandlerTest(w, m, "statusline", EventSegment, s.Command, struct{}{},
			m.Manifest.ResolveTimeout(SurfaceSegment, s.TimeoutMs), CapSegment, nil))
	}
	if h := m.Manifest.PreLaunch; h != nil && (event == "" || event == EventPreLaunch) {
		record(runPreLaunchTest(w, m, PreLaunchPayload{
			Source: "test", Cwd: m.Dir, Mission: &data.mission, Account: &data.account,
		}))
	}
	if ran == 0 {
		if event == "" {
			fmt.Fprintln(w, "error: the manifest declares no handler contributions")
		} else {
			fmt.Fprintf(w, "error: the manifest declares nothing for event %s\n", event)
		}
		return 1
	}
	if failed > 0 {
		fmt.Fprintf(w, "\nFAIL: %d of %d contribution(s) failed\n", failed, ran)
		return 1
	}
	fmt.Fprintf(w, "\nPASS: %d contribution(s)\n", ran)
	return 0
}

// canonicalEvent maps --event input to a wire event name; both the full
// names and bare surface words are accepted.
func canonicalEvent(e string) (string, error) {
	switch e {
	case "":
		return "", nil
	case EventAction, "action", "actions":
		return EventAction, nil
	case EventTransform, "transform", "missions":
		return EventTransform, nil
	case EventPreview, "preview":
		return EventPreview, nil
	case EventSegment, "segment", "statusline":
		return EventSegment, nil
	case EventPreLaunch, "prelaunch", "preLaunch", "launch":
		return EventPreLaunch, nil
	}
	return "", fmt.Errorf("unknown event %q (want action.invoke, missions.transform, preview.append statusline.segment or launch.before)", e)
}

// loadForTest loads a module straight from its directory. No registry lookup
// and no enabled check: disabled and even unregistered modules are exactly
// what an author wants to test, and running `module test <name>` is itself
// the consent to execute that module's handlers once.
func loadForTest(name string) (Module, error) {
	dir := filepath.Join(Dir(), name)
	if !within(Dir(), dir) {
		return Module{}, errors.New("name escapes the modules directory")
	}
	b, err := os.ReadFile(filepath.Join(dir, "module.json"))
	if err != nil {
		return Module{}, fmt.Errorf("module.json: %v", err)
	}
	man, err := ParseManifest(b)
	if err != nil {
		return Module{}, err
	}
	if man.Name != name {
		return Module{}, fmt.Errorf("manifest name %q does not match directory name %q", man.Name, name)
	}
	return Module{Entry: Entry{Name: name}, Manifest: man, Dir: dir, Settings: config.Load().Modules[name].Settings}, nil
}

// feed is what the envelopes get built from: the full projection for the
// transform payload, one selected mission, one account.
type feed struct {
	rows    []MissionRow
	mission MissionRow
	account AccountRow
}

// fixtureFeed is the canned input: two missions exercising both shapes (one
// pinned and tagged, one plain) and one account. Fixed timestamps keep the
// printed envelopes reproducible run to run.
func fixtureFeed() feed {
	home := "/Users/you"
	if runtime.GOOS == "windows" {
		home = `C:\Users\you`
	}
	rows := []MissionRow{
		{
			Key:           "C--Users-you-webapp/0198aaaa-1111-7000-8000-000000000001",
			ID:            "0198aaaa-1111-7000-8000-000000000001",
			Project:       "C--Users-you-webapp",
			Title:         "Fix OAuth redirect loop",
			Cwd:           filepath.Join(home, "webapp"),
			GitBranch:     "fix/oauth",
			Tags:          []string{"auth", "wip"},
			Pinned:        true,
			LastTime:      time.Date(2026, 7, 6, 9, 14, 2, 0, time.UTC),
			UserMsgs:      12,
			AssistantMsgs: 30,
		},
		{
			Key:           "C--Users-you-api/0198bbbb-2222-7000-8000-000000000002",
			ID:            "0198bbbb-2222-7000-8000-000000000002",
			Project:       "C--Users-you-api",
			Title:         "Add rate limiting to the ingest endpoint",
			Cwd:           filepath.Join(home, "api"),
			LastTime:      time.Date(2026, 7, 5, 18, 30, 0, 0, time.UTC),
			UserMsgs:      3,
			AssistantMsgs: 7,
		},
	}
	return feed{
		rows:    rows,
		mission: rows[0],
		account: AccountRow{
			ID:        "work",
			Label:     "Work account",
			ConfigDir: filepath.Join(home, ".claude-accounts", "work"),
			LastUse:   "2026-07-06T08:00:00Z",
		},
	}
}

// liveFeed builds the envelopes from the real scan, store and accounts —
// what the TUI itself would send.
func liveFeed(missionKey string) (feed, error) {
	st, err := store.Load()
	if err != nil {
		return feed{}, fmt.Errorf("store: %v", err)
	}
	ms, err := scan.ScanAll()
	if err != nil {
		return feed{}, fmt.Errorf("scan: %v", err)
	}
	if len(ms) == 0 {
		return feed{}, errors.New("--live: no missions found")
	}
	rows := ProjectRows(ms, st)
	sel := rows[0] // ScanAll sorts most-recent first
	if missionKey != "" {
		found := false
		for _, r := range rows {
			if r.Key == missionKey {
				sel, found = r, true
				break
			}
		}
		if !found {
			return feed{}, fmt.Errorf("--mission: no mission with key %q", missionKey)
		}
	}
	f := feed{rows: rows, mission: sel, account: fixtureFeed().account}
	if accs, err := accounts.Load(); err == nil && len(accs) > 0 {
		f.account = AccountRowOf(accs[0])
	}
	return f, nil
}

// runActionTest runs one action contribution. Non-interactive actions go
// through Invoke; interactive ones run attached to the real terminal with
// the envelope file, exactly like the TUI would run them.
func runActionTest(w io.Writer, m Module, a Action, d feed) bool {
	payload := ActionPayload{Screen: a.Screen, Action: a.ID}
	if a.Screen == "accounts" {
		payload.Account = &d.account
	} else {
		payload.Mission = &d.mission
	}
	label := fmt.Sprintf("action %s (key %s, %s)", a.ID, a.Key, a.Screen)
	if a.Interactive {
		return runInteractiveTest(w, m, a, label, payload)
	}
	return runHandlerTest(w, m, label, EventAction, a.Command, payload,
		m.Manifest.ResolveTimeout(SurfaceAction, a.TimeoutMs), CapReply, nil)
}

// runHandlerTest execs one non-interactive contribution via the runner and
// prints the report: envelope, raw stdout/stderr, field-by-field verdict,
// wall time against the resolved timeout.
func runHandlerTest(w io.Writer, m Module, label, event string, argv []string, payload any, timeout time.Duration, outCap int64, knownKeys map[string]bool) bool {
	fmt.Fprintf(w, "\n=== %s → %s\n", label, event)
	env := NewEnvelope(event, m, payload)
	printEnvelope(w, env)
	fmt.Fprintf(w, "exec: %s (cwd %s, timeout %s, stdout cap %d bytes)\n", strings.Join(argv, " "), m.Dir, timeout, outCap)
	start := time.Now()
	raw, tail, err := invokeTail(context.Background(), m, argv, env, outCap, timeout)
	wall := time.Since(start).Round(time.Millisecond)
	printRaw(w, "stdout", raw)
	printRaw(w, "stderr", tail)
	ok := true
	fmt.Fprintln(w, "verdict:")
	if err != nil {
		fmt.Fprintf(w, "  ✗ %v (see houston module log)\n", err)
		ok = false
	} else {
		for _, v := range verdictReply(event, raw, knownKeys) {
			fmt.Fprintln(w, "  "+v.String())
			if v.Level == vFail {
				ok = false
			}
		}
	}
	fmt.Fprintf(w, "wall time: %s of %s\n", wall, timeout)
	return ok
}

// runInteractiveTest runs an interactive action for real: a plain exec.Cmd
// attached to this terminal, envelope in $HOUSTON_EVENT_FILE, no timeout, no
// caps — there is no reply protocol, so the exit code is the whole verdict.
func runInteractiveTest(w io.Writer, m Module, a Action, label string, payload any) bool {
	fmt.Fprintf(w, "\n=== %s → %s (interactive)\n", label, EventAction)
	env := NewEnvelope(EventAction, m, payload)
	printEnvelope(w, env)
	cmd, cleanup, err := ExecAction(m, a, env)
	if err != nil {
		fmt.Fprintf(w, "verdict:\n  ✗ %v\n", err)
		return false
	}
	defer cleanup()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	fmt.Fprintf(w, "exec: %s (cwd %s, envelope in $HOUSTON_EVENT_FILE, no timeout — the handler owns the terminal)\n", strings.Join(a.Command, " "), m.Dir)
	start := time.Now()
	runErr := cmd.Run()
	wall := time.Since(start).Round(time.Millisecond)
	if runErr != nil {
		fmt.Fprintf(w, "verdict:\n  ✗ %v\n", runErr)
	} else {
		fmt.Fprintln(w, "verdict:")
		fmt.Fprintln(w, "  ✓ exit 0 (interactive actions have no reply protocol)")
	}
	fmt.Fprintf(w, "wall time: %s (no timeout)\n", wall)
	return runErr == nil
}

// runPreLaunchTest runs the pre-launch hook for real, like an interactive
// action. Any exit code is a valid outcome by contract — 0 means "launch",
// nonzero means "cancel" — so both verdicts pass; what fails the test is a
// hook that cannot be built or started at all.
func runPreLaunchTest(w io.Writer, m Module, payload PreLaunchPayload) bool {
	fmt.Fprintf(w, "\n=== preLaunch → %s (interactive)\n", EventPreLaunch)
	env := NewEnvelope(EventPreLaunch, m, payload)
	printEnvelope(w, env)
	cmd, cleanup, err := ExecPreLaunch(m, env)
	if err != nil {
		fmt.Fprintf(w, "verdict:\n  ✗ %v\n", err)
		return false
	}
	defer cleanup()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	fmt.Fprintf(w, "exec: %s (cwd %s, envelope in $HOUSTON_EVENT_FILE, no timeout — the handler owns the terminal)\n", strings.Join(m.Manifest.PreLaunch.Command, " "), m.Dir)
	start := time.Now()
	runErr := cmd.Run()
	wall := time.Since(start).Round(time.Millisecond)
	fmt.Fprintln(w, "verdict:")
	switch {
	case runErr == nil:
		fmt.Fprintln(w, "  ✓ exit 0 → launch would continue")
	default:
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			fmt.Fprintf(w, "  ✓ exit %d → launch would be cancelled (a valid verdict)\n", ee.ExitCode())
		} else {
			fmt.Fprintf(w, "  ✗ %v\n", runErr)
			fmt.Fprintf(w, "wall time: %s (no timeout)\n", wall)
			return false
		}
	}
	fmt.Fprintf(w, "wall time: %s (no timeout)\n", wall)
	return true
}

func printEnvelope(w io.Writer, env Envelope) {
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		fmt.Fprintf(w, "envelope: %v\n", err)
		return
	}
	fmt.Fprintf(w, "envelope:\n%s\n", b)
}

// rawDisplayCap bounds the printout of raw handler output; the full stream
// is still decoded and verdicted.
const rawDisplayCap = 8 << 10

// printRaw prints one raw stream as the handler emitted it — unsanitized by
// design, this is a debugging tool.
func printRaw(w io.Writer, name string, b []byte) {
	if len(b) == 0 {
		fmt.Fprintf(w, "%s: (empty)\n", name)
		return
	}
	shown := b
	if len(shown) > rawDisplayCap {
		shown = shown[:rawDisplayCap]
	}
	fmt.Fprintf(w, "%s (%d bytes):\n%s\n", name, len(b), shown)
	if len(b) > rawDisplayCap {
		fmt.Fprintf(w, "… (%d more bytes not shown)\n", len(b)-rawDisplayCap)
	}
}

// --- field-by-field reply verdicts ------------------------------------------

// verdictLevel grades one verdict line: pass, informational note (Houston
// would sanitize or ignore the field, not drop the reply), or failure (the
// real surface would drop the whole contribution).
type verdictLevel int

const (
	vOK verdictLevel = iota
	vNote
	vFail
)

// verdict is one line of the field-by-field report.
type verdict struct {
	Level verdictLevel
	Text  string
}

func (v verdict) String() string {
	switch v.Level {
	case vFail:
		return "✗ " + v.Text
	case vNote:
		return "! " + v.Text
	default:
		return "✓ " + v.Text
	}
}

// replyFields are the known reply fields per event; anything else is noted
// as ignored.
var replyFields = map[string]map[string]bool{
	EventAction:    {"status": true, "refresh": true, "notice": true},
	EventTransform: {"patches": true, "notice": true},
	EventPreview:   {"sections": true, "notice": true},
	EventSegment:   {"text": true, "notice": true},
}

var (
	patchFields   = map[string]bool{"key": true, "title": true, "badge": true, "sortKey": true, "hide": true}
	sectionFields = map[string]bool{"title": true, "body": true}
)

// verdictReply grades a successful exec's stdout against the event's reply
// contract, mirroring exactly what the real surface would keep, sanitize,
// ignore or reject. knownKeys (transform only) are the mission keys present
// in the payload — patches for other keys are ignored by the merge.
func verdictReply(event string, raw []byte, knownKeys map[string]bool) []verdict {
	var top map[string]json.RawMessage
	if err := DecodeReply(raw, &top); err != nil {
		return []verdict{{vFail, err.Error()}}
	}
	if top == nil {
		return []verdict{{vOK, "empty reply: valid no-op (" + noopMeaning(event) + ")"}}
	}
	var out []verdict
	var err error
	switch event {
	case EventTransform:
		out, err = transformVerdicts(raw, knownKeys)
	case EventPreview:
		out, err = previewVerdicts(raw)
	case EventAction:
		out, err = actionVerdicts(raw)
	case EventSegment:
		out, err = segmentVerdicts(raw)
	}
	if err != nil {
		// A wrong-typed known field: the real surface drops the whole reply.
		return []verdict{{vFail, err.Error()}}
	}
	out = append(out, unknownFieldNotes(top, replyFields[event], "")...)
	out = append(out, noticeVerdict(event, top)...)
	return out
}

func noopMeaning(event string) string {
	switch event {
	case EventAction:
		return `generic "done"`
	case EventTransform:
		return "zero patches"
	case EventPreview:
		return "no sections"
	case EventSegment:
		return "segment hidden"
	}
	return "no-op"
}

func transformVerdicts(raw []byte, knownKeys map[string]bool) ([]verdict, error) {
	var rep transformReply
	if err := DecodeReply(raw, &rep); err != nil {
		return nil, err
	}
	// Generic re-decode for unknown-field detection inside each patch; the
	// typed decode above already proved the shape.
	var gen struct {
		Patches []map[string]json.RawMessage `json:"patches"`
	}
	_ = DecodeReply(raw, &gen)
	out := []verdict{{vOK, fmt.Sprintf("patches: %d", len(rep.Patches))}}
	seen := map[string]bool{}
	for i, p := range rep.Patches {
		f := fmt.Sprintf("patches[%d]", i)
		if i < len(gen.Patches) {
			out = append(out, unknownFieldNotes(gen.Patches[i], patchFields, f+".")...)
		}
		switch {
		case knownKeys != nil && !knownKeys[p.Key]:
			out = append(out, verdict{vNote, fmt.Sprintf("%s: key %q is not in the payload — ignored", f, p.Key)})
			continue
		case seen[p.Key]:
			out = append(out, verdict{vNote, fmt.Sprintf("%s: duplicate key %q — the first patch wins", f, p.Key)})
			continue
		}
		seen[p.Key] = true
		if p.Title != nil {
			out = append(out, clipVerdict(f+".title", *p.Title, 200))
		}
		if p.Badge != nil {
			out = append(out, clipVerdict(f+".badge", *p.Badge, 16))
		}
		if p.SortKey != nil {
			out = append(out, verdict{vOK, fmt.Sprintf("%s.sortKey: %q (default views only; program views keep their order)", f, *p.SortKey)})
		}
		if p.Hide != nil && *p.Hide {
			out = append(out, verdict{vOK, f + ".hide: row removed from all list views"})
		}
	}
	return out, nil
}

func previewVerdicts(raw []byte) ([]verdict, error) {
	var rep previewReply
	if err := DecodeReply(raw, &rep); err != nil {
		return nil, err
	}
	var gen struct {
		Sections []map[string]json.RawMessage `json:"sections"`
	}
	_ = DecodeReply(raw, &gen)
	var out []verdict
	if n := len(rep.Sections); n > maxPreviewSections {
		out = append(out, verdict{vNote, fmt.Sprintf("sections: %d — only the first %d are shown", n, maxPreviewSections)})
	} else {
		out = append(out, verdict{vOK, fmt.Sprintf("sections: %d (max %d)", n, maxPreviewSections)})
	}
	for i, s := range rep.Sections {
		if i >= maxPreviewSections {
			break
		}
		f := fmt.Sprintf("sections[%d]", i)
		if i < len(gen.Sections) {
			out = append(out, unknownFieldNotes(gen.Sections[i], sectionFields, f+".")...)
		}
		out = append(out, clipVerdict(f+".title", s.Title, 40))
		out = append(out, bodyVerdict(f+".body", s.Body))
	}
	return out, nil
}

func actionVerdicts(raw []byte) ([]verdict, error) {
	var rep actionReplyWire
	if err := DecodeReply(raw, &rep); err != nil {
		return nil, err
	}
	var out []verdict
	if rep.Status == "" {
		out = append(out, verdict{vOK, `status: empty (a generic "done" is shown)`})
	} else {
		out = append(out, clipVerdict("status", rep.Status, 120))
	}
	out = append(out, verdict{vOK, fmt.Sprintf("refresh: %v (ORed with the manifest's refreshAfter)", rep.Refresh)})
	return out, nil
}

func segmentVerdicts(raw []byte) ([]verdict, error) {
	var rep segmentReply
	if err := DecodeReply(raw, &rep); err != nil {
		return nil, err
	}
	if rep.Text == "" {
		return []verdict{{vOK, "text: empty — segment hidden this cycle (valid)"}}, nil
	}
	return []verdict{clipVerdict("text", rep.Text, segMaxRunes)}, nil
}

// clipVerdict previews Houston's single-line sanitization of one field: the
// value the surface would actually render, flagged when it differs (clipped,
// ANSI/control stripped, or cut at the first newline).
func clipVerdict(field, val string, maxRunes int) verdict {
	clean := CleanLine(val, maxRunes)
	if clean == val {
		return verdict{vOK, fmt.Sprintf("%s: %q (%d/%d runes)", field, val, utf8.RuneCountInString(val), maxRunes)}
	}
	return verdict{vNote, fmt.Sprintf("%s: %q renders as %q (stripped/clipped to %d runes)", field, val, clean, maxRunes)}
}

// bodyVerdict previews the multi-line preview-body sanitization.
func bodyVerdict(field, body string) verdict {
	clean := cleanBody(body, maxPreviewBody)
	switch {
	case clean == body:
		return verdict{vOK, fmt.Sprintf("%s: %d bytes (cap %d)", field, len(body), maxPreviewBody)}
	case len(body) > maxPreviewBody:
		return verdict{vNote, fmt.Sprintf("%s: %d bytes — clipped to %d and sanitized", field, len(body), len(clean))}
	default:
		return verdict{vNote, fmt.Sprintf("%s: sanitized (ANSI/control stripped, tabs → 2 spaces)", field)}
	}
}

// unknownFieldNotes flags reply fields Houston silently ignores — usually a
// typo in the handler ("patchs") that would otherwise fail without a trace.
func unknownFieldNotes(obj map[string]json.RawMessage, known map[string]bool, prefix string) []verdict {
	var names []string
	for k := range obj {
		if !known[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	out := make([]verdict, 0, len(names))
	for _, k := range names {
		out = append(out, verdict{vNote, fmt.Sprintf("unknown field %q ignored", prefix+k)})
	}
	return out
}

// noticeVerdict grades the optional notice field. Transforms and previews
// surface it in the TUI footer (≤ 120 runes); actions use it as the footer
// text when status is empty; segments have no footer, so there it decodes
// and drops.
func noticeVerdict(event string, top map[string]json.RawMessage) []verdict {
	rawNotice, present := top["notice"]
	if !present {
		return nil
	}
	if event == EventSegment {
		return []verdict{{vNote, "notice: ignored on this surface"}}
	}
	if event == EventAction {
		if _, hasStatus := top["status"]; hasStatus {
			return []verdict{{vNote, "notice: unused when status is set (status wins the footer)"}}
		}
	}
	var s string
	if err := json.Unmarshal(rawNotice, &s); err != nil {
		// Unreachable in practice: the typed decode above already failed on a
		// wrong-typed notice for the events that keep it.
		return []verdict{{vFail, "notice: must be a string"}}
	}
	if s == "" {
		return nil
	}
	return []verdict{clipVerdict("notice", s, 120)}
}
