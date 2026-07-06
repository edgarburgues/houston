// Package statusline renders Claude Code's status line for Houston: a colored
// usage bar for EVERY Houston account — not just the one you launched — with the
// ACTIVE account (derived from CLAUDE_CONFIG_DIR) marked, so you can see at a
// glance which account has headroom. The bar tracks the 5h window (the limit you
// hit first), colored green/amber/red by how full it is.
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
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"houston/internal/accounts"
	"houston/internal/config"
	"houston/internal/flock"
	"houston/internal/theme"
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
	// User overrides only for now; enabled-module theme contributions join the
	// chain (via theme.Resolve) once the module loader exists. Applied before
	// any rendering — the statusline is a fresh single-threaded process, so
	// mutating the color vars here can never race.
	applyTheme(theme.Default().Merge(config.Load().Theme))

	var in input
	if b, err := io.ReadAll(r); err == nil {
		_ = json.Unmarshal(b, &in)
	}
	accs, _ := accounts.Load()
	active := activeID(accs, configDir)

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

// --- rendering -------------------------------------------------------------

const barWidth = 8 // cells per usage bar

// ANSI attribute codes that themes never touch.
const (
	cReset = "\x1b[0m"
	cBold  = "\x1b[1m"
)

// ANSI 256-color escapes, emitted only when useColor() is true. Vars rebuilt
// exactly once per process by applyTheme (Line calls it before rendering);
// init seeds them from theme.Default() so anything that skips Line — tests,
// Render callers — gets the exact pre-theme escapes.
var (
	cGreen  string // plenty of headroom
	cAmber  string // filling up
	cRed    string // nearly out
	cDim    string // brackets, separators, empty cells, meta
	cActive string // the active account's id + ► marker
)

func init() { applyTheme(theme.Default()) }

// applyTheme rebuilds the color escapes from the theme's statusline palette.
// NO_COLOR still trumps everything: useColor() gates emission, so a themed
// palette never leaks into a colorless line.
func applyTheme(t theme.Theme) {
	cGreen = ansi256(t.Colors.SLGreen)
	cAmber = ansi256(t.Colors.SLAmber)
	cRed = ansi256(t.Colors.SLRed)
	cDim = ansi256(t.Colors.SLDim)
	cActive = ansi256(t.Colors.SLActive)
}

func ansi256(code string) string { return "\x1b[38;5;" + code + "m" }

// useColor reports whether to emit ANSI codes. Disabled when NO_COLOR is set
// (see https://no-color.org) so the line degrades cleanly to plain text.
func useColor() bool { return os.Getenv("NO_COLOR") == "" }

// levelColor maps a usage percentage to its color: green < 50 ≤ amber < 80 ≤ red.
func levelColor(pct float64) string {
	switch {
	case pct >= 80:
		return cRed
	case pct >= 50:
		return cAmber
	default:
		return cGreen
	}
}

// barCells splits a width-cell bar at pct into its filled and empty runes, using
// eighth-blocks for the boundary cell so the bar grows smoothly. Pure and
// color-free: the filled/empty split is all Render needs to colorize it.
func barCells(pct float64, width int) (filled, empty string) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	eighths := int(math.Round(pct / 100 * float64(width*8)))
	full, rem := eighths/8, eighths%8
	var f, e strings.Builder
	for i := 0; i < width; i++ {
		switch {
		case i < full:
			f.WriteRune('█')
		case i == full && rem > 0:
			f.WriteRune([]rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉'}[rem])
		default:
			e.WriteRune('░')
		}
	}
	return f.String(), e.String()
}

// bar renders "▕███▌░░░▏" with the filled part in the level color and the track
// dim. With color off it's just the plain blocks (for tests / NO_COLOR).
func bar(pct float64, color bool) string {
	filled, empty := barCells(pct, barWidth)
	if !color {
		return "▕" + filled + empty + "▏"
	}
	return cDim + "▕" + levelColor(pct) + filled + cDim + empty + "▏" + cReset
}

// Render builds the status line: one colored usage bar per account (the active
// one marked ►), separated by │, then the model and context usage. The bar shows
// the 5h window — the limit you hit first — colored green/amber/red by how full
// it is. Pure (no I/O), honors NO_COLOR, so it's testable without the network.
func Render(rows []row, model string, ctxPct *float64) string {
	color := useColor()
	sep := " │ "
	if color {
		sep = cDim + " │ " + cReset
	}

	var parts []string
	for _, r := range rows {
		label := r.ID
		if r.Active {
			label = "►" + r.ID
			if color {
				label = cActive + cBold + label + cReset
			}
		}
		var pct, b string
		if r.OK {
			b = bar(r.U5, color)
			pct = fmt.Sprintf("%.0f%%", r.U5)
			if color {
				pct = levelColor(r.U5) + pct + cReset
			}
		} else {
			b = bar(0, color) // empty dim track
			pct = "off"
			if color {
				pct = cDim + "off" + cReset
			}
		}
		parts = append(parts, label+" "+b+" "+pct)
	}

	meta := model
	if ctxPct != nil {
		if meta != "" {
			meta += " · "
		}
		meta += fmt.Sprintf("ctx %.0f%%", *ctxPct)
	}
	if meta != "" && color {
		meta = cDim + meta + cReset
	}

	segs := parts
	if meta != "" {
		segs = append(segs, meta)
	}
	return strings.Join(segs, sep)
}

// --- cached probe ----------------------------------------------------------

// keepGoodMax is how long a last-known-good value survives consecutive probe
// failures before the account shows as off. It bridges transient 429s from
// probing too often without freezing a stale percentage forever after a
// logout or a revoked token.
const keepGoodMax = 10 * time.Minute

type cacheEntry struct {
	U5     float64 `json:"u5"`
	U7     float64 `json:"u7"`
	OK     bool    `json:"ok"`
	TS     int64   `json:"ts"`               // unix seconds of the last probe attempt
	GoodTS int64   `json:"goodTs,omitempty"` // unix seconds of the last SUCCESSFUL probe
}

// goodTS is when an entry's value was actually probed OK. Cache files written
// before the GoodTS field existed fall back to the entry's write time.
func goodTS(e cacheEntry) int64 {
	if e.GoodTS != 0 {
		return e.GoodTS
	}
	return e.TS
}

// mergeProbe folds a fresh probe result over the previous cache entry (zero
// value if none): a success refreshes the value and its GoodTS; a failure keeps
// the last-known-good value only while it's younger than keepGoodMax, and shows
// the account as off after that.
func mergeProbe(prev cacheEntry, p usage.Probe, now int64) cacheEntry {
	if p.OK {
		return cacheEntry{U5: p.U5, U7: p.U7, OK: true, TS: now, GoodTS: now}
	}
	if prev.OK && now-goodTS(prev) <= int64(keepGoodMax.Seconds()) {
		return cacheEntry{U5: prev.U5, U7: prev.U7, OK: true, TS: now, GoodTS: goodTS(prev)}
	}
	return cacheEntry{TS: now}
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
		// Single-flight across processes: with several Claude sessions open every
		// one renders a status line, and the moment the cache goes stale they'd
		// all probe (and possibly refresh tokens) at once. Whoever wins the lock
		// probes; the rest serve the previous cache — it refreshes momentarily.
		_ = os.MkdirAll(filepath.Dir(cachePath()), 0o700)
		if lk, ok := flock.TryAcquire(cachePath() + ".lock"); ok {
			fresh := map[string]cacheEntry{}
			for _, p := range usage.ProbeAll(accs, probeTimeout) {
				fresh[p.Account.ID] = mergeProbe(cache[p.Account.ID], p, now)
			}
			cache = fresh
			writeCache(cache)
			lk.Release()
		}
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
	// Uniquely-named temp + rename: a fixed ".tmp" name can interleave between
	// two writers and rename corrupted bytes into place.
	tmp, err := os.CreateTemp(filepath.Dir(p), "usage-cache-*.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if tmp.Close() != nil {
		os.Remove(name)
		return
	}
	if os.Rename(name, p) != nil {
		os.Remove(name)
	}
}

// --- helpers ---------------------------------------------------------------

// activeID resolves which account owns configDir: an exact config-dir match
// first (this also covers accounts with a custom ConfigDir, whose basename
// isn't "account-<id>"), then the basename heuristic as fallback.
func activeID(accs []accounts.Account, configDir string) string {
	if configDir == "" {
		return ""
	}
	want := filepath.Clean(configDir)
	for _, a := range accs {
		if cd := a.ResolveConfigDir(); cd != "" && strings.EqualFold(filepath.Clean(cd), want) {
			return a.ID
		}
	}
	return accountID(configDir)
}

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
