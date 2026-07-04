package scan

import (
	"os"
	"path/filepath"
	"testing"
)

const line1 = `{"type":"user","sessionId":"abc","cwd":"C:\\x","timestamp":"2026-01-02T10:00:00Z","message":{"role":"user","content":"hola"}}` + "\n"
const line2 = `{"type":"user","sessionId":"abc","cwd":"C:\\x","timestamp":"2026-01-02T10:05:00Z","message":{"role":"user","content":"otra"}}` + "\n"

func writeTranscript(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanCacheReuseAndInvalidate(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	root := t.TempDir()
	proj := filepath.Join(root, "C--x")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(proj, "abc.jsonl")
	writeTranscript(t, f, line1)

	c := LoadCache()
	ms, err := scanRoot(root, c)
	if err != nil || len(ms) != 1 {
		t.Fatalf("scan inicial: %d misiones, err=%v", len(ms), err)
	}
	if ms[0].UserMsgs != 1 {
		t.Fatalf("UserMsgs = %d, want 1", ms[0].UserMsgs)
	}
	c.Save()

	// A fresh cache load must serve the unchanged file without re-parsing.
	c2 := LoadCache()
	fi, _ := os.Stat(f)
	if _, ok := c2.lookup(f, fi); !ok {
		t.Fatal("expected a cache hit for the unchanged transcript")
	}

	// Appending a line changes the size: the entry must invalidate and a
	// rescan must see the new message.
	writeTranscript(t, f, line1+line2)
	fi2, _ := os.Stat(f)
	if _, ok := c2.lookup(f, fi2); ok {
		t.Fatal("expected invalidation after append")
	}
	ms2, err := scanRoot(root, c2)
	if err != nil || len(ms2) != 1 {
		t.Fatalf("rescan: %d misiones, err=%v", len(ms2), err)
	}
	if ms2[0].UserMsgs != 2 {
		t.Fatalf("tras append UserMsgs = %d, want 2", ms2[0].UserMsgs)
	}
}

func TestScanCachePrunesDeleted(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	root := t.TempDir()
	proj := filepath.Join(root, "C--x")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(proj, "keep.jsonl")
	gone := filepath.Join(proj, "gone.jsonl")
	writeTranscript(t, keep, line1)
	writeTranscript(t, gone, line1)

	c := LoadCache()
	if _, err := scanRoot(root, c); err != nil {
		t.Fatal(err)
	}
	c.Save()

	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	c2 := LoadCache()
	if _, err := scanRoot(root, c2); err != nil {
		t.Fatal(err)
	}
	c2.Save()

	c3 := LoadCache()
	if _, ok := c3.entries[gone]; ok {
		t.Error("deleted transcript should be pruned from the cache")
	}
	if _, ok := c3.entries[keep]; !ok {
		t.Error("surviving transcript should stay cached")
	}
}
