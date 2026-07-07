package module

// The statusline segment cache. Claude Code re-renders the status line
// roughly every 300 ms per session; uncached, 6 sessions × 2 modules would
// mean ~40 handler cold starts per second — a fork storm. One machine-wide
// file (<StoreDir>/modules-seg-cache.json) bounds that to at most one exec
// per module per TTL: the render path reads the cache once and only the
// single-flight winner of the cache's own lock re-execs stale handlers.
// Entries are keyed by module name alone, which is legitimate because the
// segment payload is machine-global by contract — no per-session fields can
// be baked into a cache every concurrent session serves.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"houston/internal/accounts"
	"houston/internal/flock"
)

// SegmentInput is the statusline-side input to Segments. The segment payload
// carries no per-session fields today (machine-global by contract); the type
// exists so a v2 per-session segment variant can grow fields without
// changing the Segments signature.
type SegmentInput struct{}

// segBudget bounds one whole refresh batch — every stale handler runs
// concurrently under this shared context, so a winning render stalls at most
// min(segBudget, max per-module timeout), never the sum. It also keeps the
// total lock-hold far below flock's 30 s staleAfter: an overrun would get
// the lock broken mid-refresh and then Release would delete the new owner's
// lock. A var so tests can shrink it.
var segBudget = 10 * time.Second

// segKeepGoodMax mirrors the usage cache's keepGoodMax: a failing handler
// keeps its last-good text this long, then the segment silently disappears —
// never an error string in the line.
const segKeepGoodMax = 10 * time.Minute

// segMaxRunes caps a segment's text (first line only, ANSI/control stripped).
const segMaxRunes = 80

// segEntry is one module's cached segment.
type segEntry struct {
	Text   string `json:"text"`
	OK     bool   `json:"ok"`
	TS     int64  `json:"ts"`               // unix seconds of the last refresh attempt
	GoodTS int64  `json:"goodTs,omitempty"` // unix seconds of the last SUCCESSFUL exec
}

func segCachePath() string { return filepath.Join(accounts.StoreDir(), "modules-seg-cache.json") }

// Segments returns the cached statusline texts of every module declaring a
// segment, in the given (lexicographic) order. When any entry is stale past
// its per-module TTL, whoever wins the cache's own lock — separate from
// usage-cache.json.lock, so a module refresh never serializes behind the
// network probe — refreshes inline; losers serve the previous cache and the
// cold start simply renders without segments until the winner lands. Errors
// never reach the line; they are logged only.
func Segments(mods []Module, sl SegmentInput) []string {
	_ = sl // no per-session fields in v1 (§3: the payload is machine-global)
	var withSeg []Module
	for _, m := range mods {
		if m.Manifest.Statusline != nil {
			withSeg = append(withSeg, m)
		}
	}
	if len(withSeg) == 0 {
		return nil
	}
	cache := readSegCache()
	now := time.Now().Unix()
	if anySegStale(withSeg, cache, now) {
		_ = os.MkdirAll(accounts.StoreDir(), 0o700)
		if lk, ok := flock.TryAcquire(segCachePath() + ".lock"); ok {
			cache = refreshSegments(withSeg)
			lk.Release()
		}
	}
	return segTexts(withSeg, cache, now)
}

// refreshSegments is the single-flight winner's path. It RE-READS the cache
// after taking the lock and recomputes staleness (double-checked): a loser
// queued behind a just-finished winner must not re-exec entries refreshed
// milliseconds ago, and the final write is built from this post-lock map so
// a concurrent refresher's fresh entries are never rolled back by a stale
// pre-lock snapshot. Still-stale handlers exec concurrently — one goroutine
// each under a WaitGroup, the usage.ProbeAll shape — each under its own
// per-module timeout and all under the shared segBudget context.
func refreshSegments(mods []Module) map[string]segEntry {
	cache := readSegCache()
	now := time.Now().Unix()
	var stale []Module
	for _, m := range mods {
		if segStale(cache[m.Name], m.Manifest.Statusline.TTL(), now) {
			stale = append(stale, m)
		}
	}
	if len(stale) == 0 {
		return cache
	}
	ctx, cancel := context.WithTimeout(context.Background(), segBudget)
	defer cancel()
	results := make([]segResult, len(stale))
	var wg sync.WaitGroup
	for i, m := range stale {
		wg.Add(1)
		go func(i int, m Module) {
			defer wg.Done()
			results[i] = execSegment(ctx, m)
		}(i, m)
	}
	wg.Wait()
	// Merge with a fresh timestamp: a module the expired budget cut short is
	// a failure with ts=now, so it doesn't re-trigger on the very next render.
	now = time.Now().Unix()
	for i, m := range stale {
		cache[m.Name] = mergeSegment(cache[m.Name], results[i], now)
	}
	writeSegCache(cache)
	return cache
}

// segResult is one handler exec's outcome. ok with empty text is a valid
// "hidden this cycle" reply.
type segResult struct {
	text string
	ok   bool
}

type segmentReply struct {
	Text string `json:"text"`
}

// execSegment runs one module's segment handler under the full Invoke
// hardening. Failures come back as the zero result — Invoke and the decoder
// path already logged the stanza.
func execSegment(ctx context.Context, m Module) segResult {
	s := m.Manifest.Statusline
	env := NewEnvelope(EventSegment, m, struct{}{})
	raw, err := Invoke(ctx, m, s.Command, env, CapSegment, m.Manifest.ResolveTimeout(SurfaceSegment, s.TimeoutMs))
	if err != nil {
		return segResult{}
	}
	var rep segmentReply
	if err := DecodeReply(raw, &rep); err != nil {
		LogEvent(m.Name, EventSegment, err.Error(), nil)
		return segResult{}
	}
	return segResult{text: CleanLine(rep.Text, segMaxRunes), ok: true}
}

// mergeSegment folds one exec result over the previous entry — the
// mergeProbe analogue: success refreshes text and GoodTS; failure keeps the
// last-good text while it's younger than segKeepGoodMax, after which the
// segment goes dark.
func mergeSegment(prev segEntry, r segResult, now int64) segEntry {
	if r.ok {
		return segEntry{Text: r.text, OK: true, TS: now, GoodTS: now}
	}
	if prev.OK && now-segGoodTS(prev) <= int64(segKeepGoodMax.Seconds()) {
		return segEntry{Text: prev.Text, OK: true, TS: now, GoodTS: segGoodTS(prev)}
	}
	return segEntry{TS: now}
}

// segGoodTS is when an entry's text was last produced by a successful exec;
// entries written without the field fall back to their write time.
func segGoodTS(e segEntry) int64 {
	if e.GoodTS != 0 {
		return e.GoodTS
	}
	return e.TS
}

// segStale reports whether an entry is past its module's TTL. The zero entry
// (module never cached) is stale by construction.
func segStale(e segEntry, ttl time.Duration, now int64) bool {
	return now-e.TS > int64(ttl.Seconds())
}

func anySegStale(mods []Module, cache map[string]segEntry, now int64) bool {
	for _, m := range mods {
		if segStale(cache[m.Name], m.Manifest.Statusline.TTL(), now) {
			return true
		}
	}
	return false
}

// segTexts emits every non-expired, non-empty text in module order. Text is
// re-stripped at emission — the cache file is plain user-writable JSON, and
// an escape sequence must never reach the line even from a hand-edited cache.
func segTexts(mods []Module, cache map[string]segEntry, now int64) []string {
	var out []string
	for _, m := range mods {
		e := cache[m.Name]
		if !e.OK || segStale(e, m.Manifest.Statusline.TTL(), now) {
			continue
		}
		if text := CleanLine(e.Text, segMaxRunes); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// pruneSegCache drops one module's entry from the shared segment cache — the
// rm step that keeps a same-named module added later from being served the
// removed module's text within the old entry's TTL (entries are keyed by
// name alone). Best-effort under the cache's own lock, the same
// read-modify-write discipline as refreshSegments; on contention the entry
// stays and doctor flags it as orphaned.
func pruneSegCache(name string) {
	lk, err := flock.Acquire(segCachePath()+".lock", 250*time.Millisecond)
	if err != nil {
		return
	}
	defer lk.Release()
	cache := readSegCache()
	if _, ok := cache[name]; !ok {
		return
	}
	delete(cache, name)
	writeSegCache(cache)
}

// SegCacheOrphans lists segment-cache entries matching no registry entry and
// no modules/ dir — leftovers of a crashed rm or a hand-edit. Doctor's
// business; rm prunes its own entry on the happy path.
func SegCacheOrphans() []string {
	cache := readSegCache()
	if len(cache) == 0 {
		return nil
	}
	known := map[string]bool{}
	if list, err := RegLoad(); err == nil {
		for _, e := range list {
			known[e.Name] = true
		}
	}
	dirents, _ := os.ReadDir(Dir())
	for _, d := range dirents {
		known[d.Name()] = true
	}
	var out []string
	for name := range cache {
		if !known[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func readSegCache() map[string]segEntry {
	cache := map[string]segEntry{}
	if b, err := os.ReadFile(segCachePath()); err == nil {
		_ = json.Unmarshal(b, &cache)
	}
	return cache
}

// writeSegCache persists the post-lock map; best-effort, atomic via the
// registry's unique-temp + rename helper.
func writeSegCache(cache map[string]segEntry) {
	b, err := json.Marshal(cache)
	if err != nil {
		return
	}
	if err := os.MkdirAll(accounts.StoreDir(), 0o700); err != nil {
		return
	}
	_ = writeFileAtomic(segCachePath(), b, 0o600)
}
