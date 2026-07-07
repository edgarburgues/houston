package module

// Invoke is the ONLY hardened exec path: transforms, previews, statusline
// segments, non-interactive actions and `module test` all go through it.
// Interactive actions are exempt by design (see ExecAction): they own the
// real terminal, so pipes, process groups, timeouts and caps would break
// exactly the things the user is looking at.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"houston/internal/accounts"
)

// invokeLocks serializes self-overlap per module name. A package-level map,
// never a Module field: Module structs are copied through Bubble Tea value
// receivers and goroutine closures, where a mutex field would be a silently
// broken copy. Distinct modules run concurrently by design.
var invokeLocks sync.Map

// waitDelay bounds Wait after the process exits or the context fires: a
// grandchild inheriting the stdout pipe would otherwise keep Wait blocked
// forever — the classic exec-runner bug, fatal inside a TUI goroutine.
const waitDelay = 2 * time.Second

// stderrTail is how much handler stderr is kept for the log — the END of the
// stream, where the stack trace lives.
const stderrTail = 32 << 10

// ExecError is a handler failure with everything the failure-policy table
// and modules.log need.
type ExecError struct {
	Module   string
	Event    string
	ExitCode int
	Timeout  bool
	Stderr   []byte // ≤ stderrTail bytes, the end of the stream
	Err      error
}

func (e *ExecError) Error() string { return e.Err.Error() }
func (e *ExecError) Unwrap() error { return e.Err }

// handlerEnv builds a handler's environment: the inherited one minus
// CLAUDE_CONFIG_DIR (same hygiene as launch.Cmd — a handler spawning `claude`
// must not inherit the wrong account) plus the HOUSTON_* contract.
func handlerEnv(m Module, event string) []string {
	inherited := os.Environ()
	env := make([]string, 0, len(inherited)+6)
	for _, kv := range inherited {
		// Case-insensitive: Windows env names are.
		if eq := strings.IndexByte(kv, '='); eq >= 0 && strings.EqualFold(kv[:eq], "CLAUDE_CONFIG_DIR") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HOUSTON_API=1",
		"HOUSTON_EVENT="+event,
		"HOUSTON_MODULE="+m.Name,
		"HOUSTON_MODULE_DIR="+m.Dir,
		"HOUSTON_STORE_DIR="+accounts.StoreDir(),
		"HOUSTON_VERSION="+HoustonVersion,
	)
}

// resolveArgv applies the command-resolution rule at exec time: a bare name
// resolves via PATH (exec.Command's LookPath), a separator-bearing path is
// joined to the module dir and must stay within it. Absolute paths are
// rejected at manifest validation but tolerated here — `module test`
// fixtures and internal callers pass the test binary directly. Elements [1:]
// are never touched.
func resolveArgv(m Module, argv []string) (string, []string, error) {
	if len(argv) == 0 {
		return "", nil, errors.New("empty command")
	}
	c := argv[0]
	switch {
	case filepath.IsAbs(c):
		return c, argv[1:], nil
	case strings.ContainsAny(c, `/\`):
		p := filepath.Join(m.Dir, filepath.FromSlash(strings.ReplaceAll(c, `\`, "/")))
		if !within(m.Dir, p) {
			return "", nil, errCommandPath
		}
		return p, argv[1:], nil
	default:
		return c, argv[1:], nil
	}
}

// capWriter buffers up to max+1 bytes of handler stdout. On overflow it
// cancels the exec — over cap is a hard failure (truncated JSON must never
// parse) and the writer must not be left running against a closed ear.
// Writes come from exec's single copy goroutine; Bytes is read after Wait.
type capWriter struct {
	buf    bytes.Buffer
	max    int64
	over   bool
	cancel context.CancelFunc
}

func (w *capWriter) Write(p []byte) (int, error) {
	if !w.over {
		if room := w.max + 1 - int64(w.buf.Len()); room > 0 {
			if int64(len(p)) > room {
				w.buf.Write(p[:room])
			} else {
				w.buf.Write(p)
			}
		}
		if int64(w.buf.Len()) > w.max {
			w.over = true
			if w.cancel != nil {
				w.cancel()
			}
		}
	}
	return len(p), nil // swallow the excess; the cancel reaps the writer
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// Invoke execs one handler under the full hardening policy: per-module
// serialization, timeout with process-tree kill, stdout cap, stderr tail,
// WaitDelay reaping, module-dir cwd, contract env. stdin/stdout run on exec's
// own goroutines, so a handler that writes before draining a large envelope
// cannot deadlock the pipes. Nonzero exit discards stdout — the exit code is
// the contract; a half-run script may emit a half-truth. Failures are logged
// to modules.log and returned as *ExecError.
func Invoke(ctx context.Context, m Module, argv []string, env Envelope, outCap int64, timeout time.Duration) ([]byte, error) {
	out, _, err := invokeTail(ctx, m, argv, env, outCap, timeout)
	return out, err
}

// invokeTail is Invoke plus the captured stderr tail: `module test` prints a
// handler's stderr even when the exec succeeds, where ExecError never exists
// to carry it.
func invokeTail(ctx context.Context, m Module, argv []string, env Envelope, outCap int64, timeout time.Duration) ([]byte, []byte, error) {
	lk, _ := invokeLocks.LoadOrStore(m.Name, &sync.Mutex{})
	mu := lk.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	bin, args, err := resolveArgv(m, argv)
	if err != nil {
		return nil, nil, fail(m, env.Event, nil, err, false, -1, 0)
	}
	body, err := marshalEnvelope(env)
	if err != nil {
		return nil, nil, fail(m, env.Event, nil, err, false, -1, 0)
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Dir = m.Dir
	cmd.Env = handlerEnv(m, env.Event)
	cmd.SysProcAttr = runnerSysProcAttr()
	// Kill the whole child tree, not just the direct child: on Windows
	// Process.Kill leaves grandchildren running (and holding our pipes).
	cmd.Cancel = func() error { return killTree(cmd.Process) }
	cmd.WaitDelay = waitDelay
	cmd.Stdin = bytes.NewReader(body)
	out := &capWriter{max: outCap, cancel: cancel}
	cmd.Stdout = out
	tail := &tailBuffer{max: stderrTail}
	cmd.Stderr = tail

	runErr := cmd.Run()
	dur := time.Since(start)
	// A lingering grandchild that kept the pipes open past WaitDelay is not
	// the handler's failure: its own exit status was fine and its output is
	// complete up to the force-close.
	if errors.Is(runErr, exec.ErrWaitDelay) {
		runErr = nil
	}
	timedOut := cctx.Err() == context.DeadlineExceeded

	switch {
	case timedOut:
		return nil, tail.buf, fail(m, env.Event, tail.buf, fmt.Errorf("timed out after %s", timeout), true, -1, dur)
	case out.over:
		return nil, tail.buf, fail(m, env.Event, tail.buf, fmt.Errorf("stdout exceeded %d bytes", outCap), false, -1, dur)
	case runErr != nil:
		code := -1
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			code = ee.ExitCode()
		}
		return nil, tail.buf, fail(m, env.Event, tail.buf, runErr, false, code, dur)
	}
	return out.buf.Bytes(), tail.buf, nil
}

// marshalEnvelope is split out so tests can build oversized payloads without
// an exec.
func marshalEnvelope(env Envelope) ([]byte, error) {
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("envelope: %v", err)
	}
	return b, nil
}

// fail logs one stanza and wraps the cause as *ExecError.
func fail(m Module, event string, stderr []byte, cause error, timedOut bool, code int, dur time.Duration) error {
	LogEvent(m.Name, event,
		fmt.Sprintf("exit=%d dur=%s timeout=%v: %v", code, dur.Round(100*time.Millisecond), timedOut, cause),
		stderr)
	return &ExecError{Module: m.Name, Event: event, ExitCode: code, Timeout: timedOut, Stderr: stderr, Err: cause}
}
