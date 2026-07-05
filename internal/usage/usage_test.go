package usage

import (
	"testing"
	"time"

	"houston/internal/accounts"
)

func TestWeightedPressure(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	full5 := now.Add(win5h) // f5 = 1 (just reset)
	full7 := now.Add(win7d) // f7 = 1
	at5 := now              // f5 = 0 (resetting now)

	cases := []struct {
		name   string
		u5, u7 float64
		r5, r7 time.Time
		want   float64
	}{
		// both windows full-weight → the bottleneck window sets the pressure
		{"both full", 10, 20, full5, full7, 20},
		// 5h resetting now → contributes nothing, only 7d counts
		{"5h at reset", 100, 20, at5, full7, 20},
		// a high 5h about to reset is discounted vs a modest 7d far out
		{"discount near-reset", 90, 10, at5, full7, 10},
		// no reset info → used windows count in full (conservative)
		{"unknown resets", 10, 20, time.Time{}, time.Time{}, 20},
		// untouched window with unknown reset (resets_at null) = pure headroom
		{"idle 5h unknown reset", 0, 20, time.Time{}, full7, 20},
		// 5h saturated AND far from reset → can't be discounted below its own
		// value (account is unusable now), so it dominates the modest 7d.
		{"saturated 5h dominates", 100, 10, full5, full7, 100},
	}
	for _, c := range cases {
		got := weightedPressure(c.u5, c.u7, c.r5, c.r7, now)
		if got != c.want {
			t.Errorf("%s: weightedPressure=%.3f, want %.3f", c.name, got, c.want)
		}
	}
}

func TestSaturationGuardUsesAbsoluteTime(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	full7 := now.Add(win7d)
	// 5h saturated with the reset 2 h away: unusable NOW even though the
	// weighted blend would dilute it (f5≈0.4 → ~34) — the guard must win.
	if got := weightedPressure(95, 10, now.Add(2*time.Hour), full7, now); got != 95 {
		t.Errorf("saturated 2h from reset should dominate with 95, got %.1f", got)
	}
	// 5h saturated but resetting in 10 min (< grace): frees up right away, so
	// averaging below the saturated value is allowed.
	if got := weightedPressure(95, 10, now.Add(10*time.Minute), full7, now); got >= saturated {
		t.Errorf("saturated but about to reset should not dominate, got %.1f", got)
	}
}

// TestIdleAccountRanksFirst is the production regression that motivated the
// bottleneck-window formula: an idle account (5h untouched → resets_at null)
// whose week resets in minutes must rank BELOW every loaded account. With the
// old normalized average it collapsed to u7 (73%) and systematically lost to
// the most loaded account of the pool.
func TestIdleAccountRanksFirst(t *testing.T) {
	now := time.Date(2026, 7, 4, 23, 56, 0, 0, time.UTC)
	// real probe data captured on 2026-07-04 (work1/work2/work3)
	w1 := weightedPressure(15, 41, now.Add(1*time.Hour+54*time.Minute), now.Add(5*time.Hour+4*time.Minute), now)
	w2 := weightedPressure(0, 73, time.Time{}, now.Add(64*time.Minute), now)
	w3 := weightedPressure(26, 11, now.Add(3*time.Hour+20*time.Minute), now.Add(142*time.Hour), now)
	if !(w2 < w1 && w1 < w3) {
		t.Fatalf("ranking should be idle w2 < w1 < w3, got w1=%.1f w2=%.1f w3=%.1f", w1, w2, w3)
	}
	// (b) a high weekly with an imminent reset must not dominate the pressure
	if w2 > 5 {
		t.Errorf("73%% weekly resetting in 64min should attenuate to ~0.5, got %.1f", w2)
	}
}

// TestPickBestTieBreak: near-equal pressures (within tieBand) alternate by
// least-recently-used instead of always picking the same account.
func TestPickBestTieBreak(t *testing.T) {
	probes := []Probe{
		{Account: accounts.Account{ID: "a", LastUse: "2026-07-04T10:00:00Z"}, Pressure: 10.0, OK: true},
		{Account: accounts.Account{ID: "b", LastUse: "2026-07-03T10:00:00Z"}, Pressure: 11.5, OK: true},
		{Account: accounts.Account{ID: "c", LastUse: ""}, Pressure: 30.0, OK: true},
	}
	// a and b are tied (Δ1.5 < 2): the least-recently-used (b) wins; c is out.
	if got := pickBest(probes); got.ID != "b" {
		t.Fatalf("tie should go to least-recently-used 'b', got %q", got.ID)
	}
	// clear winner outside the band: pressure decides, LastUse irrelevant
	probes[1].Pressure = 20.0
	if got := pickBest(probes); got.ID != "a" {
		t.Fatalf("clear minimum 'a' should win, got %q", got.ID)
	}
}

func TestExpiredMs(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	ms := func(t time.Time) int64 { return t.UnixMilli() }
	cases := []struct {
		name string
		exp  int64
		want bool
	}{
		{"expired hours ago", ms(now.Add(-3 * time.Hour)), true},
		{"expires within the margin (30s)", ms(now.Add(30 * time.Second)), true},
		{"valid with headroom", ms(now.Add(2 * time.Hour)), false},
		{"unknown (0): tried as-is", 0, false},
	}
	for _, c := range cases {
		if got := expiredMs(c.exp, now); got != c.want {
			t.Errorf("%s: expiredMs=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestProbeTokenEmptyFailsFast(t *testing.T) {
	// An account without login has no credential: it must fail without the
	// network (and without waiting the timeout) instead of sending an empty
	// Bearer doomed to a 401.
	start := time.Now()
	_, _, _, _, err := ProbeToken("", 8*time.Second)
	if err == nil {
		t.Fatal("empty token should return an error")
	}
	if time.Since(start) > time.Second {
		t.Error("the empty-token failure should be immediate")
	}
}

func TestRemainingFractionClamps(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	if f := remainingFraction(now.Add(-time.Hour), now, win5h); f != 0 {
		t.Errorf("reset in the past should give 0, got %.3f", f)
	}
	if f := remainingFraction(now.Add(100*win5h), now, win5h); f != 1 {
		t.Errorf("far-future reset should clamp to 1, got %.3f", f)
	}
}

func TestLRUFirst(t *testing.T) {
	// Best's no-probe fallback: empty LastUse (never used) wins, else oldest.
	accs := []accounts.Account{
		{ID: "a", LastUse: "2026-05-28T10:00:00Z"},
		{ID: "b", LastUse: ""}, // never used -> should win
		{ID: "c", LastUse: "2026-05-27T10:00:00Z"},
	}
	if got := lruFirst(accs); got.ID != "b" {
		t.Fatalf("lruFirst should pick never-used 'b', picked %q", got.ID)
	}
	accs[1].LastUse = "2026-05-28T12:00:00Z" // with all used, oldest wins
	if got := lruFirst(accs); got.ID != "c" {
		t.Fatalf("lruFirst should pick the oldest 'c', picked %q", got.ID)
	}
}
