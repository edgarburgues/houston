// Package resume launches `claude --resume` for a mission with the correct
// working directory. Two things have to be right:
//
//   - cwd: claude resolves the session from projects/<encoded-cwd>/, so we cd
//     into the dir that encodes back to the mission's project folder (see
//     internal/pathenc).
//   - account: each account is its own CLAUDE_CONFIG_DIR (per-account dir with
//     its /login). The shared projects/ link means any logged-in account can
//     resume any session, so resume balances across accounts exactly like
//     `houston run`: it probes quota and picks the lowest-pressure logged-in
//     account (falling back to least-recently-used if the probe fails). Only
//     when no Houston account is logged in does it fall back to the config dir
//     that physically holds the transcript.
package resume

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"houston/internal/accounts"
	"houston/internal/launch"
	"houston/internal/model"
	"houston/internal/usage"
)

// probeTimeout caps how long resume waits on the usage probe before launching.
const probeTimeout = 8 * time.Second

// Command builds the *exec.Cmd that resumes the mission. Run it via bubbletea's
// tea.ExecProcess so the TUI suspends and hands the terminal to claude.
func Command(m model.Mission) (*exec.Cmd, error) {
	dir := m.Cwd
	if dir == "" {
		dir = filepath.Dir(filepath.Dir(m.Path)) // best effort
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("cwd no existe: %s", dir)
	}

	// Balance the resume across accounts, exactly like `houston run`: the shared
	// projects/ link lets any logged-in account reopen any session, so we probe
	// quota and pick the lowest-pressure logged-in account (usage.Best, which
	// degrades to least-recently-used if probing fails). Everything balances
	// unless the user forces an account elsewhere. Fall back to the config dir
	// that physically holds the transcript only when no Houston account is logged
	// in. No token injection: identity comes from the dir's own login.
	configDir := configDirOf(m.Path)
	if accs, _ := accounts.Load(); len(accs) > 0 {
		var loggedIn []accounts.Account
		for _, a := range accs {
			if a.LoggedIn() {
				loggedIn = append(loggedIn, a)
			}
		}
		if best, _, err := usage.Best(loggedIn, probeTimeout); err == nil {
			if cd := best.ResolveConfigDir(); cd != "" {
				configDir = cd
			}
			accounts.TouchUse(best.ID, accounts.Now())
		}
	}
	return launch.Cmd(configDir, []string{"--resume", m.ID}, dir), nil
}

// configDirOf returns the Claude config dir containing a transcript, i.e. the
// parent of the ".../projects/..." segment. "" if it can't tell (claude default).
func configDirOf(transcriptPath string) string {
	p := filepath.ToSlash(transcriptPath)
	i := strings.LastIndex(p, "/projects/")
	if i < 0 {
		return ""
	}
	return filepath.FromSlash(p[:i])
}

// Hint is the manual command, shown in the UI so the user can copy it.
func Hint(m model.Mission) string {
	return fmt.Sprintf("cd \"%s\"; claude --resume %s", m.Cwd, m.ID)
}
