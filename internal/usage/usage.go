// Package usage probes Anthropic's OAuth usage endpoint to balance accounts by
// quota pressure, and selects the best account to launch. Pressure weights each
// quota window (5h, 7d) by how much of its period is still left before it resets,
// so a window about to renew barely counts (its load frees up imminently) and one
// far from reset counts fully — favouring the account with the most sustained
// headroom. If probing fails (e.g. a long-lived token can't query usage), it
// degrades gracefully to least-recently-used balancing.
package usage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"houston/internal/accounts"
)

const (
	usageURL  = "https://api.anthropic.com/api/oauth/usage"
	usageBeta = "oauth-2025-04-20"

	win5h = 5 * time.Hour
	win7d = 7 * 24 * time.Hour
)

// Probe is the result of querying one account's usage.
type Probe struct {
	Account  accounts.Account
	U5       float64   // 5-hour utilization in percent (0..100, as the endpoint reports)
	U7       float64   // 7-day utilization in percent (0..100)
	Reset5   time.Time // when the 5h window resets (zero if unknown)
	Reset7   time.Time // when the 7d window resets (zero if unknown)
	Pressure float64   // time-weighted blend of U5/U7 (see weightedPressure)
	OK       bool
	Err      string
}

type usageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type usageResp struct {
	FiveHour usageWindow `json:"five_hour"`
	SevenDay usageWindow `json:"seven_day"`
}

// ProbeToken queries usage for a single token. An empty token (a dir that was
// never logged in) fails fast instead of firing a doomed request with an empty
// bearer — one per account per probe otherwise: the run table, `account ls`
// and every statusline render with a stale cache.
func ProbeToken(token string, timeout time.Duration) (u5, u7 float64, r5, r7 time.Time, err error) {
	if token == "" {
		return 0, 0, time.Time{}, time.Time{}, fmt.Errorf("sin credencial (cuenta sin login)")
	}
	req, _ := http.NewRequest(http.MethodGet, usageURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", usageBeta)
	req.Header.Set("User-Agent", "houston/0.1")
	c := &http.Client{Timeout: timeout}
	resp, err := c.Do(req)
	if err != nil {
		return 0, 0, time.Time{}, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, 0, time.Time{}, time.Time{}, fmt.Errorf("usage HTTP %d", resp.StatusCode)
	}
	var r usageResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return 0, 0, time.Time{}, time.Time{}, err
	}
	return r.FiveHour.Utilization, r.SevenDay.Utilization,
		parseReset(r.FiveHour.ResetsAt), parseReset(r.SevenDay.ResetsAt), nil
}

// parseReset parses an RFC3339 reset timestamp, returning the zero time if it's
// missing or unparseable (treated as "unknown" → no weight).
func parseReset(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// remainingFraction returns how much of a window's period is still left before
// it resets, clamped to [0,1]: 1 just after a reset, →0 as the reset approaches.
// An unknown reset (zero time) yields 0 weight.
func remainingFraction(reset, now time.Time, period time.Duration) float64 {
	if reset.IsZero() {
		return 0
	}
	frac := reset.Sub(now).Seconds() / period.Seconds()
	if frac < 0 {
		return 0
	}
	if frac > 1 {
		return 1
	}
	return frac
}

// saturated is the utilization (in percent — the endpoint reports 0..100) at or
// above which a window is treated as a hard blocker: if you're maxed on the 5h
// window you literally can't use the account right now, regardless of a roomy 7d.
const saturated = 90.0

// saturationGrace: a saturated window whose reset is at most this far away is
// about to free up, so the blend may still average it down. Anything further
// out is a hard blocker RIGHT NOW. Absolute time, not a fraction of the period:
// half of the 5h window is 2.5 h — far too long to count as "about to reset".
const saturationGrace = 15 * time.Minute

// blockedFor reports whether a saturated window keeps the account unusable
// beyond the grace period. Unknown reset (zero time) is not treated as blocked,
// consistent with the blend giving it no weight.
func blockedFor(reset, now time.Time) bool {
	return !reset.IsZero() && reset.Sub(now) > saturationGrace
}

// weightedPressure blends the 5h and 7d utilizations, weighting each by how much
// of its period is still left before it resets (a window about to reset barely
// counts; one far from reset counts fully). Falls back to max(u5,u7) when neither
// reset time is known.
//
// Guard: a window that is both saturated AND not resetting within
// saturationGrace is a real bottleneck, so the blend can't average it away
// below its own value — otherwise an account maxed on 5h (unusable now) but
// idle on 7d could outrank a steady one.
func weightedPressure(u5, u7 float64, r5, r7, now time.Time) float64 {
	f5 := remainingFraction(r5, now, win5h)
	f7 := remainingFraction(r7, now, win7d)
	var p float64
	if f5+f7 == 0 {
		p = max(u5, u7)
	} else {
		p = (f5*u5 + f7*u7) / (f5 + f7)
	}
	if u5 >= saturated && blockedFor(r5, now) {
		p = max(p, u5)
	}
	if u7 >= saturated && blockedFor(r7, now) {
		p = max(p, u7)
	}
	return p
}

// ProbeAll probes every account in parallel.
func ProbeAll(accs []accounts.Account, timeout time.Duration) []Probe {
	now := time.Now()
	out := make([]Probe, len(accs))
	var wg sync.WaitGroup
	for i, a := range accs {
		wg.Add(1)
		go func(i int, a accounts.Account) {
			defer wg.Done()
			p := Probe{Account: a}
			u5, u7, r5, r7, err := ProbeToken(a.ProbeCredential(), timeout)
			if err != nil {
				p.Err = err.Error()
			} else {
				p.U5, p.U7, p.Reset5, p.Reset7 = u5, u7, r5, r7
				p.Pressure = weightedPressure(u5, u7, r5, r7, now)
				p.OK = true
			}
			out[i] = p
		}(i, a)
	}
	wg.Wait()
	return out
}

// Best selects the account to launch: lowest pressure among successful probes;
// if none probe successfully, the least-recently-used account.
func Best(accs []accounts.Account, timeout time.Duration) (accounts.Account, []Probe, error) {
	if len(accs) == 0 {
		return accounts.Account{}, nil, fmt.Errorf("no hay cuentas; añade una con: houston account add")
	}
	probes := ProbeAll(accs, timeout)
	ok := make([]Probe, 0, len(probes))
	for _, p := range probes {
		if p.OK {
			ok = append(ok, p)
		}
	}
	if len(ok) > 0 {
		sort.SliceStable(ok, func(i, j int) bool { return ok[i].Pressure < ok[j].Pressure })
		return ok[0].Account, probes, nil
	}
	// fallback: least-recently-used (empty LastUse = never used = preferred)
	return lruFirst(accs), probes, nil
}

// lruFirst returns the least-recently-used account (empty LastUse = never used,
// sorts first). Caller guarantees len(accs) > 0.
func lruFirst(accs []accounts.Account) accounts.Account {
	cp := append([]accounts.Account(nil), accs...)
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].LastUse < cp[j].LastUse })
	return cp[0]
}

