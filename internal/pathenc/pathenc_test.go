package pathenc

import "testing"

func TestEncode(t *testing.T) {
	cases := map[string]string{
		`C:\Users\TESTER`:                              `C--Users-TESTER`,
		`C:\Users\TESTER\Documents\Github\Pokemon`:     `C--Users-TESTER-Documents-Github-Pokemon`,
		`C:\Users\TESTER\Documents\Github\Pokemon\08-copia`: `C--Users-TESTER-Documents-Github-Pokemon-08-copia`,
	}
	for in, want := range cases {
		if got := Encode(in); got != want {
			t.Errorf("Encode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolvePrefersMatchingCwd(t *testing.T) {
	// 650953a6 case: first cwd encodes to project, last does not -> use first.
	proj := `C--Users-TESTER-Documents-Github-Pokemon`
	got := ResolveResumeDir(
		`C:\Users\TESTER\Documents\Github\Pokemon`,
		`C:\Users\TESTER\Documents\Github\Pokemon\10-repos`,
		proj,
	)
	want := `C:\Users\TESTER\Documents\Github\Pokemon`
	if got != want {
		t.Errorf("ResolveResumeDir = %q, want %q", got, want)
	}
}

// TestDecodeHyphenSegment depends on the real folder existing on this machine.
func TestDecodeHyphenSegment(t *testing.T) {
	proj := `C--Users-TESTER-Documents-Github-Pokemon-08-copia`
	want := `C:\Users\TESTER\Documents\Github\Pokemon\08-copia`
	if !isDir(want) {
		t.Skip("carpeta real no presente; salto la prueba dependiente de disco")
	}
	if got := DecodeProjectDir(proj); got != want {
		t.Errorf("DecodeProjectDir(%q) = %q, want %q (08-copia debe quedar como un solo segmento)", proj, got, want)
	}
}
