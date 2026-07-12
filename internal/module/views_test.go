package module

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestManifestViewValidation(t *testing.T) {
	base := `{"api":1,"name":"m",%s}`
	cases := []struct {
		name    string
		frag    string
		wantErr string
	}{
		{"valid", `"views":[{"id":"issues","key":"I","title":"Issues","command":["pwsh","-File","x.ps1"]}]`, ""},
		{"dup key vs action", `"actions":[{"id":"a","key":"I","title":"t","screen":"missions","command":["x"]}],"views":[{"id":"v","key":"I","title":"t","command":["x"]}]`, "duplicate key"},
		{"dup key across views", `"views":[{"id":"a","key":"I","title":"t","command":["x"]},{"id":"b","key":"I","title":"t","command":["x"]}]`, "duplicate key"},
		{"bad id", `"views":[{"id":"Bad Id","key":"I","title":"t","command":["x"]}]`, "invalid id"},
		{"ctrl alias", `"views":[{"id":"v","key":"ctrl+i","title":"t","command":["x"]}]`, "can never fire"},
		{"absolute command", `"views":[{"id":"v","key":"I","title":"t","command":["C:\\evil.exe"]}]`, "inside the module directory"},
		{"no title", `"views":[{"id":"v","key":"I","title":"","command":["x"]}]`, "title"},
		{"action enter ok", `"views":[{"id":"v","key":"I","title":"t","command":["x"],"actions":[{"id":"open","key":"enter","title":"open"}]}]`, ""},
		{"action ctrl alias", `"views":[{"id":"v","key":"I","title":"t","command":["x"],"actions":[{"id":"a","key":"ctrl+m","title":"t"}]}]`, "can never fire"},
		{"action dup key", `"views":[{"id":"v","key":"I","title":"t","command":["x"],"actions":[{"id":"a","key":"c","title":"t"},{"id":"b","key":"c","title":"t"}]}]`, "duplicate key"},
		{"action bad id", `"views":[{"id":"v","key":"I","title":"t","command":["x"],"actions":[{"id":"Bad Id","key":"c","title":"t"}]}]`, "invalid id"},
		{"action no title", `"views":[{"id":"v","key":"I","title":"t","command":["x"],"actions":[{"id":"a","key":"c","title":""}]}]`, "title"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(strings.Replace(base, "%s", c.frag, 1)))
			if c.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestRunView(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "viewer")
	v := View{ID: "page", Key: "I", Title: "Fallback", Command: helperCmd("view")}
	m.Manifest.Views = []View{v}
	title, body, rows, err := RunView(context.Background(), m, v)
	if err != nil {
		t.Fatal(err)
	}
	if title != "My Page" {
		t.Fatalf("title: %q", title)
	}
	if len(rows) != 0 {
		t.Fatalf("a body view must not grow rows: %v", rows)
	}
	if !strings.Contains(body, "line1\nline2") {
		t.Fatalf("body: %q", body)
	}
	if strings.Contains(body, "\x1b") {
		t.Fatal("ANSI must be stripped from view bodies")
	}
	_ = time.Now
}

func TestRunViewRows(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "lister")
	v := View{ID: "list", Key: "I", Title: "Fallback", Command: helperCmd("view-rows")}
	m.Manifest.Views = []View{v}
	title, body, rows, err := RunView(context.Background(), m, v)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Rows (2)" {
		t.Fatalf("title: %q", title)
	}
	if body != "" {
		t.Fatalf("rows win over body, got body %q", body)
	}
	if len(rows) != 2 || rows[0].ID != "r1" || rows[1].Text != "second" {
		t.Fatalf("rows: %+v", rows)
	}
	if strings.Contains(rows[0].Text, "\x1b") {
		t.Fatal("ANSI must be stripped from row text")
	}
}

func TestRunViewAction(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "lister")
	v := View{ID: "list", Key: "I", Title: "L", Command: helperCmd("view-invoke")}
	va := ViewAction{ID: "open", Key: "enter", Title: "open"}
	env := NewEnvelope(EventViewInvoke, m, ViewInvokePayload{View: v.ID, Action: va.ID, Row: &ViewRow{ID: "SOP-7", Text: "x"}})
	rep, err := RunViewAction(context.Background(), m, v, va, env)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != "did open on SOP-7 in list" {
		t.Fatalf("the handler must see view, action and row: %q", rep.Status)
	}
	if !rep.Refresh {
		t.Fatal("reply refresh must survive")
	}
}

func TestViewInvokeRowIsExplicitNull(t *testing.T) {
	b, err := json.Marshal(ViewInvokePayload{View: "v", Action: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"row":null`) {
		t.Fatalf("a body view must send an explicit null row, got %s", b)
	}
}

func TestViewVerdictsGradeSanitizedRows(t *testing.T) {
	reply, err := json.Marshal(map[string]any{
		"rows": []map[string]string{{"id": "", "text": string(rune(27)) + "[31m"}},
		"body": "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := viewVerdicts(reply)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, v := range out {
		joined += v.Text + "\n"
	}
	if !strings.Contains(joined, "NONE survive") || !strings.Contains(joined, "read-only page") {
		t.Fatalf("all-dead rows must be graded as the body page: %s", joined)
	}
}
