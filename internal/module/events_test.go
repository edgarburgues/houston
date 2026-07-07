package module

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"houston/internal/model"
	"houston/internal/store"
)

func TestProjectRowsExcludesHeavyFields(t *testing.T) {
	ms := []model.Mission{{
		ID:      "id1",
		Project: "proj",
		Title:   "a title",
		Cwd:     "C:\\work",
		Path:    "C:\\SECRET-TRANSCRIPT-PATH\\id1.jsonl",
		Search:  "SECRET-HAYSTACK conversation text",
	}}
	st := &store.Store{Meta: map[string]model.Meta{
		"proj/id1": {Tags: []string{"wip"}, Pinned: true},
	}}
	rows := ProjectRows(ms, st)
	if len(rows) != 1 {
		t.Fatalf("rows: %d", len(rows))
	}
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "SECRET-HAYSTACK") || strings.Contains(s, "SECRET-TRANSCRIPT-PATH") {
		t.Fatalf("projection leaks Search/Path:\n%s", s)
	}
	r := rows[0]
	if r.Key != "proj/id1" || !r.Pinned || len(r.Tags) != 1 || r.Tags[0] != "wip" {
		t.Fatalf("meta not projected: %+v", r)
	}
	// A nil store must not panic — CLI/statusline callers have no Store.
	if got := ProjectRows(ms, nil); got[0].Pinned {
		t.Fatal("nil store must yield zero meta")
	}
}

func TestRunTransformsMergeAndSanitize(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	handler := func(mode string) *Handler { return &Handler{Command: helperCmd(mode)} }
	a := testModule(t, "a-mod")
	a.Manifest.Transforms.Missions = handler("transform-a")
	b := testModule(t, "b-mod")
	b.Manifest.Transforms.Missions = handler("transform-b")
	rows := []MissionRow{{Key: "k1", Title: "orig"}}

	patches, warns := RunTransforms(context.Background(), []Module{a, b}, 7, rows)
	p, ok := patches["k1"]
	if !ok {
		t.Fatalf("no patch for k1: %v", patches)
	}
	// a-mod set title+badge (its second k1 patch is dropped: first per key
	// wins within one module); b-mod later wins per field it sets.
	if !p.HasTitle || p.Title != "TA" {
		t.Errorf("title: %+v", p)
	}
	if p.Badge != "B" {
		t.Errorf("badge later-name-wins: %q", p.Badge)
	}
	if !p.Hide {
		t.Error("hide from b-mod lost")
	}
	if _, ok := patches["nope"]; ok {
		t.Error("unknown key must be ignored")
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "[a-mod] hello") {
			found = true
		}
	}
	if !found {
		t.Errorf("notice missing from warnings: %v", warns)
	}
}

func TestRunTransformsSanitizesText(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "dirty")
	m.Manifest.Transforms.Missions = &Handler{Command: helperCmd("transform-dirty")}
	patches, _ := RunTransforms(context.Background(), []Module{m}, 1, []MissionRow{{Key: "k1"}})
	p := patches["k1"]
	if strings.Contains(p.Badge, "\x1b") {
		t.Fatalf("ANSI reached the badge: %q", p.Badge)
	}
	if got := len([]rune(p.Badge)); got > 16 {
		t.Fatalf("badge %d runes > 16: %q", got, p.Badge)
	}
	if !strings.HasPrefix(p.Badge, "RED") {
		t.Fatalf("badge content mangled: %q", p.Badge)
	}
	if got := len([]rune(p.Title)); got != 200 {
		t.Fatalf("title clamp: %d runes", got)
	}
}

func TestRunTransformsFailureIsIsolated(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	bad := testModule(t, "bad")
	bad.Manifest.Transforms.Missions = &Handler{Command: helperCmd("garbage")}
	good := testModule(t, "good")
	good.Manifest.Transforms.Missions = &Handler{Command: helperCmd("transform-b")}
	patches, warns := RunTransforms(context.Background(), []Module{bad, good}, 1, []MissionRow{{Key: "k1"}})
	if !patches["k1"].Hide {
		t.Fatalf("good module's patches lost to bad module's garbage: %v", patches)
	}
	if len(warns) == 0 || !strings.Contains(warns[0], "[bad]") {
		t.Fatalf("bad module not warned: %v", warns)
	}
}

func TestRunPreviewsCapsAndCleans(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "prev")
	m.Manifest.Transforms.Preview = &Handler{Command: helperCmd("preview")}
	secs, warns := RunPreviews(context.Background(), []Module{m}, MissionRow{Key: "k1"})
	if len(warns) != 0 {
		t.Fatalf("warns: %v", warns)
	}
	if len(secs) != maxPreviewSections {
		t.Fatalf("sections: %d, want %d", len(secs), maxPreviewSections)
	}
	if secs[0].Module != "prev" || secs[0].Title != "S1" {
		t.Fatalf("section: %+v", secs[0])
	}
	if !strings.Contains(secs[0].Body, "line1\n  indented") {
		t.Fatalf("tab must become two spaces: %q", secs[0].Body)
	}
}

func TestRunActionRefreshORsWithManifest(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "act")
	a := Action{ID: "x", Command: helperCmd("empty"), RefreshAfter: true}
	rep, err := RunAction(context.Background(), m, a, NewEnvelope(EventAction, m, nil))
	if err != nil {
		t.Fatal(err)
	}
	// Empty reply is a valid no-op; refresh still honors the manifest.
	if rep.Status != "" || !rep.Refresh {
		t.Fatalf("got %+v", rep)
	}
}

func TestRunActionNoticeFillsEmptyStatus(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "act")
	a := Action{ID: "x", Command: helperCmd("action-notice")}
	rep, err := RunAction(context.Background(), m, a, NewEnvelope(EventAction, m, nil))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != "from the notice" {
		t.Fatalf("notice should fill an empty status, got %+v", rep)
	}
	// status wins when both are present
	a.Command = helperCmd("action-status-and-notice")
	rep, err = RunAction(context.Background(), m, a, NewEnvelope(EventAction, m, nil))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != "the status" {
		t.Fatalf("status must win over notice, got %+v", rep)
	}
}

func TestExecActionEnvelopeFile(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "inter")
	a := Action{ID: "open", Command: []string{"whatever"}, Interactive: true}
	env := NewEnvelope(EventAction, m, ActionPayload{Screen: "missions", Action: "open"})
	cmd, cleanup, err := ExecAction(m, a, env)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr != nil {
		t.Fatal("interactive actions must not get runner SysProcAttr (Ctrl-C, console)")
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatal("interactive stdio must stay nil for tea.ExecProcess")
	}
	if cmd.Dir != m.Dir {
		t.Fatalf("cwd: %q", cmd.Dir)
	}
	var envFile string
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "HOUSTON_EVENT_FILE="); ok {
			envFile = v
		}
	}
	if envFile == "" {
		t.Fatal("HOUSTON_EVENT_FILE not set")
	}
	if filepath.Dir(envFile) != TmpDir() {
		t.Fatalf("envelope outside store tmp: %q", envFile)
	}
	b, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	var back Envelope
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Event != EventAction || back.Module != "inter" {
		t.Fatalf("envelope: %+v", back)
	}
	cleanup()
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Fatal("cleanup must remove the envelope")
	}
}

func TestSweepTmpKeepsFresh(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	if err := os.MkdirAll(TmpDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(TmpDir(), "envelope-old.json")
	fresh := filepath.Join(TmpDir(), "envelope-fresh.json")
	os.WriteFile(old, []byte("{}"), 0o600)
	os.WriteFile(fresh, []byte("{}"), 0o600)
	stale := time.Now().Add(-2 * sweepAge)
	os.Chtimes(old, stale, stale)
	SweepTmp()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale envelope kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh envelope removed — it may belong to a live session")
	}
}

func TestSweepStagingKeepsFresh(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	old := filepath.Join(Dir(), ".staging-old")
	fresh := filepath.Join(Dir(), ".staging-fresh")
	for _, d := range []string{old, fresh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-2 * sweepAge)
	os.Chtimes(old, stale, stale)
	SweepStaging()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale staging dir kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh staging dir removed — it may belong to a live module add")
	}
}

func TestStartupMaintenanceTrimsLog(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(LogPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 1023) + "\n"
	big := strings.Repeat(line, (logMaxBytes/1024)+64)
	if err := os.WriteFile(LogPath(), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	// The wiring-level check: the TUI-start entry point must actually trim,
	// not only the unit-tested TrimLog.
	StartupMaintenance()
	fi, err := os.Stat(LogPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > logMaxBytes {
		t.Fatalf("modules.log not trimmed at startup: %d bytes", fi.Size())
	}
}

func TestCleanLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"first\nsecond", "first"},
		{"crlf\r\nrest", "crlf"},
		{"\x1b[31mred\x1b[0m ok", "red ok"},
		{"tab\there", "tab here"},
		{"bell\x07gone", "bellgone"},
		{"  pad  ", "  pad"},
	}
	for _, c := range cases {
		if got := CleanLine(c.in, 80); got != c.want {
			t.Errorf("CleanLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := CleanLine("aaaaaa", 3); got != "aaa" {
		t.Errorf("clamp: %q", got)
	}
}
