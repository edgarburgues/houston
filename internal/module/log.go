package module

// modules.log is the single append-only trail of handler failures — one
// stanza per failed exec, shared by every surface and every process (TUI,
// statusline renders, CLI). All writes and the trim share one lock; logging
// is best-effort and must never block a render, so contention drops the
// stanza rather than wait.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"houston/internal/accounts"
	"houston/internal/flock"
)

func logDir() string { return filepath.Join(accounts.StoreDir(), "logs") }

// LogPath is where module handler failures are recorded (`module log` reads it).
func LogPath() string { return filepath.Join(logDir(), "modules.log") }

const (
	logLockWait  = 250 * time.Millisecond // best-effort: drop the stanza rather than block
	logMaxBytes  = 1 << 20                // trim target at TUI start
	logStderrCap = 8 << 10                // stderr tail per stanza
)

// LogEvent appends one failure stanza: a `--- <ts> module=X event=Y <reason>`
// header plus the stderr tail. Open-append-close per stanza under the log
// lock; every error here is swallowed by design.
func LogEvent(mod, event, reason string, stderr []byte) {
	if err := os.MkdirAll(logDir(), 0o700); err != nil {
		return
	}
	lk, err := flock.Acquire(LogPath()+".lock", logLockWait)
	if err != nil {
		return
	}
	defer lk.Release()
	f, err := os.OpenFile(LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if len(stderr) > logStderrCap {
		stderr = stderr[len(stderr)-logStderrCap:] // the end holds the stack trace
	}
	fmt.Fprintf(f, "--- %s module=%s event=%s %s\n", accounts.Now(), mod, event, reason)
	if tail := bytes.TrimSpace(stderr); len(tail) > 0 {
		f.Write(append(tail, '\n'))
	}
}

// TrimLog bounds modules.log to its last logMaxBytes, cut on a line boundary.
// TryAcquire + skip on contention: an in-place replace against concurrent
// statusline appenders (Windows has no FILE_SHARE_DELETE here) would fail
// forever or silently drop stanzas written inside the copy window. Called at
// TUI start; a skipped trim just waits for the next quiet start.
func TrimLog() {
	lk, ok := flock.TryAcquire(LogPath() + ".lock")
	if !ok {
		return
	}
	defer lk.Release()
	fi, err := os.Stat(LogPath())
	if err != nil || fi.Size() <= logMaxBytes {
		return
	}
	b, err := os.ReadFile(LogPath())
	if err != nil || len(b) <= logMaxBytes {
		return
	}
	b = b[len(b)-logMaxBytes:]
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[i+1:]
	}
	_ = writeFileAtomic(LogPath(), b, 0o600)
}
