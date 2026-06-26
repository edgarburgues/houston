// Package statusline renders Claude Code's status line for Houston: it shows the
// ACTIVE account (derived from CLAUDE_CONFIG_DIR) and the 5h/7d quota of EVERY
// Houston account — not just the one you launched — so you can see at a glance
// which account has headroom while you work.
//
// The active account's quota comes live from the status-line JSON Claude pipes on
// stdin (rate_limits); the other accounts are probed against the usage endpoint
// and cached briefly (the status line runs often, so a network probe every render
// would be wasteful and slow). Wire it up in the shared settings.json:
//
//	{ "statusLine": { "type": "command", "command": "houston statusline" } }
//
// No jq or external dependency: the Houston binary parses the JSON itself.
package statusline

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"houston/internal/accounts"
	"houston/internal/usage"
)

// cacheTTL is how long a probe of the non-active accounts is reused before the
// status line re-probes. Short enough to feel live, long enough to stay snappy.
const cacheTTL = 60 * time.Second

// probeTimeout caps the per-render network wait when the cache is stale.
const probeTimeout = 4 * time.Second

// input is the subset of the status-line JSON we use. rate_limits appears only
// for Claude.ai subscribers and only after the first API response, so every field
// is optional and rendered defensively.
type input struct {
	Model struct {
		DisplayName string `json:"display_name"`
	} `json:"model"`
	ContextWindow struct {
		UsedPercentage *float64 `json:"used_percentage"`
	} `json:"context_window"`
	RateLimits struct {
		FiveHour *window `json:"five_hour"`
		SevenDay *window `json:"seven_day"`
	} `json:"rate_limits"`
}

type window struct {
	UsedPercentage float64 `json:"used_percentage"`
}

// row is one account's line in the status line.
type row struct {
	ID     string
	U5, U7 float64
	OK     bool
	Active bool
}

// Line is the entry point for the `houston statusline` command: it reads the
// status-line JSON from r, figures out the active account from configDir, gathers
// every account's quota (cached probe + live override for the active one) and
// returns the formatted status line.
func Line(r io.Reader, configDir string) string {
	var in input
	if b, err := io.ReadAll(r); err == nil {
		_ = json.Unmarshal(b, &in)
	}
	active := accountID(configDir)
	accs, _ := accounts.Load()

	probes := cachedProbes(accs)
	rows := make([]row, 0, len(accs))
	for _, a := range accs {
		rw := row{ID: a.ID, Active: a.ID == active}
		if p, ok := probes[a.ID]; ok {
			rw.U5, rw.U7, rw.OK = p.U5, p.U7, p.OK
		}
		// The active account's live rate_limits (from stdin) beat the cached probe.
		if rw.Active && in.RateLimits.FiveHour != nil {
			rw.U5, rw.OK = in.RateLimits.FiveHour.UsedPercentage, true
			if in.RateLimits.SevenDay != nil {
				rw.U7 = in.RateLimits.SevenDay.UsedPercentage
			}
		}
		rows = append(rows, rw)
	}

	var ctx *float64
	if in.ContextWindow.UsedPercentage != nil {
		ctx = in.ContextWindow.UsedPercentage
	}
	return Render(rows, in.Model.DisplayName, ctx)
}

// Render formats the status line: 🚀, then each account as "id 5h/7d" (the active
// one marked with ►), then the model and context usage if known. Pure (no I/O) so
// it's testable without the network.
func Render(rows []row, model string, ctxPct *float64) string {
	var parts []string
	for _, r := range rows {
		mark := ""
		if r.Active {
			mark = "►"
		}
		if r.OK {
			parts = append(parts, fmt.Sprintf("%s%s %.0f/%.0f", mark, r.ID, r.U5, r.U7))
		} else {
			parts = append(parts, fmt.Sprintf("%s%s —", mark, r.ID))
		}
	}
	line := "🚀 5h/7d"
	if len(parts) > 0 {
		line += " " + strings.Join(parts, " · ")
	}
	if model != "" {
		line += " · " + model
	}
	if ctxPct != nil {
		line += fmt.Sprintf(" · ctx %.0f%%", *ctxPct)
	}
	return line
}

// --- cached probe ----------------------------------------------------------

type cacheEntry struct {
	U5 float64 `json:"u5"`
	U7 float64 `json:"u7"`
	OK bool    `json:"ok"`
	TS int64   `json:"ts"` // unix seconds
}

func cachePath() string { return filepath.Join(accounts.StoreDir(), "usage-cache.json") }

// cachedProbes returns each account's last-known usage, re-probing the endpoint
// only when the cache is missing or older than cacheTTL. Probing every render
// would hammer the endpoint and stall the status line.
func cachedProbes(accs []accounts.Account) map[string]usage.Probe {
	cache := map[string]cacheEntry{}
	if b, err := os.ReadFile(cachePath()); err == nil {
		_ = json.Unmarshal(b, &cache)
	}
	now := time.Now().Unix()
	stale := false
	for _, a := range accs {
		e, ok := cache[a.ID]
		if !ok || now-e.TS > int64(cacheTTL.Seconds()) {
			stale = true
			break
		}
	}
	if stale && len(accs) > 0 {
		fresh := map[string]cacheEntry{}
		for _, p := range usage.ProbeAll(accs, probeTimeout) {
			if !p.OK {
				// Keep the last-known-good value through a transient failure (e.g. an
				// HTTP 429 from probing too often) instead of blanking the account.
				// New timestamp so we don't immediately re-probe and 429 again.
				if prev, ok := cache[p.Account.ID]; ok && prev.OK {
					fresh[p.Account.ID] = cacheEntry{U5: prev.U5, U7: prev.U7, OK: true, TS: now}
					continue
				}
			}
			fresh[p.Account.ID] = cacheEntry{U5: p.U5, U7: p.U7, OK: p.OK, TS: now}
		}
		cache = fresh
		writeCache(cache)
	}
	out := map[string]usage.Probe{}
	for id, e := range cache {
		out[id] = usage.Probe{U5: e.U5, U7: e.U7, OK: e.OK}
	}
	return out
}

func writeCache(c map[string]cacheEntry) {
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	p := cachePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// --- helpers ---------------------------------------------------------------

// accountID derives the short account id from the config dir basename
// (".../account-work2" → "work2"). Returns "" for the default ~/.claude dir.
func accountID(configDir string) string {
	if configDir == "" {
		return ""
	}
	base := filepath.Base(configDir)
	if strings.HasPrefix(base, "account-") {
		return strings.TrimPrefix(base, "account-")
	}
	if base == ".claude" {
		return ""
	}
	return base
}
