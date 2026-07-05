// Package browse opens URLs on behalf of the claude child process. Houston
// injects itself as the child's $BROWSER (see launch.Cmd): Claude Code invokes
// $BROWSER with the URL as its only argument for EVERY link it opens — the
// OAuth login page included. Login URLs are opened in a PRIVATE window of the
// default browser, so the login never inherits the signed-in claude.ai
// session of your normal browsing (crucial when juggling several accounts);
// every other URL opens normally.
package browse

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// IsHTTP reports whether s is a plain http(s) URL — the shape $BROWSER mode
// accepts. Anything else (a directory path, a flag) is not for us.
func IsHTTP(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// CleanURL strips whitespace and one level of surrounding quotes. Claude Code
// pre-quotes the URL it hands to $BROWSER (`"https://…"`); depending on the
// platform's argv round-trip the quotes may arrive as literal characters.
func CleanURL(raw string) string {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return s
}

// IsLogin reports whether u is an Anthropic OAuth page — the URLs worth
// isolating in a private window. Kept tight (host + /oauth path) so ordinary
// claude.ai links keep opening in the normal browser session.
func IsLogin(s string) bool {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	anthropic := host == "claude.ai" || strings.HasSuffix(host, ".claude.ai") ||
		host == "console.anthropic.com" || host == "anthropic.com" || strings.HasSuffix(host, ".anthropic.com")
	return anthropic && strings.HasPrefix(u.EscapedPath(), "/oauth")
}

// Open dispatches a URL coming from the claude child: login pages go to a
// private window (falling back to a normal open if the default browser isn't
// recognized), everything else to the default opener. Set
// HOUSTON_LOGIN_PRIVATE=off to disable the private-window behavior.
func Open(raw string) error {
	u := CleanURL(raw)
	if !IsHTTP(u) {
		return fmt.Errorf("not an http(s) url: %q", raw)
	}
	if IsLogin(u) && os.Getenv("HOUSTON_LOGIN_PRIVATE") != "off" {
		if err := openPrivate(u); err == nil {
			return nil
		}
		// Unrecognized/undetectable default browser: a normal open still logs
		// you in — just without the isolation.
	}
	return openDefault(u)
}

// privateFlag is the private-window switch per browser family — the same
// table Claude Code's bundled `open` package uses.
func privateFlag(id string) string {
	id = strings.ToLower(id)
	switch {
	case strings.Contains(id, "brave"):
		return "--incognito"
	case strings.Contains(id, "chrom"): // chrome, chromium
		return "--incognito"
	case strings.Contains(id, "firefox"):
		return "--private-window"
	case strings.Contains(id, "edge"):
		return "--inprivate"
	default:
		return ""
	}
}
