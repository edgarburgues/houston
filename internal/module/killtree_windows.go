//go:build windows

package module

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// createNoWindow keeps headless handler execs from flashing a console window
// per invocation (not in the syscall package; documented Windows constant).
const createNoWindow = 0x08000000

// runnerSysProcAttr puts hardened handlers in their own process group (so
// console Ctrl-C aimed at Houston never reaches them) without a console
// window. Interactive actions must NOT get these flags — a new process group
// never receives the console's Ctrl-C and CREATE_NO_WINDOW detaches the
// console entirely.
func runnerSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow}
}

// killTree kills the handler and its whole child tree. Process.Kill on
// Windows kills only the direct child; an orphaned grandchild would linger
// (WaitDelay unblocks our Wait, but the process itself must die). taskkill
// gets a short budget of its own, then Kill is the fallback.
func killTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	k := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid))
	k.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	if err := k.Run(); err == nil {
		return nil
	}
	return p.Kill()
}
