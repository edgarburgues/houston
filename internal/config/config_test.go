package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPath(t *testing.T) {
	t.Setenv("HOUSTON_HOME", filepath.Join("some", "dir"))
	want := filepath.Join("some", "dir", "config.json")
	if got := Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name  string
		body  string // "" with write=false means no file at all
		write bool
		check func(t *testing.T, c Config)
	}{
		{
			name: "missing file yields zero value",
			check: func(t *testing.T, c Config) {
				if !reflect.DeepEqual(c, Config{}) {
					t.Errorf("want zero Config, got %+v", c)
				}
			},
		},
		{
			name:  "malformed JSON yields zero value",
			body:  `{"theme": {"colors": {`,
			write: true,
			check: func(t *testing.T, c Config) {
				if !reflect.DeepEqual(c, Config{}) {
					t.Errorf("want zero Config, got %+v", c)
				}
			},
		},
		{
			name:  "empty file yields zero value",
			body:  "",
			write: true,
			check: func(t *testing.T, c Config) {
				if !reflect.DeepEqual(c, Config{}) {
					t.Errorf("want zero Config, got %+v", c)
				}
			},
		},
		{
			name:  "wrong-typed field discards the whole config",
			body:  `{"theme": {"colors": {"accent": 75}}, "modules": {}}`,
			write: true,
			check: func(t *testing.T, c Config) {
				if !reflect.DeepEqual(c, Config{}) {
					t.Errorf("partial decode must not leak through, got %+v", c)
				}
			},
		},
		{
			name:  "theme colors and layout parse",
			body:  `{"theme": {"colors": {"accent": "75"}, "layout": {"rightPercent": 45}}}`,
			write: true,
			check: func(t *testing.T, c Config) {
				if got := c.Theme.Colors["accent"]; got != "75" {
					t.Errorf("colors.accent = %q, want %q", got, "75")
				}
				if c.Theme.Layout == nil {
					t.Fatal("layout not parsed")
				}
				if c.Theme.Layout.RightPercent != 45 {
					t.Errorf("layout.rightPercent = %d, want 45", c.Theme.Layout.RightPercent)
				}
				if c.Theme.Layout.LeftWidth != 0 || c.Theme.Layout.RightMin != 0 {
					t.Errorf("unset layout fields must stay zero: %+v", *c.Theme.Layout)
				}
			},
		},
		{
			name:  "module settings pass through as raw JSON",
			body:  `{"modules": {"jira-git": {"settings": {"jiraUrl":"https://jira.example"}}, "empty": {}}}`,
			write: true,
			check: func(t *testing.T, c Config) {
				got := string(c.Modules["jira-git"].Settings)
				want := `{"jiraUrl":"https://jira.example"}`
				if got != want {
					t.Errorf("settings = %q, want verbatim %q", got, want)
				}
				m, ok := c.Modules["empty"]
				if !ok {
					t.Fatal("module with no settings dropped")
				}
				if m.Settings != nil {
					t.Errorf("absent settings should stay nil, got %q", m.Settings)
				}
			},
		},
		{
			name:  "unknown fields ignored",
			body:  `{"future": true, "theme": {"colors": {"grey": "250"}}}`,
			write: true,
			check: func(t *testing.T, c Config) {
				if got := c.Theme.Colors["grey"]; got != "250" {
					t.Errorf("colors.grey = %q, want %q", got, "250")
				}
			},
		},
		{
			// PowerShell default encodings prepend a BOM; the file must
			// still parse (json.Unmarshal alone would reject it).
			name:  "UTF-8 BOM stripped",
			body:  "\xEF\xBB\xBF" + `{"theme": {"colors": {"accent": "75"}}}`,
			write: true,
			check: func(t *testing.T, c Config) {
				if got := c.Theme.Colors["accent"]; got != "75" {
					t.Errorf("colors.accent = %q, want %q (BOM not stripped?)", got, "75")
				}
			},
		},
		{
			name:  "UTF-16 yields zero value",
			body:  "\xFF\xFE{\x00}\x00",
			write: true,
			check: func(t *testing.T, c Config) {
				if !reflect.DeepEqual(c, Config{}) {
					t.Errorf("want zero Config, got %+v", c)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOUSTON_HOME", t.TempDir())
			if tt.write {
				if err := os.WriteFile(Path(), []byte(tt.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tt.check(t, Load())
		})
	}
}

// TestCheck covers what Load hides: doctor's view of why a config file is
// being ignored.
func TestCheck(t *testing.T) {
	tests := []struct {
		name    string
		body    string // "" with write=false means no file at all
		write   bool
		wantErr string // substring; "" = no error
	}{
		{name: "missing file is fine"},
		{name: "valid file is fine", body: `{"theme": {}}`, write: true},
		{name: "BOM alone is fine", body: "\xEF\xBB\xBF{}", write: true},
		{name: "malformed JSON reported", body: `{"theme": {`, write: true, wantErr: "unexpected end"},
		{name: "UTF-16 reported actionably", body: "\xFF\xFE{\x00}\x00", write: true, wantErr: "UTF-16"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOUSTON_HOME", t.TempDir())
			if tt.write {
				if err := os.WriteFile(Path(), []byte(tt.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := Check()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Check() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Check() = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
