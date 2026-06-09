// Package launch runs `claude` for a chosen account by pointing CLAUDE_CONFIG_DIR
// at that account's per-account config dir — which holds its own /login and
// onboarding, so Claude shows the real account identity. Concurrency-safe: the
// account is just the process's config dir, so different terminals run different
// accounts cleanly without touching any shared credential file.
package launch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ClaudeBin locates the claude executable.
func ClaudeBin() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	if fb := filepath.Join(home, ".local", "bin", "claude.exe"); fileExists(fb) {
		return fb
	}
	return "claude"
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// Cmd builds an *exec.Cmd that runs `claude args...` in dir, under the given
// CLAUDE_CONFIG_DIR (the account's config dir — its login lives there). An empty
// configDir uses claude's default (~/.claude). Run via tea.ExecProcess (TUI) or
// directly.
func Cmd(configDir string, args []string, dir string) *exec.Cmd {
	c := exec.Command(ClaudeBin(), args...)
	if dir != "" {
		c.Dir = dir
	}
	// Strip any inherited CLAUDE_CONFIG_DIR (claudeswap leaves it set in the shell
	// and never restores it) so it can't shadow the account we select — Windows
	// honors the first of two duplicate env entries, which would be the inherited
	// one. With it removed, the value we append is the only one.
	c.Env = stripEnv(os.Environ(), "CLAUDE_CONFIG_DIR")
	if configDir != "" {
		c.Env = append(c.Env, "CLAUDE_CONFIG_DIR="+configDir)
	}
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c
}

// stripEnv returns env without the named variables (case-insensitive, since
// Windows env names are case-insensitive).
func stripEnv(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		eq := strings.IndexByte(e, '=')
		drop := false
		if eq >= 0 {
			for _, n := range names {
				if strings.EqualFold(e[:eq], n) {
					drop = true
					break
				}
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
}
