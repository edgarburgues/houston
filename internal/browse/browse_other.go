//go:build !windows

package browse

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// openDefault opens the URL with the platform's default opener.
func openDefault(u string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", u).Start()
	}
	return exec.Command("xdg-open", u).Start()
}

// openPrivate opens the URL in a private window of the default browser.
// Linux: resolves the default handler via xdg-settings and maps the .desktop
// name to a binary + private flag. macOS: default-browser detection needs
// LaunchServices plist parsing, so it errors and the caller falls back to a
// normal open.
func openPrivate(u string) error {
	if runtime.GOOS == "darwin" {
		return fmt.Errorf("private-window launch not supported on darwin")
	}
	out, err := exec.Command("xdg-settings", "get", "default-web-browser").Output()
	if err != nil {
		return err
	}
	desktop := strings.ToLower(strings.TrimSpace(string(out))) // e.g. "firefox.desktop"
	flag := privateFlag(desktop)
	if flag == "" {
		return fmt.Errorf("default browser %q has no known private mode", desktop)
	}
	bin := ""
	for _, cand := range binCandidates(desktop) {
		if p, err := exec.LookPath(cand); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		return fmt.Errorf("could not locate the binary for %q", desktop)
	}
	return exec.Command(bin, flag, u).Start()
}

// binCandidates maps a .desktop handler name to likely executable names.
func binCandidates(desktop string) []string {
	switch {
	case strings.Contains(desktop, "brave"):
		return []string{"brave-browser", "brave"}
	case strings.Contains(desktop, "chromium"):
		return []string{"chromium", "chromium-browser"}
	case strings.Contains(desktop, "chrome"):
		return []string{"google-chrome", "google-chrome-stable"}
	case strings.Contains(desktop, "firefox"):
		return []string{"firefox"}
	case strings.Contains(desktop, "edge"):
		return []string{"microsoft-edge", "microsoft-edge-stable"}
	default:
		return nil
	}
}
