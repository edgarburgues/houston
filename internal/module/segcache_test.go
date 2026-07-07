package module

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"houston/internal/flock"
)

// segModule builds an in-memory module whose statusline segment re-execs the
// test binary; marker files land in markDir so tests can count execs.
func segModule(t *testing.T, name string, argv []string, ttlSeconds int) Module {
	t.Helper()
	m := testModule(t, name)
	m.Manifest.Statusline = &Segment{Command: argv, TTLSeconds: ttlSeconds}
	return m
}

func markDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func countMarks(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}

func writeSegFile(t *testing.T, cache map[string]segEntry) {
	t.Helper()
	b, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(segCachePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(segCachePath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readSegFile(t *testing.T) map[string]segEntry {
	t.Helper()
	cache := map[string]segEntry{}
	b, err := os.ReadFile(segCachePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cache); err != nil {
		t.Fatalf("cache file is not valid JSON: %v", err)
	}
	return cache
}

func TestSegmentsColdStartRefreshesAndServes(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	marks := markDir(t)
	m := segModule(t, "alpha", helperCmd("seg-mark", marks, "sprint 12"), 0)

	got := Segments([]Module{m}, SegmentInput{})
	if len(got) != 1 || got[0] != "sprint 12" {
		t.Fatalf("winner should serve its fresh result: %q", got)
	}
	if n := countMarks(t, marks); n != 1 {
		t.Fatalf("cold start should exec exactly once, got %d", n)
	}
	// The write is atomic (unique temp + rename): the file parses and no temp
	// stragglers remain next to it.
	cache := readSegFile(t)
	if e := cache["alpha"]; !e.OK || e.Text != "sprint 12" || e.GoodTS == 0 {
		t.Fatalf("cache entry after refresh: %+v", e)
	}
	ents, _ := os.ReadDir(filepath.Dir(segCachePath()))
	for _, ent := range ents {
		if strings.HasSuffix(ent.Name(), ".tmp") {
			t.Fatalf("leftover temp file: %s", ent.Name())
		}
	}
}

func TestSegmentsTTLHitExecsNothing(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	marks := markDir(t)
	m := segModule(t, "alpha", helperCmd("seg-mark", marks, "new-text"), 0)
	now := time.Now().Unix()
	writeSegFile(t, map[string]segEntry{
		"alpha": {Text: "cached", OK: true, TS: now, GoodTS: now},
	})

	got := Segments([]Module{m}, SegmentInput{})
	if len(got) != 1 || got[0] != "cached" {
		t.Fatalf("fresh entry should be served from cache: %q", got)
	}
	if n := countMarks(t, marks); n != 0 {
		t.Fatalf("TTL hit must exec nothing, got %d execs", n)
	}
}

func TestSegmentsSingleFlightUnderContention(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	marks := markDir(t)
	m := segModule(t, "alpha", helperCmd("seg-mark", marks, "sprint 12"), 0)

	// Another process holds the cache lock: this render must lose, exec
	// nothing, and serve whatever the previous cache holds (here: nothing).
	_ = os.MkdirAll(filepath.Dir(segCachePath()), 0o700)
	lk, ok := flock.TryAcquire(segCachePath() + ".lock")
	if !ok {
		t.Fatal("could not take the lock for the test")
	}
	if got := Segments([]Module{m}, SegmentInput{}); len(got) != 0 {
		t.Fatalf("loser on cold start should render without segments: %q", got)
	}
	if n := countMarks(t, marks); n != 0 {
		t.Fatalf("loser must not exec, got %d", n)
	}
	lk.Release()

	// Lock free again: the next render wins and refreshes.
	if got := Segments([]Module{m}, SegmentInput{}); len(got) != 1 || got[0] != "sprint 12" {
		t.Fatalf("after contention clears the winner refreshes: %q", got)
	}
	if n := countMarks(t, marks); n != 1 {
		t.Fatalf("want exactly one exec after the lock cleared, got %d", n)
	}
}

func TestRefreshPostLockReRead(t *testing.T) {
	// The double-checked single-flight: a loser that saw a stale cache before
	// the lock, then acquired it right behind a finished winner, must re-read
	// and exec nothing — and a concurrent writer's fresh entry must survive
	// the merge untouched (the write is built from the post-lock map).
	t.Setenv("HOUSTON_HOME", t.TempDir())
	marksA, marksB := markDir(t), markDir(t)
	a := segModule(t, "alpha", helperCmd("seg-mark", marksA, "alpha-new"), 0)
	b := segModule(t, "beta", helperCmd("seg-mark", marksB, "beta-new"), 0)

	now := time.Now().Unix()
	// Pre-lock this process believed both were stale; meanwhile a concurrent
	// refresher landed a fresh beta.
	writeSegFile(t, map[string]segEntry{
		"alpha": {Text: "alpha-old", OK: true, TS: now - 3600, GoodTS: now - 3600},
		"beta":  {Text: "beta-fresh", OK: true, TS: now, GoodTS: now},
	})

	cache := refreshSegments([]Module{a, b})
	if n := countMarks(t, marksB); n != 0 {
		t.Fatalf("fresh beta must not be re-exec'd post-lock, got %d execs", n)
	}
	if n := countMarks(t, marksA); n != 1 {
		t.Fatalf("stale alpha should exec once, got %d", n)
	}
	if e := cache["alpha"]; e.Text != "alpha-new" || !e.OK {
		t.Fatalf("alpha not refreshed: %+v", e)
	}
	if e := cache["beta"]; e.Text != "beta-fresh" || e.TS != now {
		t.Fatalf("concurrent writer's beta was rolled back: %+v", e)
	}
	// And the same survives on disk.
	if e := readSegFile(t)["beta"]; e.Text != "beta-fresh" || e.TS != now {
		t.Fatalf("written cache rolled beta back: %+v", e)
	}
}

func TestSegmentsBatchRunsConcurrently(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	const sleepMs = 2000
	arg := strconv.Itoa(sleepMs)
	a := segModule(t, "slow-a", helperCmd("seg-sleep", arg, "a"), 0)
	b := segModule(t, "slow-b", helperCmd("seg-sleep", arg, "b"), 0)

	start := time.Now()
	got := Segments([]Module{a, b}, SegmentInput{})
	elapsed := time.Since(start)
	// A sequential batch would sleep 2×2000ms before anything else; finishing
	// under the sum proves the handlers overlapped (the ProbeAll shape).
	if elapsed >= 2*sleepMs*time.Millisecond {
		t.Fatalf("batch took %s — handlers did not run concurrently", elapsed)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("want both segments in module order, got %q", got)
	}
}

func TestSegmentsBudgetExpiredFailsWithFreshTS(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	old := segBudget
	segBudget = 300 * time.Millisecond
	defer func() { segBudget = old }()

	// Both handlers outlive the budget. slow-a has a recent good value (kept),
	// slow-b has no history (goes dark) — and both get ts=now so the very next
	// render doesn't re-trigger the refresh.
	a := segModule(t, "slow-a", helperCmd("seg-sleep", "5000", "never-a"), 0)
	b := segModule(t, "slow-b", helperCmd("seg-sleep", "5000", "never-b"), 0)
	now := time.Now().Unix()
	writeSegFile(t, map[string]segEntry{
		"slow-a": {Text: "old-good", OK: true, TS: now - 3600, GoodTS: now - 30},
	})

	start := time.Now()
	got := Segments([]Module{a, b}, SegmentInput{})
	// The global budget cuts the batch, not the 4s per-module timeout.
	if elapsed := time.Since(start); elapsed >= 3900*time.Millisecond {
		t.Fatalf("budget did not bound the batch: %s", elapsed)
	}
	if len(got) != 1 || got[0] != "old-good" {
		t.Fatalf("keep-last-good should survive the expired budget: %q", got)
	}
	cache := readSegFile(t)
	if e := cache["slow-a"]; !e.OK || e.Text != "old-good" || e.TS < start.Unix() {
		t.Fatalf("slow-a should keep its good text with ts=now: %+v", e)
	}
	if e := cache["slow-b"]; e.OK || e.TS < start.Unix() {
		t.Fatalf("slow-b should be a fresh failure entry: %+v", e)
	}
}

func TestMergeSegment(t *testing.T) {
	now := int64(1_000_000)
	good := segEntry{Text: "sprint 12", OK: true, TS: now - 120, GoodTS: now - 120}

	// success: refreshes text and GoodTS
	if e := mergeSegment(good, segResult{text: "sprint 13", ok: true}, now); !e.OK || e.Text != "sprint 13" || e.GoodTS != now {
		t.Errorf("success should refresh text and GoodTS: %+v", e)
	}
	// success with empty text: valid hidden-this-cycle reply
	if e := mergeSegment(good, segResult{ok: true}, now); !e.OK || e.Text != "" {
		t.Errorf("empty success should cache a hidden segment: %+v", e)
	}
	// recent failure (< segKeepGoodMax): keeps the last good text and its GoodTS
	if e := mergeSegment(good, segResult{}, now); !e.OK || e.Text != "sprint 12" || e.GoodTS != good.GoodTS {
		t.Errorf("transient failure should keep the last good text: %+v", e)
	}
	// persistent failure (> segKeepGoodMax since the last success): goes dark
	stale := segEntry{Text: "sprint 12", OK: true, TS: now - 30, GoodTS: now - int64(segKeepGoodMax.Seconds()) - 1}
	if e := mergeSegment(stale, segResult{}, now); e.OK || e.Text != "" {
		t.Errorf("an expired good text should go dark: %+v", e)
	}
	// entry without GoodTS: falls back to TS as the last-success time
	legacy := segEntry{Text: "sprint 12", OK: true, TS: now - 60}
	if e := mergeSegment(legacy, segResult{}, now); !e.OK || e.GoodTS != legacy.TS {
		t.Errorf("legacy entry should expire by its TS: %+v", e)
	}
	// no previous entry and a failure: dark, ts stamped
	if e := mergeSegment(segEntry{}, segResult{}, now); e.OK || e.TS != now {
		t.Errorf("failure with no history: %+v", e)
	}
}

func TestSegStaleHonorsPerModuleTTL(t *testing.T) {
	now := int64(1_000_000)
	cases := []struct {
		age  int64
		ttl  int
		want bool
	}{
		{age: 70, ttl: 300, want: false}, // within a long TTL
		{age: 301, ttl: 300, want: true},
		{age: 70, ttl: 0, want: true},      // default 60
		{age: 70, ttl: 5, want: true},      // 60s floor: 5 clamps up, 70 is stale
		{age: 30, ttl: 5, want: false},     // …but 30 is inside the floored TTL
		{age: 3601, ttl: 9999, want: true}, // 3600s ceiling
	}
	for _, c := range cases {
		e := segEntry{OK: true, TS: now - c.age}
		if got := segStale(e, (Segment{TTLSeconds: c.ttl}).TTL(), now); got != c.want {
			t.Errorf("segStale(age=%d, ttl=%d) = %v, want %v", c.age, c.ttl, got, c.want)
		}
	}
	// The zero entry (never cached) is always stale.
	if !segStale(segEntry{}, (Segment{}).TTL(), now) {
		t.Error("missing entry must be stale")
	}
}

func TestSegTextsSanitizesCacheText(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	marks := markDir(t)
	m := segModule(t, "alpha", helperCmd("seg-mark", marks, "unused"), 0)
	now := time.Now().Unix()
	// A hand-edited cache must not be able to inject escapes or oversized
	// text into the line: emission re-strips and re-clamps.
	writeSegFile(t, map[string]segEntry{
		"alpha": {Text: "\x1b[31mRED\x1b[0m " + strings.Repeat("x", 200) + "\nsecond line", OK: true, TS: now, GoodTS: now},
	})
	got := Segments([]Module{m}, SegmentInput{})
	if len(got) != 1 {
		t.Fatalf("want one segment, got %q", got)
	}
	if strings.Contains(got[0], "\x1b") || strings.Contains(got[0], "\n") {
		t.Fatalf("escape or newline reached the line: %q", got[0])
	}
	if n := len([]rune(got[0])); n > segMaxRunes {
		t.Fatalf("segment text %d runes > %d", n, segMaxRunes)
	}
	if !strings.HasPrefix(got[0], "RED ") {
		t.Fatalf("stripped text mangled: %q", got[0])
	}
	if countMarks(t, marks) != 0 {
		t.Fatal("fresh entry must not exec")
	}
}

func TestPruneSegCacheDropsOneEntry(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	now := time.Now().Unix()
	writeSegFile(t, map[string]segEntry{
		"gone": {Text: "stale", OK: true, TS: now, GoodTS: now},
		"kept": {Text: "fine", OK: true, TS: now, GoodTS: now},
	})
	pruneSegCache("gone")
	cache := readSegFile(t)
	if _, ok := cache["gone"]; ok {
		t.Fatal("pruned entry still cached")
	}
	if e := cache["kept"]; e.Text != "fine" {
		t.Fatalf("unrelated entry harmed: %+v", e)
	}
	// Contention: pruning is best-effort — a held lock skips, never blocks.
	lk, err := flock.Acquire(segCachePath()+".lock", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	pruneSegCache("kept")
	if _, ok := readSegFile(t)["kept"]; !ok {
		t.Fatal("prune under a held lock must skip, not wait it out")
	}
}

func TestSegCacheOrphans(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	now := time.Now().Unix()
	writeSegFile(t, map[string]segEntry{
		"ghost": {Text: "x", OK: true, TS: now},
		"alive": {Text: "y", OK: true, TS: now},
	})
	if err := os.MkdirAll(filepath.Join(Dir(), "alive"), 0o700); err != nil {
		t.Fatal(err)
	}
	got := SegCacheOrphans()
	if len(got) != 1 || got[0] != "ghost" {
		t.Fatalf("orphans: %v", got)
	}
}

func TestRemovePrunesSegCache(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	dir := filepath.Join(Dir(), "seggy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	man := `{"api":1,"name":"seggy","statusline":{"command":["x"]}}`
	if err := os.WriteFile(filepath.Join(dir, "module.json"), []byte(man), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RegSave([]Entry{{Name: "seggy"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	writeSegFile(t, map[string]segEntry{
		"seggy": {Text: "old text", OK: true, TS: now, GoodTS: now},
	})
	if err := Remove("seggy"); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSegFile(t)["seggy"]; ok {
		t.Fatal("rm must prune the module's segment-cache entry")
	}
}
