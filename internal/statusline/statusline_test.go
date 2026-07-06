package statusline

import (
	"path/filepath"
	"strings"
	"testing"

	"houston/internal/theme"
	"houston/internal/usage"
)

func TestAccountID(t *testing.T) {
	cases := map[string]string{
		filepath.Join("x", "account-work2"): "work2",
		filepath.Join("x", "account-work"):  "work",
		"":                                  "",
	}
	for in, want := range cases {
		if got := accountID(in); got != want {
			t.Errorf("accountID(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestMergeProbe(t *testing.T) {
	now := int64(1_000_000)
	good := cacheEntry{U5: 41, U7: 7, OK: true, TS: now - 120, GoodTS: now - 120}

	// success: refreshes value and GoodTS
	if e := mergeProbe(good, usage.Probe{U5: 55, U7: 9, OK: true}, now); !e.OK || e.U5 != 55 || e.GoodTS != now {
		t.Errorf("success should refresh value and GoodTS: %+v", e)
	}
	// recent failure (< keepGoodMax): keeps the last good value and its GoodTS
	if e := mergeProbe(good, usage.Probe{OK: false}, now); !e.OK || e.U5 != 41 || e.GoodTS != good.GoodTS {
		t.Errorf("transient failure should keep the last good value: %+v", e)
	}
	// persistent failure (> keepGoodMax since the last success): goes off
	stale := cacheEntry{U5: 41, U7: 7, OK: true, TS: now - 30, GoodTS: now - int64(keepGoodMax.Seconds()) - 1}
	if e := mergeProbe(stale, usage.Probe{OK: false}, now); e.OK {
		t.Errorf("an expired good value should show as off: %+v", e)
	}
	// legacy entry without GoodTS: uses TS as the last-success time
	legacy := cacheEntry{U5: 41, OK: true, TS: now - 60}
	if e := mergeProbe(legacy, usage.Probe{OK: false}, now); !e.OK || e.GoodTS != legacy.TS {
		t.Errorf("legacy entry should expire by its TS: %+v", e)
	}
	// no previous entry and a failure: off
	if e := mergeProbe(cacheEntry{}, usage.Probe{OK: false}, now); e.OK {
		t.Errorf("failure with no history should be off: %+v", e)
	}
}

func TestLevelColor(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{0, cGreen}, {49, cGreen}, {50, cAmber}, {79, cAmber}, {80, cRed}, {100, cRed},
	}
	for _, c := range cases {
		if got := levelColor(c.pct); got != c.want {
			t.Errorf("levelColor(%v) = %q, want %q", c.pct, got, c.want)
		}
	}
}

func TestBarCells(t *testing.T) {
	// width is always preserved: len(filled runes)+len(empty runes) accounts for
	// every cell (the partial boundary cell counts as one filled rune).
	cases := []struct {
		pct                   float64
		wantFilled, wantEmpty string
	}{
		{0, "", "░░░░░░░░"},
		{100, "████████", ""},
		{50, "████", "░░░░"},    // 50% of 8 = exactly 4 cells
		{56.25, "████▌", "░░░"}, // 4.5 cells → 4 full + a half block
		{200, "████████", ""},   // clamped
		{-5, "", "░░░░░░░░"},    // clamped
	}
	for _, c := range cases {
		f, e := barCells(c.pct, barWidth)
		if f != c.wantFilled || e != c.wantEmpty {
			t.Errorf("barCells(%v) = (%q,%q), want (%q,%q)", c.pct, f, e, c.wantFilled, c.wantEmpty)
		}
		if len([]rune(f))+len([]rune(e)) != barWidth {
			t.Errorf("barCells(%v) total cells = %d, want %d", c.pct, len([]rune(f))+len([]rune(e)), barWidth)
		}
	}
}

func TestRenderPlain(t *testing.T) {
	t.Setenv("NO_COLOR", "1") // deterministic, escape-code-free output
	ctx := 12.0
	rows := []row{
		{ID: "work", U5: 12, U7: 3, OK: true},
		{ID: "work2", U5: 41, U7: 7, OK: true, Active: true},
		{ID: "work3", OK: false}, // not logged in / probe failed
	}
	got := Render(rows, "Opus 4.8", &ctx)
	for _, want := range []string{
		"work ▕", "12%", // first account: id + bar + percent
		"►work2 ▕", "41%", // active account marked and shown
		"work3 ▕", "off", // failed account: empty bar + off
		" │ ", // separators between segments
		"Opus 4.8", "ctx 12%",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, "🚀") {
		t.Errorf("rocket should be gone: %q", got)
	}
	if strings.Count(got, "►") != 1 {
		t.Errorf("exactly one active marker expected: %q", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("NO_COLOR set but ANSI codes present: %q", got)
	}
}

func TestRenderColorEmitsANSI(t *testing.T) {
	t.Setenv("NO_COLOR", "") // force color on
	rows := []row{{ID: "work2", U5: 41, OK: true, Active: true}}
	got := Render(rows, "", nil)
	if !strings.Contains(got, "\x1b[") {
		t.Errorf("expected ANSI color codes when NO_COLOR is unset: %q", got)
	}
	// id and percent stay contiguous (not split by codes) so they remain greppable
	if !strings.Contains(got, "►work2") || !strings.Contains(got, "41%") {
		t.Errorf("id/percent should remain intact: %q", got)
	}
}

func TestApplyThemeDefaultEscapes(t *testing.T) {
	// Golden: init seeds the vars with the exact escapes the pre-theme
	// constants carried, so untouched setups render byte-identical lines.
	applyTheme(theme.Default())
	cases := map[string]string{
		cGreen:  "\x1b[38;5;42m",
		cAmber:  "\x1b[38;5;214m",
		cRed:    "\x1b[38;5;203m",
		cDim:    "\x1b[38;5;240m",
		cActive: "\x1b[38;5;45m",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("default escape = %q, want %q", got, want)
		}
	}
}

func TestApplyThemeRecolorsRender(t *testing.T) {
	t.Setenv("NO_COLOR", "") // force color on
	defer applyTheme(theme.Default())

	applyTheme(theme.Default().Merge(theme.Overrides{
		Colors: map[string]string{"slactive": "99", "slgreen": "119"},
	}))
	rows := []row{{ID: "work2", U5: 12, OK: true, Active: true}}
	got := Render(rows, "", nil)
	if !strings.Contains(got, "\x1b[38;5;99m") {
		t.Errorf("active marker should use the themed slactive: %q", got)
	}
	if !strings.Contains(got, "\x1b[38;5;119m") {
		t.Errorf("low-usage bar should use the themed slgreen: %q", got)
	}
	if strings.Contains(got, "\x1b[38;5;45m") || strings.Contains(got, "\x1b[38;5;42m") {
		t.Errorf("default escapes should be fully replaced: %q", got)
	}
}

func TestNoColorTrumpsTheme(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	defer applyTheme(theme.Default())

	applyTheme(theme.Default().Merge(theme.Overrides{
		Colors: map[string]string{"slactive": "99"},
	}))
	rows := []row{{ID: "work2", U5: 41, OK: true, Active: true}}
	if got := Render(rows, "Opus 4.8", nil); strings.Contains(got, "\x1b[") {
		t.Errorf("NO_COLOR must strip themed escapes too: %q", got)
	}
}

func TestRenderNoAccounts(t *testing.T) {
	// With no accounts and no model the line is empty (no rocket placeholder);
	// a model alone still renders.
	if got := Render(nil, "", nil); got != "" {
		t.Errorf("empty input should render empty, got %q", got)
	}
	if got := Render(nil, "Opus 4.8", nil); !strings.Contains(got, "Opus 4.8") {
		t.Errorf("model alone should still render: %q", got)
	}
}
