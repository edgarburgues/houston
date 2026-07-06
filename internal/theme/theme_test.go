package theme

import (
	"reflect"
	"testing"
)

func layout(l, p, m int) *struct{ LeftWidth, RightPercent, RightMin int } {
	return &struct{ LeftWidth, RightPercent, RightMin int }{l, p, m}
}

func TestDefaultLiterals(t *testing.T) {
	// These are the values the TUI and statusline shipped with; changing any
	// of them is a visible break, so pin them explicitly.
	d := Default()
	wantC := Colors{
		Accent: "39", Grey: "245", Dim: "240", Green: "42", Yellow: "220", SelBg: "236",
		SLGreen: "42", SLAmber: "214", SLRed: "203", SLDim: "240", SLActive: "45",
	}
	if d.Colors != wantC {
		t.Errorf("Default colors = %+v, want %+v", d.Colors, wantC)
	}
	if want := (Layout{LeftWidth: 26, RightPercent: 40, RightMin: 36}); d.Layout != want {
		t.Errorf("Default layout = %+v, want %+v", d.Layout, want)
	}
}

func TestMerge(t *testing.T) {
	tests := []struct {
		name string
		ov   Overrides
		want func(d Theme) Theme
	}{
		{
			name: "zero-value overrides are a no-op",
			ov:   Overrides{},
			want: func(d Theme) Theme { return d },
		},
		{
			name: "color override applies",
			ov:   Overrides{Colors: map[string]string{"accent": "75"}},
			want: func(d Theme) Theme { d.Colors.Accent = "75"; return d },
		},
		{
			name: "color keys match case-insensitively",
			ov:   Overrides{Colors: map[string]string{"selBg": "17", "SLAMBER": "208"}},
			want: func(d Theme) Theme { d.Colors.SelBg = "17"; d.Colors.SLAmber = "208"; return d },
		},
		{
			name: "invalid colors skipped field-wise, valid ones kept",
			ov: Overrides{Colors: map[string]string{
				"accent": "blue", "grey": "256", "dim": "-1", "green": "007", "yellow": "208",
			}},
			want: func(d Theme) Theme { d.Colors.Yellow = "208"; return d },
		},
		{
			name: "unknown color keys ignored",
			ov:   Overrides{Colors: map[string]string{"magenta": "13"}},
			want: func(d Theme) Theme { return d },
		},
		{
			name: "layout override applies",
			ov:   Overrides{Layout: layout(30, 45, 40)},
			want: func(d Theme) Theme { d.Layout = Layout{LeftWidth: 30, RightPercent: 45, RightMin: 40}; return d },
		},
		{
			name: "unset (zero) layout fields keep defaults",
			ov:   Overrides{Layout: layout(0, 45, 0)},
			want: func(d Theme) Theme { d.Layout.RightPercent = 45; return d },
		},
		{
			name: "out-of-range layout values skipped field-wise",
			ov:   Overrides{Layout: layout(-5, 200, 40)},
			want: func(d Theme) Theme { d.Layout.RightMin = 40; return d },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Default().Merge(tt.ov)
			if want := tt.want(Default()); !reflect.DeepEqual(got, want) {
				t.Errorf("Merge = %+v, want %+v", got, want)
			}
		})
	}
}

func TestMergeDoesNotMutateReceiver(t *testing.T) {
	d := Default()
	d.Merge(Overrides{Colors: map[string]string{"accent": "75"}, Layout: layout(30, 0, 0)})
	if !reflect.DeepEqual(d, Default()) {
		t.Errorf("Merge mutated its receiver: %+v", d)
	}
}

func TestResolvePrecedence(t *testing.T) {
	// defaults < modules in lex order (later wins per field) < user.
	modA := Overrides{Colors: map[string]string{"accent": "75", "green": "34"}}
	modB := Overrides{Colors: map[string]string{"accent": "99"}}
	user := Overrides{Colors: map[string]string{"green": "40"}, Layout: layout(0, 45, 0)}

	got := Resolve([]Overrides{modA, modB}, user)
	if got.Colors.Accent != "99" {
		t.Errorf("later module should win per field: accent = %q, want 99", got.Colors.Accent)
	}
	if got.Colors.Green != "40" {
		t.Errorf("user must trump modules: green = %q, want 40", got.Colors.Green)
	}
	if got.Layout.RightPercent != 45 || got.Layout.LeftWidth != 26 {
		t.Errorf("layout merge wrong: %+v", got.Layout)
	}
	if got.Colors.Grey != "245" {
		t.Errorf("untouched fields keep defaults: grey = %q", got.Colors.Grey)
	}
	if empty := Resolve(nil, Overrides{}); !reflect.DeepEqual(empty, Default()) {
		t.Errorf("Resolve with no overrides must equal Default, got %+v", empty)
	}
}
