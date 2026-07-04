package flock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExclusion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l1, ok := TryAcquire(p)
	if !ok {
		t.Fatal("first TryAcquire should succeed")
	}
	if _, ok := TryAcquire(p); ok {
		t.Fatal("second TryAcquire should fail while held")
	}
	l1.Release()
	l2, ok := TryAcquire(p)
	if !ok {
		t.Fatal("TryAcquire after Release should succeed")
	}
	l2.Release()
}

func TestAcquireWaitsForRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l1, _ := TryAcquire(p)
	go func() {
		time.Sleep(60 * time.Millisecond)
		l1.Release()
	}()
	l2, err := Acquire(p, 2*time.Second)
	if err != nil {
		t.Fatalf("Acquire should win once the holder releases: %v", err)
	}
	l2.Release()
}

func TestStaleLockIsBroken(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	if _, ok := TryAcquire(p); !ok {
		t.Fatal("setup acquire failed")
	}
	// Backdate the lock beyond staleAfter: the owner "crashed".
	old := time.Now().Add(-2 * staleAfter)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(p, time.Second)
	if err != nil {
		t.Fatalf("stale lock should be broken and re-acquired: %v", err)
	}
	l.Release()
}

func TestReleaseNilIsSafe(t *testing.T) {
	var l *Lock
	l.Release() // must not panic
}
