package module

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"

	"houston/internal/flock"
)

func TestLogEventStanza(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	big := bytes.Repeat([]byte("s"), logStderrCap+4096)
	copy(big[len(big)-4:], "TAIL")
	LogEvent("m1", EventSegment, "exit=1 dur=0.1s timeout=false: boom", big)
	b, err := os.ReadFile(LogPath())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "module=m1 event=statusline.segment") || !strings.Contains(s, "boom") {
		t.Fatalf("stanza header:\n%s", s)
	}
	if !strings.Contains(s, "TAIL") {
		t.Fatal("stderr tail must keep the END of the stream")
	}
	if len(b) > logStderrCap+1024 {
		t.Fatalf("stderr not capped: %d bytes", len(b))
	}
}

func TestLogEventConcurrentAppenders(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			LogEvent("conc", EventAction, "exit=1 dur=0s timeout=false: x", nil)
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(LogPath())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "--- "); n != 10 {
		t.Fatalf("want 10 stanzas, got %d", n)
	}
}

func TestTrimLogSkipsUnderContention(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	LogEvent("m", EventAction, "seed", nil)
	// Inflate past the trim threshold.
	f, err := os.OpenFile(LogPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(bytes.Repeat([]byte("x"), logMaxBytes+512))
	f.Close()
	before, _ := os.Stat(LogPath())

	lk, err := flock.Acquire(LogPath()+".lock", 0)
	if err != nil {
		t.Fatal(err)
	}
	TrimLog() // must skip: an appender (us) holds the lock
	lk.Release()
	after, _ := os.Stat(LogPath())
	if after.Size() != before.Size() {
		t.Fatal("trim ran under contention")
	}

	TrimLog() // uncontended: bounds the file
	fi, _ := os.Stat(LogPath())
	if fi.Size() > logMaxBytes {
		t.Fatalf("trim left %d > %d", fi.Size(), logMaxBytes)
	}
}
