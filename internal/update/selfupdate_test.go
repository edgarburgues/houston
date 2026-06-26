package update

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAssetName(t *testing.T) {
	cases := map[[2]string]string{
		{"windows", "amd64"}: "houston-windows-amd64.exe",
		{"windows", "arm64"}: "houston-windows-arm64.exe",
		{"darwin", "arm64"}:  "houston-darwin-arm64",
		{"linux", "amd64"}:   "houston-linux-amd64",
	}
	for in, want := range cases {
		if got := assetName(in[0], in[1]); got != want {
			t.Errorf("assetName(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestExpectedSHA(t *testing.T) {
	// sha256sum format: "<hex>␠␠<file>"
	body := []byte("abc123  houston-linux-amd64\nDEF456  houston-windows-amd64.exe\n")
	if got, ok := expectedSHA(body, "houston-linux-amd64"); !ok || got != "abc123" {
		t.Errorf("linux: got %q ok=%v, want abc123", got, ok)
	}
	// hex is lowercased so comparison against fmt %x matches
	if got, ok := expectedSHA(body, "houston-windows-amd64.exe"); !ok || got != "def456" {
		t.Errorf("windows: got %q ok=%v, want def456", got, ok)
	}
	if _, ok := expectedSHA(body, "houston-darwin-arm64"); ok {
		t.Error("missing file should not be found")
	}
}

func TestDownloadURL(t *testing.T) {
	t.Setenv("HOUSTON_REPO", "acme/houston")
	want := "https://github.com/acme/houston/releases/download/v1.2.3/checksums.txt"
	if got := downloadURL("v1.2.3", "checksums.txt"); got != want {
		t.Errorf("downloadURL = %q, want %q", got, want)
	}
}

// TestSwap replaces a fake binary with new content and checks that the original
// path ends up with the new bytes (exercises the Windows rename-aside path when
// run on Windows, the atomic-rename path elsewhere).
func TestSwap(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "houston")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	leftover, err := Swap(exe, []byte("NEW"))
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil || string(got) != "NEW" {
		t.Fatalf("after Swap exe = %q (err %v), want NEW", got, err)
	}
	// Nothing holds the temp file open, so the old copy should be gone.
	if leftover != "" {
		t.Errorf("unexpected leftover %q (old binary not removed)", leftover)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Errorf("temp .new file was not cleaned up")
	}
}
