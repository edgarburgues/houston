//go:build windows

package browse

import (
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// openDefault opens the URL with the system's default handler — the same
// mechanism Claude Code itself uses when no $BROWSER is set.
func openDefault(u string) error {
	return exec.Command("rundll32", "url,OpenURL", u).Start()
}

// openPrivate opens the URL in a private window of the DEFAULT browser: it
// resolves the user's https handler from the registry (UserChoice ProgId →
// its shell open command), matches the browser family and appends its
// private-window switch. Errors mean "couldn't do it safely" — the caller
// falls back to a normal open.
func openPrivate(u string) error {
	progID, err := defaultProgID()
	if err != nil {
		return err
	}
	cmdline, err := progIDCommand(progID)
	if err != nil {
		return err
	}
	exe := exeFromCommand(cmdline)
	flag := privateFlag(progID + " " + exe)
	if exe == "" || flag == "" {
		return fmt.Errorf("default browser %q has no known private mode", progID)
	}
	return exec.Command(exe, flag, u).Start()
}

// defaultProgID reads the ProgId of the user's default https handler.
func defaultProgID() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\Shell\Associations\UrlAssociations\https\UserChoice`,
		registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("ProgId")
	return v, err
}

// progIDCommand returns the shell open command registered for a ProgId, e.g.
// `"C:\...\msedge.exe" --single-argument %1`.
func progIDCommand(progID string) (string, error) {
	k, err := registry.OpenKey(registry.CLASSES_ROOT, progID+`\shell\open\command`, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("")
	return v, err
}

// exeFromCommand extracts the executable path from a registry shell command:
// the quoted first token, or the first whitespace-delimited token otherwise.
func exeFromCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, `"`) {
		if end := strings.Index(cmd[1:], `"`); end >= 0 {
			return cmd[1 : end+1]
		}
		return ""
	}
	if i := strings.IndexByte(cmd, ' '); i >= 0 {
		return cmd[:i]
	}
	return cmd
}
