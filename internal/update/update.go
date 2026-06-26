// Package update checks GitHub Releases for a newer Houston and produces a short
// notice when one exists. The check is cached (default 24h) in the Houston data
// dir so normal use never waits on the network or hammers the GitHub API, and
// dev builds (version "dev"/"") never nag.
package update

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"houston/internal/accounts"
)

// Repo is the GitHub repo to check. Override with $HOUSTON_REPO (handy for forks).
func Repo() string {
	if r := os.Getenv("HOUSTON_REPO"); r != "" {
		return r
	}
	return "edgarburgues/houston"
}

// checkTTL is how long a "latest version" lookup is reused before re-querying.
const checkTTL = 24 * time.Hour

type cache struct {
	Latest string `json:"latest"`
	TS     int64  `json:"ts"` // unix seconds of last successful fetch
}

func cachePath() string { return filepath.Join(accounts.StoreDir(), "version-check.json") }

// Notice returns a one-line, user-facing update notice if a newer release exists,
// or "" when up to date, on a dev build, or if the check can't run. Safe to call
// on every launch: the network is only touched once per checkTTL.
func Notice(current string, timeout time.Duration) string {
	if current == "" || current == "dev" {
		return "" // unversioned/local build: don't nag
	}
	latest := cachedLatest(timeout)
	if latest == "" || !newer(latest, current) {
		return ""
	}
	return fmt.Sprintf("nueva versión %s disponible (tienes %s) — actualiza: https://github.com/%s/releases/latest",
		latest, current, Repo())
}

// cachedLatest returns the latest release tag, querying GitHub at most once per
// checkTTL and serving the cached value otherwise. "" if never fetched and the
// network lookup fails.
func cachedLatest(timeout time.Duration) string {
	var c cache
	if b, err := os.ReadFile(cachePath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if c.Latest != "" && time.Now().Unix()-c.TS < int64(checkTTL.Seconds()) {
		return c.Latest // still fresh
	}
	if tag := fetchLatest(timeout); tag != "" {
		writeCache(cache{Latest: tag, TS: time.Now().Unix()})
		return tag
	}
	return c.Latest // network failed: fall back to whatever we had (maybe "")
}

func fetchLatest(timeout time.Duration) string {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", Repo())
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "houston-update-check") // GitHub requires a UA
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return ""
	}
	return r.TagName
}

func writeCache(c cache) {
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	p := cachePath()
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// newer reports whether version a is strictly newer than b. Both may carry a
// leading "v"; comparison is field-by-field numeric (1.10.0 > 1.9.0), with
// non-numeric/missing fields treated as 0. Unparseable input → false (no nag).
func newer(a, b string) bool {
	pa, pb := parse(a), parse(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parse(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// drop any pre-release/build suffix (e.g. 1.2.3-rc1)
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, f := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		out[i], _ = strconv.Atoi(strings.TrimSpace(f))
	}
	return out
}
