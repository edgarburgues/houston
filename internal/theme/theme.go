// Package theme defines Houston's TUI palette and pane layout plus the
// override shape shared by config.json and module manifests.
package theme

import (
	"strconv"
	"strings"
)

// Colors are ANSI-256 codes kept as strings so they slot straight into
// lipgloss.Color and "\x1b[38;5;<code>m" without conversion.
type Colors struct {
	Accent, Grey, Dim, Green, Yellow, SelBg  string // TUI palette
	SLGreen, SLAmber, SLRed, SLDim, SLActive string // statusline palette
}

// Layout drives the three-pane geometry: a fixed left pane, a right pane
// sized as a percentage of the terminal with a floor, and the middle pane
// taking the rest. Final clamps live at the point of use in the TUI.
type Layout struct {
	LeftWidth, RightPercent, RightMin int
}

type Theme struct {
	Colors Colors
	Layout Layout
}

// Overrides is the sparse customization shape users (config.json) and modules
// (manifest "theme") provide: ANSI-256 color codes as strings plus optional
// pane widths. Invalid or unknown values are skipped field-wise at merge time,
// never rejected wholesale — a typo in one color must not discard the rest.
type Overrides struct {
	Colors map[string]string                                `json:"colors,omitempty"`
	Layout *struct{ LeftWidth, RightPercent, RightMin int } `json:"layout,omitempty"`
}

// Default returns the built-in theme — the exact literals the TUI and the
// statusline shipped with before themes existed, so an empty config renders
// byte-identical output.
func Default() Theme {
	return Theme{
		Colors: Colors{
			Accent: "39", Grey: "245", Dim: "240", Green: "42", Yellow: "220", SelBg: "236",
			SLGreen: "42", SLAmber: "214", SLRed: "203", SLDim: "240", SLActive: "45",
		},
		Layout: Layout{LeftWidth: 26, RightPercent: 40, RightMin: 36},
	}
}

// Merge layers o over t and returns the result; t is never mutated. Unknown
// color keys, non-0-255 color values and out-of-range layout values are
// skipped field-wise (zero layout fields mean "not set").
func (t Theme) Merge(o Overrides) Theme {
	for k, v := range o.Colors {
		if !validColor(v) {
			continue
		}
		if p := t.Colors.field(k); p != nil {
			*p = v
		}
	}
	if o.Layout != nil {
		if w := o.Layout.LeftWidth; w > 0 {
			t.Layout.LeftWidth = w
		}
		if p := o.Layout.RightPercent; p > 0 && p <= 100 {
			t.Layout.RightPercent = p
		}
		if w := o.Layout.RightMin; w > 0 {
			t.Layout.RightMin = w
		}
	}
	return t
}

// Resolve layers overrides onto the defaults: built-ins < module themes in
// lexicographic module-name order (callers pass them pre-sorted, later wins
// per field) < the user's config.json — installed code must never override
// the user's explicit taste.
func Resolve(moduleOv []Overrides, user Overrides) Theme {
	t := Default()
	for _, o := range moduleOv {
		t = t.Merge(o)
	}
	return t.Merge(user)
}

// field maps a JSON color key (matched case-insensitively, so "selbg" and
// "selBg" both work) to its struct field; nil for unknown keys.
func (c *Colors) field(key string) *string {
	switch strings.ToLower(key) {
	case "accent":
		return &c.Accent
	case "grey":
		return &c.Grey
	case "dim":
		return &c.Dim
	case "green":
		return &c.Green
	case "yellow":
		return &c.Yellow
	case "selbg":
		return &c.SelBg
	case "slgreen":
		return &c.SLGreen
	case "slamber":
		return &c.SLAmber
	case "slred":
		return &c.SLRed
	case "sldim":
		return &c.SLDim
	case "slactive":
		return &c.SLActive
	}
	return nil
}

// validColor accepts ANSI-256 numeric codes only ("0".."255").
func validColor(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 0 && n <= 255 && s == strconv.Itoa(n)
}
