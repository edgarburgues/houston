// Package resume launches `claude --resume` for a mission with the correct
// working directory. Two things have to be right:
//
//   - cwd: claude resolves the session from projects/<encoded-cwd>/, so we cd
//     into the dir that encodes back to the mission's project folder (see
//     internal/pathenc).
//   - account: each account is its own CLAUDE_CONFIG_DIR (per-account dir with
//     its /login). Resume launches under a logged-in account's config dir,
//     preferring the least-recently-used; the shared projects/ link means any
//     account can resume any session. With no Houston accounts, it falls back to
//     the config dir that physically holds the transcript.
package resume

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"houston/internal/accounts"
	"houston/internal/launch"
	"houston/internal/model"
	"houston/internal/usage"
)

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

	// Resume under the config dir that physically holds the transcript when it
	// carries its own login — legacy/claudeswap stores and per-account dirs do.
	// Only when that dir has no credentials (e.g. the bare shared store) fall back
	// to a logged-in Houston account dir, which links to the same store and has a
	// login. No token injection: identity comes from the dir's own login.
	configDir := configDirOf(m.Path)
	if !hasCredentials(configDir) {
		if accs, _ := accounts.Load(); len(accs) > 0 {
			var loggedIn []accounts.Account
			for _, a := range accs {
				if a.LoggedIn() {
					loggedIn = append(loggedIn, a)
				}
			}
			if acc, ok := usage.PickLRU(loggedIn); ok {
				if cd := acc.ResolveConfigDir(); cd != "" {
					configDir = cd
				}
				accounts.TouchUse(acc.ID, accounts.Now())
			}
		}
	}
	return launch.Cmd(configDir, []string{"--resume", m.ID}, dir), nil
}

// hasCredentials reports whether a config dir carries its own login.
func hasCredentials(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".credentials.json"))
	return err == nil
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
