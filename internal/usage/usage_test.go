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
		name           string
		u5, u7         float64
		r5, r7         time.Time
		want           float64
	}{
		// both windows full-weight → plain average
		{"both full", 10, 20, full5, full7, 15},
		// 5h resetting now → ignored, only 7d counts
		{"5h at reset", 100, 20, at5, full7, 20},
		// a high 5h about to reset is discounted vs a modest 7d far out
		{"discount near-reset", 90, 10, at5, full7, 10},
		// no reset info → falls back to max(u5,u7)
		{"unknown resets", 10, 20, time.Time{}, time.Time{}, 20},
	}
	for _, c := range cases {
		got := weightedPressure(c.u5, c.u7, c.r5, c.r7, now)
		if got != c.want {
			t.Errorf("%s: weightedPressure=%.3f, want %.3f", c.name, got, c.want)
		}
	}
}

func TestRemainingFractionClamps(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	if f := remainingFraction(now.Add(-time.Hour), now, win5h); f != 0 {
		t.Errorf("reset en el pasado debería dar 0, dio %.3f", f)
	}
	if f := remainingFraction(now.Add(100*win5h), now, win5h); f != 1 {
		t.Errorf("reset muy lejano debería clamparse a 1, dio %.3f", f)
	}
}

func TestPickLRU(t *testing.T) {
	accs := []accounts.Account{
		{ID: "a", LastUse: "2026-05-28T10:00:00Z"},
		{ID: "b", LastUse: ""}, // never used -> should win
		{ID: "c", LastUse: "2026-05-27T10:00:00Z"},
	}
	got, ok := PickLRU(accs)
	if !ok || got.ID != "b" {
		t.Fatalf("PickLRU debería elegir la nunca usada 'b', eligió %q", got.ID)
	}
	// with all used, oldest wins
	accs[1].LastUse = "2026-05-28T12:00:00Z"
	got, _ = PickLRU(accs)
	if got.ID != "c" {
		t.Fatalf("PickLRU debería elegir la más antigua 'c', eligió %q", got.ID)
	}
}

func TestPickLRUEmpty(t *testing.T) {
	if _, ok := PickLRU(nil); ok {
		t.Errorf("PickLRU sobre lista vacía debería devolver ok=false")
	}
}
