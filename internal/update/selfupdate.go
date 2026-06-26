package update

// Self-update: download the release binary for this platform from GitHub
// Releases, verify it against the release's checksums.txt, and swap it in over
// the running binary. The orchestration/UX (confirmation, "close your other
// terminals" warning) lives in main.cmdUpdate; this file is just the mechanics.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// assetName is the release asset for a platform, matching the names produced by
// .github/workflows/release.yml (houston-<os>-<arch>, .exe on Windows).
func assetName(goos, goarch string) string {
	n := fmt.Sprintf("houston-%s-%s", goos, goarch)
	if goos == "windows" {
		n += ".exe"
	}
	return n
}

// downloadURL is the public release-download URL for a file in a given tag.
func downloadURL(tag, file string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", Repo(), tag, file)
}

// FetchLatest returns the newest release tag, bypassing the 24h cache used by
// Notice. "" if the lookup fails.
func FetchLatest(timeout time.Duration) string { return fetchLatest(timeout) }

// Newer reports whether tag a is strictly newer than tag b (exported wrapper).
func Newer(a, b string) bool { return newer(a, b) }

func httpGet(url string, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "houston-update") // GitHub requires a UA
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

// expectedSHA finds the lowercase sha256 hex for file in a checksums.txt body
// ("<hex>␠␠<filename>" per line, the sha256sum format used by release.yml).
func expectedSHA(checksums []byte, file string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[1] == file {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

// DownloadVerified fetches this platform's binary for tag and verifies it
// against the release's checksums.txt. Returns the verified bytes and the asset
// name. Any mismatch or missing checksum is an error (never returns bad bytes).
func DownloadVerified(tag string, timeout time.Duration) (bin []byte, file string, err error) {
	file = assetName(runtime.GOOS, runtime.GOARCH)
	sums, err := httpGet(downloadURL(tag, "checksums.txt"), timeout)
	if err != nil {
		return nil, file, fmt.Errorf("could not download checksums.txt: %w", err)
	}
	want, ok := expectedSHA(sums, file)
	if !ok {
		return nil, file, fmt.Errorf("release %s has no checksum for %s", tag, file)
	}
	bin, err = httpGet(downloadURL(tag, file), timeout)
	if err != nil {
		return nil, file, fmt.Errorf("could not download %s: %w", file, err)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(bin))
	if got != want {
		return nil, file, fmt.Errorf("checksum mismatch for %s (want %s, got %s)", file, want, got)
	}
	return bin, file, nil
}

// Swap replaces the binary at exePath with newBin. On Windows a running .exe
// can't be overwritten, so the in-use binary is renamed aside (<exe>.old) first
// and then the new one is moved into place; the old copy is removed if free, or
// reported as leftover (CleanupStale collects it on a later run). On Unix the
// replace is a single atomic rename. On any failure the original is restored.
func Swap(exePath string, newBin []byte) (leftover string, err error) {
	tmp := exePath + ".new"
	if err = os.WriteFile(tmp, newBin, 0o755); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		if err = os.Rename(tmp, exePath); err != nil {
			_ = os.Remove(tmp)
			return "", err
		}
		return "", nil
	}
	old := exePath + ".old"
	_ = os.Remove(old) // clear a prior leftover if it's now free
	if err = os.Rename(exePath, old); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("could not move the in-use binary aside: %w", err)
	}
	if err = os.Rename(tmp, exePath); err != nil {
		_ = os.Rename(old, exePath) // rollback
		_ = os.Remove(tmp)
		return "", err
	}
	if os.Remove(old) != nil {
		leftover = old // still locked by another running houston; cleaned later
	}
	return leftover, nil
}

// CleanupStale best-effort removes binaries left aside by a previous Windows
// update once they're no longer locked. Safe and silent to call on startup.
func CleanupStale(exePath string) {
	for _, suffix := range []string{".old", ".oldlocked"} {
		_ = os.Remove(exePath + suffix)
	}
}
