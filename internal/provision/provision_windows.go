//go:build windows

package provision

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// makeLink creates a directory junction at link pointing to target via
// `mklink /J` (junctions need no admin, unlike symlinks).
//
// The command line is assembled by hand with both paths quoted: Go only quotes
// arguments containing whitespace, so an unquoted path holding cmd.exe
// metacharacters (&, ^, |, parentheses) would be parsed — and executed — by
// cmd. Inside double quotes they're literal. Two characters can't be passed
// safely even quoted — '"' itself and '%' (environment expansion happens even
// inside quotes) — so those paths are rejected with a clear error instead of
// silently misbehaving.
func makeLink(link, target string) error {
	for _, p := range []string{link, target} {
		if strings.ContainsAny(p, "\"%\r\n") {
			return fmt.Errorf("path not representable on a cmd.exe command line: %q", p)
		}
	}
	c := exec.Command("cmd")
	c.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: fmt.Sprintf(`cmd /d /c mklink /J "%s" "%s"`, link, target),
		// Heal runs makeLink from the console-less statusline render process;
		// without CREATE_NO_WINDOW (0x08000000, not named in syscall) every
		// relink attempt would flash a conhost window — every ~300 ms while a
		// relink keeps failing. mklink needs no console: stdio is already NUL.
		CreationFlags: 0x08000000,
	}
	return c.Run()
}
