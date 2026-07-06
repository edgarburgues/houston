//go:build !windows

package module

import (
	"os"
	"syscall"
)

// runnerSysProcAttr puts hardened handlers in their own process group so the
// negative-pid kill below reaps the whole tree. Interactive actions must NOT
// get this — their own group would stop terminal Ctrl-C from reaching them.
func runnerSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killTree kills the handler's whole process group.
func killTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err == nil || err == syscall.ESRCH {
		return nil
	}
	return p.Kill()
}
