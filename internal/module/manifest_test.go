package module

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSafeName(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"jira-git", true},
		{"a", true},
		{"0mod", true},
		{"my.mod_2-x", true},
		{strings.Repeat("a", 64), true},
		{"", false},
		{strings.Repeat("a", 65), false},
		{"Foo", false},             // uppercase
		{"CON", false},             // uppercase (and reserved)
		{"con", false},             // Windows reserved device
		{"nul.sync", false},        // reserved stem before first '.'
		{"lpt9", false},            // reserved
		{"com1.handler.py", false}, // reserved stem
		{"foo.", false},            // trailing dot: Windows drops it
		{".hidden", false},         // must start [a-z0-9]
		{"-dash", false},           // must start [a-z0-9]
		{"..", false},              // traversal
		{"../evil", false},         // traversal chars
		{`a\b`, false},             // separator
		{"a/b", false},             // separator
		{"a b", false},             // space
		{"café", false},            // non-ASCII
		{"conx", true},             // reserved names match stems, not prefixes
		{"console", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := SafeName(tt.name)
			if tt.ok && err != nil {
				t.Errorf("SafeName(%q) = %v, want nil", tt.name, err)
			}
			if !tt.ok && err == nil {
				t.Errorf("SafeName(%q) = nil, want error", tt.name)
			}
		})
	}
}

// act builds a minimal manifest with one action whose named field is
// replaced, keeping the invalid-matrix rows short.
func act(field, value string) string {
	a := map[string]string{
		"id":      `"a1"`,
		"key":     `"J"`,
		"title":   `"do the thing"`,
		"screen":  `"missions"`,
		"command": `["python", "handler.py"]`,
	}
	a[field] = value
	return `{"api":1,"name":"mod","actions":[{` +
		`"id":` + a["id"] + `,"key":` + a["key"] + `,"title":` + a["title"] +
		`,"screen":` + a["screen"] + `,"command":` + a["command"] + `}]}`
}

func TestParseManifest(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string // substring; "" means must parse
	}{
		{"minimal valid", `{"api":1,"name":"mod"}`, ""},
		{"missing api", `{"name":"mod"}`, `missing "api"`},
		{"api 2 unavailable", `{"api":2,"name":"mod"}`, "needs modules api 2; try houston update"},
		{"not json", `{`, "module.json"},
		{"wrong-typed known field", `{"api":"1","name":"mod"}`, "module.json"},
		{"wrong-typed actions", `{"api":1,"name":"mod","actions":{}}`, "module.json"},
		{"wrong-typed command", act("command", `"python"`), "module.json"},
		{"unknown fields ignored",
			`{"api":1,"name":"mod","homepage":"https://x","future":{"deep":[1]}}`, ""},
		{"unknown action fields ignored",
			`{"api":1,"name":"mod","actions":[{"id":"a1","key":"J","title":"t","screen":"missions","command":["python"],"newfield":true}]}`, ""},
		{"bad name uppercase", `{"api":1,"name":"Mod"}`, "invalid module name"},
		{"bad name reserved", `{"api":1,"name":"con"}`, "reserved"},
		{"bad name reserved stem", `{"api":1,"name":"nul.sync"}`, "reserved"},
		{"bad name trailing dot", `{"api":1,"name":"mod."}`, "invalid module name"},
		{"bad name traversal", `{"api":1,"name":"../x"}`, "invalid module name"},
		{"description over 200 runes",
			`{"api":1,"name":"mod","description":"` + strings.Repeat("x", 201) + `"}`,
			"description exceeds 200 runes"},
		{"17 actions", manyActions(17), "max 16"},
		{"16 actions ok", manyActions(16), ""},
		{"duplicate ids",
			`{"api":1,"name":"mod","actions":[` + oneAction("a1", "J") + `,` + oneAction("a1", "K") + `]}`,
			"duplicate id"},
		{"duplicate keys",
			`{"api":1,"name":"mod","actions":[` + oneAction("a1", "J") + `,` + oneAction("a2", "J") + `]}`,
			`duplicate key "J"`},
		{"invalid id charset", act("id", `"A1"`), "invalid id"},
		{"missing id", act("id", `""`), "invalid id"},
		{"key valid ctrl", act("key", `"ctrl+j"`), ""},
		{"key valid punctuation", act("key", `"?"`), ""},
		{"key case-sensitive upper ok", act("key", `"J"`), ""},
		{"key empty", act("key", `""`), "invalid key"},
		{"key multi-rune", act("key", `"jj"`), "invalid key"},
		{"key named key not a rune", act("key", `"tab"`), "invalid key"},
		{"key bare ctrl", act("key", `"ctrl+"`), "invalid key"},
		{"ctrl-alias ctrl+i", act("key", `"ctrl+i"`), `key "ctrl+i" arrives as "tab"`},
		{"ctrl-alias ctrl+m", act("key", `"ctrl+m"`), `key "ctrl+m" arrives as "enter"`},
		{"ctrl-alias ctrl+[", act("key", `"ctrl+["`), `key "ctrl+[" arrives as "esc"`},
		{"ctrl-alias ctrl+h", act("key", `"ctrl+h"`), `key "ctrl+h" arrives as "backspace"`},
		{"title empty", act("title", `""`), "title must be 1-40 runes"},
		{"title over 40 runes", act("title", `"`+strings.Repeat("x", 41)+`"`), "title must be 1-40 runes"},
		{"screen invalid", act("screen", `"settings"`), "screen must be"},
		{"screen accounts ok", act("screen", `"accounts"`), ""},
		{"command empty array", act("command", `[]`), "at least one element"},
		{"command empty argv0", act("command", `[""]`), "must not be empty"},
		{"command absolute posix", act("command", `["/usr/bin/python"]`), "bare executable names or paths inside"},
		{"command absolute windows", act("command", `["C:\\Python312\\python.exe"]`), "bare executable names or paths inside"},
		{"command drive-relative", act("command", `["c:python"]`), "bare executable names or paths inside"},
		{"command unc", act("command", `["\\\\server\\share\\x.exe"]`), "bare executable names or paths inside"},
		{"command dotdot escape", act("command", `["../evil"]`), "bare executable names or paths inside"},
		{"command dotdot backslash", act("command", `["..\\evil"]`), "bare executable names or paths inside"},
		{"command hidden escape", act("command", `["scripts/../../evil"]`), "bare executable names or paths inside"},
		{"command relative inside ok", act("command", `["scripts/run.ps1"]`), ""},
		{"command dot-relative ok", act("command", `["./run.ps1"]`), ""},
		{"command inner dotdot ok", act("command", `["scripts/../run.ps1"]`), ""},
		{"transform command validated",
			`{"api":1,"name":"mod","transforms":{"missions":{"command":["/abs/x"]}}}`,
			"transforms.missions: commands must be bare"},
		{"preview command validated",
			`{"api":1,"name":"mod","transforms":{"preview":{"command":[]}}}`,
			"transforms.preview: command must have at least one element"},
		{"statusline command validated",
			`{"api":1,"name":"mod","statusline":{"command":["..\\..\\x"]}}`,
			"statusline: commands must be bare"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tt.body))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseManifest() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseManifest() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func oneAction(id, key string) string {
	return `{"id":"` + id + `","key":"` + key + `","title":"t","screen":"missions","command":["python"]}`
}

func manyActions(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = oneAction("a"+strings.Repeat("x", i+1), string(rune('a'+i)))
	}
	return `{"api":1,"name":"mod","actions":[` + strings.Join(parts, ",") + `]}`
}

// TestParseManifestFull parses the docs' all-surfaces example and pins that
// argv elements — flags included — come through verbatim, never rewritten.
func TestParseManifestFull(t *testing.T) {
	body := `{
	  "api": 1,
	  "name": "jira-git",
	  "version": "1.2.0",
	  "description": "Jira ticket badges, preview info, and a sync action",
	  "actions": [
	    { "id": "open-ticket", "key": "J", "title": "open Jira ticket", "screen": "missions",
	      "command": ["pwsh", "-NoProfile", "-File", "open-ticket.ps1"] },
	    { "id": "sync-worklog", "key": "ctrl+j", "title": "log time to Jira", "screen": "missions",
	      "command": ["python", "sync_worklog.py"], "interactive": true, "refreshAfter": true }
	  ],
	  "transforms": {
	    "missions": { "command": ["python", "badge_missions.py"], "timeoutMs": 1500 },
	    "preview":  { "command": ["python", "preview_ticket.py"] }
	  },
	  "statusline": { "command": ["pwsh", "-NoProfile", "-File", "sprint_segment.ps1"], "ttlSeconds": 300 },
	  "theme": { "colors": { "yellow": "214" } }
	}`
	m, err := ParseManifest([]byte(body))
	if err != nil {
		t.Fatalf("ParseManifest() = %v", err)
	}
	if m.API != 1 || m.Name != "jira-git" || m.Version != "1.2.0" {
		t.Errorf("header fields wrong: %+v", m)
	}
	if len(m.Actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(m.Actions))
	}
	// Flag-bearing argv untouched: -NoProfile/-File are options, not paths.
	want := []string{"pwsh", "-NoProfile", "-File", "open-ticket.ps1"}
	if !reflect.DeepEqual(m.Actions[0].Command, want) {
		t.Errorf("argv rewritten: %q, want %q", m.Actions[0].Command, want)
	}
	a := m.Actions[1]
	if !a.Interactive || !a.RefreshAfter || a.Key != "ctrl+j" {
		t.Errorf("action flags wrong: %+v", a)
	}
	if m.Transforms.Missions == nil || m.Transforms.Missions.TimeoutMs != 1500 {
		t.Errorf("transforms.missions wrong: %+v", m.Transforms.Missions)
	}
	if m.Transforms.Preview == nil || len(m.Transforms.Preview.Command) != 2 {
		t.Errorf("transforms.preview wrong: %+v", m.Transforms.Preview)
	}
	if m.Statusline == nil || m.Statusline.TTLSeconds != 300 {
		t.Errorf("statusline wrong: %+v", m.Statusline)
	}
	if m.Theme == nil || m.Theme.Colors["yellow"] != "214" {
		t.Errorf("theme wrong: %+v", m.Theme)
	}
}

func TestResolveTimeout(t *testing.T) {
	tests := []struct {
		name      string
		surface   Surface
		moduleMs  int
		handlerMs int
		want      time.Duration
	}{
		{"action surface default", SurfaceAction, 0, 0, 10 * time.Second},
		{"transform surface default", SurfaceTransform, 0, 0, 2 * time.Second},
		{"preview surface default", SurfacePreview, 0, 0, 3 * time.Second},
		{"segment surface default", SurfaceSegment, 0, 0, 4 * time.Second},
		{"handler level wins over module-wide", SurfaceAction, 20000, 15000, 15 * time.Second},
		{"module-wide fallback", SurfaceTransform, 5000, 0, 5 * time.Second},
		{"module-wide fallback clamped per surface", SurfaceSegment, 30000, 0, 4 * time.Second},
		{"action clamp floor", SurfaceAction, 0, 100, 500 * time.Millisecond},
		{"action clamp ceiling", SurfaceAction, 0, 60000, 30 * time.Second},
		{"transform clamp floor", SurfaceTransform, 0, 50, 200 * time.Millisecond},
		{"transform clamp ceiling", SurfaceTransform, 0, 99999, 10 * time.Second},
		{"preview clamp floor", SurfacePreview, 0, 1, 200 * time.Millisecond},
		{"segment clamp floor", SurfaceSegment, 0, 100, 500 * time.Millisecond},
		{"segment clamp ceiling", SurfaceSegment, 0, 8000, 4 * time.Second},
		{"negative treated as unset", SurfacePreview, -5, -7, 3 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Manifest{TimeoutMs: tt.moduleMs}
			if got := m.ResolveTimeout(tt.surface, tt.handlerMs); got != tt.want {
				t.Errorf("ResolveTimeout(%v, %d) with module %d = %v, want %v",
					tt.surface, tt.handlerMs, tt.moduleMs, got, tt.want)
			}
		})
	}
}

func TestSegmentTTL(t *testing.T) {
	tests := []struct {
		name string
		ttl  int
		want time.Duration
	}{
		{"default 60", 0, 60 * time.Second},
		{"floor 60", 30, 60 * time.Second},
		{"in range", 300, 300 * time.Second},
		{"ceiling 3600", 7200, 3600 * time.Second},
		{"negative defaults", -1, 60 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Segment{TTLSeconds: tt.ttl}).TTL(); got != tt.want {
				t.Errorf("TTL(%d) = %v, want %v", tt.ttl, got, tt.want)
			}
		})
	}
}
