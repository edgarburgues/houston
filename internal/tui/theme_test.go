package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"houston/internal/theme"
)

func TestPaneWidthClamps(t *testing.T) {
	tests := []struct {
		name                string
		lay                 theme.Layout
		width               int
		wantLeft, wantRight int
	}{
		{"defaults", theme.Default().Layout, 100, 26, 40},
		{"left clamped low", theme.Layout{LeftWidth: 5, RightPercent: 40, RightMin: 36}, 100, 16, 40},
		{"left clamped high", theme.Layout{LeftWidth: 500, RightPercent: 40, RightMin: 36}, 100, 60, 40},
		{"right floor kicks in", theme.Layout{LeftWidth: 26, RightPercent: 40, RightMin: 36}, 80, 26, 36},
		{"custom percent", theme.Layout{LeftWidth: 26, RightPercent: 50, RightMin: 36}, 100, 26, 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{lay: tt.lay, width: tt.width}
			if got := m.leftW(); got != tt.wantLeft {
				t.Errorf("leftW = %d, want %d", got, tt.wantLeft)
			}
			if got := m.rightW(); got != tt.wantRight {
				t.Errorf("rightW = %d, want %d", got, tt.wantRight)
			}
		})
	}
}

func TestApplyThemeRecolorsStyles(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(prev)
	defer applyTheme(theme.Default()) // other tests rely on the default palette

	th := theme.Default().Merge(theme.Overrides{Colors: map[string]string{"accent": "75"}})
	applyTheme(th)
	if out := headerStyle.Render("x"); !strings.Contains(out, "48;5;75") {
		t.Errorf("header background should use the themed accent, got %q", out)
	}
	if curLayout != th.Layout {
		t.Errorf("applyTheme must capture the layout: %+v", curLayout)
	}

	applyTheme(theme.Default())
	if out := headerStyle.Render("x"); !strings.Contains(out, "48;5;39") {
		t.Errorf("default accent should be restored, got %q", out)
	}
}
