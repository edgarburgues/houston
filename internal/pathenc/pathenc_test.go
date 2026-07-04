package pathenc

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEncode(t *testing.T) {
	cases := map[string]string{
		`C:\Users\TESTER`:                                   `C--Users-TESTER`,
		`C:\Users\TESTER\Documents\Github\Pokemon`:          `C--Users-TESTER-Documents-Github-Pokemon`,
		`C:\Users\TESTER\Documents\Github\Pokemon\08-copia`: `C--Users-TESTER-Documents-Github-Pokemon-08-copia`,
		// Claude encodes EVERY non-alphanumeric to '-', not just separators
		// (verified against real stores):
		`C:\Users\x\my.project`:                 `C--Users-x-my-project`,
		`C:\dir with spaces\a_b`:                `C--dir-with-spaces-a-b`,
		`\\10.66.77.20\Media\Series\South Park`: `--10-66-77-20-Media-Series-South-Park`,
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
		t.Skip("real folder not present; skipping the disk-dependent test")
	}
	if got := DecodeProjectDir(proj); got != want {
		t.Errorf("DecodeProjectDir(%q) = %q, want %q (08-copia must stay a single segment)", proj, got, want)
	}
}

// TestDecodePunctuationSegments builds real dirs whose names contain '.', ' '
// and '_' — all encoded as '-' — and checks the ReadDir-based decoder finds
// them (a join-with-hyphens reconstruction never could).
func TestDecodePunctuationSegments(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("DecodeProjectDir keys off the drive-root '--'; Windows only")
	}
	base := t.TempDir() // e.g. C:\Users\...\Temp\TestDecode...\001
	deep := filepath.Join(base, "my.project", "sub dir", "a_b-c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := DecodeProjectDir(Encode(deep)); got != deep {
		t.Errorf("DecodeProjectDir(Encode(%q)) = %q, want the original path", deep, got)
	}
}
