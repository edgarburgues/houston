package tui

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestGoldenDefaultTheme locks the rendered frames byte-for-byte: the theme
// extraction must not change a single cell of the default output. Regenerate
// with `go test ./internal/tui -run Golden -update` only when a deliberate
// visual change lands.
func TestGoldenDefaultTheme(t *testing.T) {
	// Pin everything the renderer consults outside the Model: the color
	// profile (tests have no TTY, so the default would be escape-free Ascii
	// and the golden would never see a color regression) and the local
	// timezone (viewMid and the preview format LastTime.Local()).
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(prev)
	prevTZ := time.Local
	time.Local = time.UTC
	defer func() { time.Local = prevTZ }()

	scenarios := []struct {
		name string
		msgs []tea.Msg
	}{
		{"missions", nil},
		{"missions-help", []tea.Msg{runes("?")}},
		{"missions-left-focus", []tea.Msg{key(tea.KeyTab)}},
		{"missions-palette", []tea.Msg{runes(":")}},
		{"accounts-empty", []tea.Msg{runes("A")}},
		{"notices-empty", []tea.Msg{runes("3")}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Setenv("HOUSTON_HOME", t.TempDir()) // accounts.Load must not see the real store
			m := drive(newModel(t), sc.msgs...)
			got := []byte(m.View())
			path := filepath.Join("testdata", sc.name+".golden")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing golden (run with -update): %v", err)
			}
			if !bytes.Equal(got, want) {
				i := 0
				for i < len(got) && i < len(want) && got[i] == want[i] {
					i++
				}
				t.Errorf("output differs from golden at byte %d\ngot  …%q\nwant …%q", i, tail(got, i), tail(want, i))
			}
		})
	}
}

// tail returns a short window of b around offset i for mismatch reports.
func tail(b []byte, i int) []byte {
	start, end := i-20, i+40
	if start < 0 {
		start = 0
	}
	if end > len(b) {
		end = len(b)
	}
	return b[start:end]
}
