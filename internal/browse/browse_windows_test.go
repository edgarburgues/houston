//go:build windows

package browse

import "testing"

func TestExeFromCommand(t *testing.T) {
	cases := map[string]string{
		`"C:\Program Files\msedge.exe" --single-argument %1`: `C:\Program Files\msedge.exe`,
		`"C:\x\chrome.exe" -- "%1"`:                          `C:\x\chrome.exe`,
		`C:\x\firefox.exe -osint -url "%1"`:                  `C:\x\firefox.exe`,
		`C:\x\firefox.exe`:                                   `C:\x\firefox.exe`,
		`"unterminated`:                                      ``,
		``:                                                   ``,
	}
	for in, want := range cases {
		if got := exeFromCommand(in); got != want {
			t.Errorf("exeFromCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDefaultBrowserDetection exercises the real registry read on this
// machine; it only checks the chain doesn't error (the default browser may
// legitimately be an unknown family).
func TestDefaultBrowserDetection(t *testing.T) {
	progID, err := defaultProgID()
	if err != nil {
		t.Skipf("no UserChoice ProgId (unusual setup): %v", err)
	}
	cmd, err := progIDCommand(progID)
	if err != nil {
		t.Fatalf("progIDCommand(%q): %v", progID, err)
	}
	if exeFromCommand(cmd) == "" {
		t.Errorf("no executable extracted from %q", cmd)
	}
	t.Logf("default: %s → %s (private flag %q)", progID, cmd, privateFlag(progID))
}
