// Package flock is a tiny cross-platform advisory lock built on an
// O_CREATE|O_EXCL lock file. It serializes Houston's multi-process critical
// sections — credential refresh, accounts.json mutations, usage-cache probes —
// where several processes (statusline renders, `houston run`, the TUI) can
// collide. Locks abandoned by a crashed owner are broken after staleAfter.
package flock

import (
	"fmt"
	"os"
	"time"
)

// staleAfter is how old a lock file may get before it's considered abandoned
// (owner crashed mid-section) and broken by the next acquirer. Every guarded
// section is a few network calls at most, so half a minute is generous.
const staleAfter = 30 * time.Second

// retryEvery is the poll interval while waiting for a busy lock.
const retryEvery = 25 * time.Millisecond

// Lock is a held lock; Release removes it. Safe to Release a nil Lock.
type Lock struct{ path string }

// TryAcquire attempts to take the lock without waiting. On failure it also
// breaks the lock if it's stale, so the next attempt can succeed.
func TryAcquire(path string) (*Lock, bool) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		// Owner info is best-effort debugging aid, not part of the protocol.
		fmt.Fprintf(f, "%d %s", os.Getpid(), time.Now().Format(time.RFC3339))
		f.Close()
		return &Lock{path: path}, true
	}
	breakIfStale(path)
	return nil, false
}

// Acquire takes the lock, polling for up to wait before giving up.
func Acquire(path string, wait time.Duration) (*Lock, error) {
	deadline := time.Now().Add(wait)
	for {
		if l, ok := TryAcquire(path); ok {
			return l, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("lock busy: %s", path)
		}
		time.Sleep(retryEvery)
	}
}

// Release drops the lock.
func (l *Lock) Release() {
	if l != nil {
		_ = os.Remove(l.path)
	}
}

// breakIfStale removes a lock file whose mtime is older than staleAfter, so a
// crashed owner can't wedge every future run.
func breakIfStale(path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if time.Since(fi.ModTime()) > staleAfter {
		_ = os.Remove(path)
	}
}
