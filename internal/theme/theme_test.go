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
	// defaults < modules in lex order (later wins per field) < user, merged
	// field-wise; invalid values are skipped per-field, never per-layer.
	tests := []struct {
		name    string
		modules []Overrides
		user    Overrides
		want    func(d Theme) Theme
	}{
		{
			name: "no overrides yields the defaults",
			want: func(d Theme) Theme { return d },
		},
		{
			name:    "module override beats the default",
			modules: []Overrides{{Colors: map[string]string{"accent": "75"}}},
			want:    func(d Theme) Theme { d.Colors.Accent = "75"; return d },
		},
		{
			name: "later module wins per field, earlier fields survive",
			modules: []Overrides{
				{Colors: map[string]string{"accent": "75", "green": "34"}},
				{Colors: map[string]string{"accent": "99"}},
			},
			want: func(d Theme) Theme { d.Colors.Accent = "99"; d.Colors.Green = "34"; return d },
		},
		{
			name: "user trumps every module, untouched fields keep module values",
			modules: []Overrides{
				{Colors: map[string]string{"accent": "75", "green": "34"}},
				{Colors: map[string]string{"accent": "99"}},
			},
			user: Overrides{Colors: map[string]string{"green": "40"}},
			want: func(d Theme) Theme { d.Colors.Accent = "99"; d.Colors.Green = "40"; return d },
		},
		{
			name: "invalid module color skipped per-field, valid sibling applies",
			modules: []Overrides{
				{Colors: map[string]string{"accent": "not-a-color", "yellow": "208"}},
			},
			want: func(d Theme) Theme { d.Colors.Yellow = "208"; return d },
		},
		{
			name:    "invalid user color leaves the module's value standing",
			modules: []Overrides{{Colors: map[string]string{"accent": "75"}}},
			user:    Overrides{Colors: map[string]string{"accent": "256"}},
			want:    func(d Theme) Theme { d.Colors.Accent = "75"; return d },
		},
		{
			name:    "layout follows the same chain field-wise",
			modules: []Overrides{{Layout: layout(30, 50, 0)}},
			user:    Overrides{Layout: layout(0, 45, 0)},
			want: func(d Theme) Theme {
				d.Layout.LeftWidth = 30    // module, untouched by user
				d.Layout.RightPercent = 45 // user trumps module's 50
				return d
			},
		},
		{
			name:    "out-of-range user layout value keeps the module's",
			modules: []Overrides{{Layout: layout(0, 50, 0)}},
			user:    Overrides{Layout: layout(0, 200, 40)},
			want:    func(d Theme) Theme { d.Layout.RightPercent = 50; d.Layout.RightMin = 40; return d },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.modules, tt.user)
			if want := tt.want(Default()); !reflect.DeepEqual(got, want) {
				t.Errorf("Resolve = %+v, want %+v", got, want)
			}
		})
	}
}
