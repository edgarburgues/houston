package module

import (
	"context"
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
	title, body, err := RunView(context.Background(), m, v)
	if err != nil {
		t.Fatal(err)
	}
	if title != "My Page" {
		t.Fatalf("title: %q", title)
	}
	if !strings.Contains(body, "line1\nline2") {
		t.Fatalf("body: %q", body)
	}
	if strings.Contains(body, "\x1b") {
		t.Fatal("ANSI must be stripped from view bodies")
	}
	_ = time.Now
}
