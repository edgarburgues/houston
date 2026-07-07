package module

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func joinVerdicts(vs []verdict) string {
	var b strings.Builder
	for _, v := range vs {
		b.WriteString(v.String())
		b.WriteString("\n")
	}
	return b.String()
}

func hasFail(vs []verdict) bool {
	for _, v := range vs {
		if v.Level == vFail {
			return true
		}
	}
	return false
}

func TestVerdictReply(t *testing.T) {
	known := map[string]bool{"k1": true, "k2": true}
	tests := []struct {
		name     string
		event    string
		raw      string
		keys     map[string]bool
		wantFail bool
		want     []string // substrings that must appear in the joined verdicts
	}{
		{name: "empty stdout is a no-op", event: EventTransform, raw: "",
			want: []string{"valid no-op", "zero patches"}},
		{name: "whitespace stdout is a no-op", event: EventAction, raw: " \r\n\t ",
			want: []string{"valid no-op", `generic "done"`}},
		{name: "utf16 rejected with the actionable message", event: EventAction,
			raw: "\xff\xfe{\x00}\x00", wantFail: true, want: []string{"UTF-16"}},
		{name: "leading garbage rejected", event: EventAction, raw: "Loading module...",
			wantFail: true, want: []string{"invalid reply"}},
		{name: "bom stripped silently", event: EventSegment, raw: "\xef\xbb\xbf" + `{"text":"ok"}`,
			want: []string{`text: "ok"`}},

		{name: "clean transform patch", event: EventTransform, keys: known,
			raw:  `{"patches":[{"key":"k1","badge":"OK","title":"New title"}]}`,
			want: []string{"patches: 1", `badge: "OK" (2/16 runes)`, `title: "New title"`}},
		{name: "unknown top-level field noted", event: EventTransform, keys: known,
			raw:  `{"patchs":[{"key":"k1"}]}`,
			want: []string{`unknown field "patchs" ignored`}},
		{name: "unknown patch field noted", event: EventTransform, keys: known,
			raw:  `{"patches":[{"key":"k1","color":"red"}]}`,
			want: []string{`unknown field "patches[0].color" ignored`}},
		{name: "patch key not in payload", event: EventTransform, keys: known,
			raw:  `{"patches":[{"key":"zzz","hide":true}]}`,
			want: []string{"not in the payload — ignored"}},
		{name: "duplicate patch key", event: EventTransform, keys: known,
			raw:  `{"patches":[{"key":"k1","badge":"A"},{"key":"k1","badge":"B"}]}`,
			want: []string{"the first patch wins"}},
		{name: "badge over 16 runes previews the clip", event: EventTransform, keys: known,
			raw:  fmt.Sprintf(`{"patches":[{"key":"k1","badge":%q}]}`, strings.Repeat("b", 20)),
			want: []string{"renders as", "clipped to 16"}},
		{name: "ansi in title previews the strip", event: EventTransform, keys: known,
			raw:  `{"patches":[{"key":"k1","title":"\u001b[31mRED\u001b[0m"}]}`,
			want: []string{"renders as", `"RED"`}},
		{name: "hide and sortKey reported", event: EventTransform, keys: known,
			raw:  `{"patches":[{"key":"k1","hide":true,"sortKey":"2026-07-09"}]}`,
			want: []string{"removed from all list views", "default views only"}},
		{name: "wrong-typed patches is a hard failure", event: EventTransform, keys: known,
			raw: `{"patches":5}`, wantFail: true},
		{name: "transform notice over 120 runes", event: EventTransform, keys: known,
			raw:  fmt.Sprintf(`{"patches":[],"notice":%q}`, strings.Repeat("n", 130)),
			want: []string{"notice:", "clipped to 120"}},

		{name: "action status and refresh", event: EventAction,
			raw:  `{"status":"opened PROJ-142","refresh":true}`,
			want: []string{`status: "opened PROJ-142"`, "refresh: true"}},
		{name: "action empty object", event: EventAction, raw: `{}`,
			want: []string{"status: empty", "refresh: false"}},
		{name: "action notice fills an empty status", event: EventAction, raw: `{"notice":"hi"}`,
			want: []string{`notice: "hi"`}},
		{name: "action notice loses to status", event: EventAction,
			raw:  `{"status":"s","notice":"hi"}`,
			want: []string{"notice: unused when status is set"}},
		{name: "action wrong-typed refresh is a hard failure", event: EventAction,
			raw: `{"refresh":"yes"}`, wantFail: true},

		{name: "preview extra sections dropped", event: EventPreview,
			raw:  `{"sections":[{"title":"A","body":"a"},{"title":"B","body":"b"},{"title":"C","body":"c"},{"title":"D","body":"d"}]}`,
			want: []string{"sections: 4 — only the first 3 are shown"}},
		{name: "preview title over 40 runes", event: EventPreview,
			raw:  fmt.Sprintf(`{"sections":[{"title":%q,"body":"b"}]}`, strings.Repeat("t", 50)),
			want: []string{"renders as", "clipped to 40"}},
		{name: "preview tabby body sanitized", event: EventPreview,
			raw:  `{"sections":[{"title":"T","body":"a\tb"}]}`,
			want: []string{"sanitized", "tabs"}},
		{name: "preview unknown section field", event: EventPreview,
			raw:  `{"sections":[{"title":"T","body":"b","icon":"x"}]}`,
			want: []string{`unknown field "sections[0].icon" ignored`}},

		{name: "segment text within cap", event: EventSegment,
			raw:  `{"text":"sprint 12 · 3d left"}`,
			want: []string{`text: "sprint 12 · 3d left"`}},
		{name: "segment empty text is hidden", event: EventSegment, raw: `{"text":""}`,
			want: []string{"hidden this cycle (valid)"}},
		{name: "segment second line dropped", event: EventSegment,
			raw:  `{"text":"line1\nline2"}`,
			want: []string{"renders as", `"line1"`}},
		{name: "segment notice is ignored", event: EventSegment,
			raw:  `{"text":"t","notice":"n"}`,
			want: []string{"notice: ignored on this surface"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vs := verdictReply(tc.event, []byte(tc.raw), tc.keys)
			joined := joinVerdicts(vs)
			if got := hasFail(vs); got != tc.wantFail {
				t.Fatalf("fail = %v, want %v:\n%s", got, tc.wantFail, joined)
			}
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("verdicts missing %q:\n%s", want, joined)
				}
			}
		})
	}
}

func TestCanonicalEvent(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "", want: ""},
		{in: "action", want: EventAction},
		{in: EventAction, want: EventAction},
		{in: "transform", want: EventTransform},
		{in: "missions.transform", want: EventTransform},
		{in: "preview", want: EventPreview},
		{in: "segment", want: EventSegment},
		{in: "statusline", want: EventSegment},
		{in: "bogus", wantErr: true},
	}
	for _, tc := range tests {
		got, err := canonicalEvent(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("canonicalEvent(%q) = %q, %v", tc.in, got, err)
		}
	}
}

// writeTestModule lands a module dir directly under the temp store, skipping
// the install path — RunTest must work on unregistered dirs.
func writeTestModule(t *testing.T, name, manifest string) string {
	t.Helper()
	dir := filepath.Join(Dir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "module.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// installHelper copies the running test binary into the module dir so the
// manifest can reference it as a relative command — manifest validation
// rejects absolute paths, so the helper must live inside the module.
func installHelper(t *testing.T, dir string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "handler" + filepath.Ext(exe)
	if err := copyFile(exe, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	return "./" + name
}

func TestRunTestEndToEnd(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	dir := writeTestModule(t, "passmod", "")
	h := installHelper(t, dir)
	manifest := fmt.Sprintf(`{
		"api": 1, "name": "passmod", "timeoutMs": 10000,
		"actions": [{"id": "hi", "key": "H", "title": "say hi", "screen": "missions",
			"command": [%q, "-test.run=^TestHelperProcess$", "--", "json"]}],
		"transforms": {"preview": {"command": [%q, "-test.run=^TestHelperProcess$", "--", "preview"]}}
	}`, h, h)
	if err := os.WriteFile(filepath.Join(dir, "module.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if code := RunTest("passmod", TestOpts{Out: &buf}); code != 0 {
		t.Fatalf("want exit 0, got %d:\n%s", code, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"action hi (key H, missions)",
		"transforms.preview",
		"envelope:",
		`"event": "action.invoke"`,
		`status: "done"`,
		"only the first 3 are shown", // the preview helper replies with 4 sections
		"wall time:",
		"PASS: 2 contribution(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}

	// The --event filter must skip everything else and fail when nothing is left.
	buf.Reset()
	if code := RunTest("passmod", TestOpts{Event: "preview", Out: &buf}); code != 0 {
		t.Fatalf("preview-only run failed:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "action hi") {
		t.Errorf("--event preview still ran the action:\n%s", buf.String())
	}
	buf.Reset()
	if code := RunTest("passmod", TestOpts{Event: "segment", Out: &buf}); code != 1 {
		t.Fatalf("want exit 1 when nothing matches --event, got %d:\n%s", code, buf.String())
	}
}

func TestRunTestFailingHandler(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	dir := writeTestModule(t, "failmod", "")
	h := installHelper(t, dir)
	manifest := fmt.Sprintf(`{
		"api": 1, "name": "failmod", "timeoutMs": 10000,
		"actions": [{"id": "bad", "key": "B", "title": "emit garbage", "screen": "missions",
			"command": [%q, "-test.run=^TestHelperProcess$", "--", "garbage"]}]
	}`, h)
	if err := os.WriteFile(filepath.Join(dir, "module.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if code := RunTest("failmod", TestOpts{Out: &buf}); code != 1 {
		t.Fatalf("want exit 1, got %d:\n%s", code, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"✗", "invalid reply", "FAIL: 1 of 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

func TestRunTestErrors(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	var buf bytes.Buffer
	if code := RunTest("no-such-module", TestOpts{Out: &buf}); code != 1 {
		t.Fatalf("missing module: want 1, got %d", code)
	}
	buf.Reset()
	if code := RunTest("../evil", TestOpts{Out: &buf}); code != 1 {
		t.Fatalf("traversal name: want 1, got %d", code)
	}
	buf.Reset()
	if code := RunTest("x", TestOpts{Event: "bogus", Out: &buf}); code != 1 || !strings.Contains(buf.String(), "unknown event") {
		t.Fatalf("bogus event: got %d, %q", code, buf.String())
	}
	// A valid manifest with no handler contributions has nothing to test.
	writeTestModule(t, "bare", `{"api":1,"name":"bare","theme":{"colors":{"accent":"75"}}}`)
	buf.Reset()
	if code := RunTest("bare", TestOpts{Out: &buf}); code != 1 || !strings.Contains(buf.String(), "declares no handler contributions") {
		t.Fatalf("bare module: got %d, %q", code, buf.String())
	}
}
