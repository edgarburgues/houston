package scan

import (
	"encoding/json"
	"os"
	"path/filepath"

	"houston/internal/accounts"
	"houston/internal/model"
)

// Cache remembers the parsed Mission of every transcript keyed by path and
// validated by (size, mtime), so TUI launches and rescans only re-read files
// that actually changed — parsing streams, but a big store still costs tens of
// MB of I/O per pass. It's a pure accelerator: any load/save problem simply
// means a full re-parse. Note the cached Cwd/HasSubagents reflect the disk
// state at parse time; resume re-checks the dir and fails gracefully if it
// moved.
type Cache struct {
	path    string
	entries map[string]cacheEntry
	used    map[string]bool // paths seen this run; Save prunes the rest
	dirty   bool
}

type cacheEntry struct {
	Size    int64         `json:"size"`
	MTimeNS int64         `json:"mtimeNs"`
	Mission model.Mission `json:"mission"`
}

// LoadCache reads the scan cache from Houston's data dir (an empty cache if
// missing or unreadable).
func LoadCache() *Cache {
	c := &Cache{
		path:    filepath.Join(accounts.StoreDir(), "scan-cache.json"),
		entries: map[string]cacheEntry{},
		used:    map[string]bool{},
	}
	if b, err := os.ReadFile(c.path); err == nil {
		var on struct {
			Entries map[string]cacheEntry `json:"entries"`
		}
		if json.Unmarshal(b, &on) == nil && on.Entries != nil {
			c.entries = on.Entries
		}
	}
	return c
}

// lookup returns the cached mission if the file is unchanged. Nil-safe so the
// uncached Scan path can share the walker.
func (c *Cache) lookup(path string, fi os.FileInfo) (model.Mission, bool) {
	if c == nil || fi == nil {
		return model.Mission{}, false
	}
	e, ok := c.entries[path]
	if !ok || e.Size != fi.Size() || e.MTimeNS != fi.ModTime().UnixNano() {
		return model.Mission{}, false
	}
	c.used[path] = true
	return e.Mission, true
}

func (c *Cache) put(path string, fi os.FileInfo, m model.Mission) {
	if c == nil || fi == nil {
		return
	}
	c.entries[path] = cacheEntry{Size: fi.Size(), MTimeNS: fi.ModTime().UnixNano(), Mission: m}
	c.used[path] = true
	c.dirty = true
}

// Save persists the entries touched this run — pruning transcripts that no
// longer exist — atomically and best-effort.
func (c *Cache) Save() {
	if c == nil || c.path == "" {
		return
	}
	if !c.dirty && len(c.used) == len(c.entries) {
		return // clean pass over the exact same set: nothing to write
	}
	kept := map[string]cacheEntry{}
	for p := range c.used {
		if e, ok := c.entries[p]; ok {
			kept[p] = e
		}
	}
	b, err := json.Marshal(struct {
		Entries map[string]cacheEntry `json:"entries"`
	}{kept})
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(c.path), 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), "scan-cache-*.tmp")
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
	if os.Rename(name, c.path) != nil {
		os.Remove(name)
	}
}
