// Package provision owns Houston's multi-account layout on disk: the shared
// store (~/.claude-shared), each account's CLAUDE_CONFIG_DIR
// (~/.claude-accounts/account-<id>) with its own login + onboarding, and the
// junction/symlink that wires every account's data dirs to the shared store.
//
// This is the Go, cross-platform home for what houston-setup-accounts.ps1 did
// on Windows only: `houston doctor` audits the layout and `houston doctor --fix`
// repairs it (idempotent, no admin — Windows uses directory junctions via
// `mklink /J`, macOS/Linux use symlinks). It never clobbers a real directory
// that holds data; divergence is reported, not silently overwritten.
package provision

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"houston/internal/accounts"
)

// ShareDirs are the data/customization dirs linked from each account into the
// shared store, so every account sees the same conversations, plugins, skills,
// commands, subagents, workflows, rules, styles and themes. Identity dirs
// (.claude.json / .credentials.json) are deliberately NOT here — they stay
// per-account so each account keeps its own login and onboarding.
var ShareDirs = []string{
	"projects", "sessions", "plugins", "plans", "todos", // data
	"skills", "commands", "agents", "workflows", "rules", "output-styles", "themes", // user customizations
}

// seedFiles are copied (not linked) into each account dir if missing, so a fresh
// account starts from the same settings/MCP config without sharing mutable state.
var seedFiles = []string{"settings.json", "mcp.json"}

// SharedDir is the real store every account links into. Override $HOUSTON_SHARED_DIR.
func SharedDir() string {
	if d := os.Getenv("HOUSTON_SHARED_DIR"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude-shared")
}

// LinkState classifies what currently sits at an account's data-dir path.
type LinkState int

const (
	LinkMissing  LinkState = iota // nothing there yet
	LinkOK                        // junction/symlink already points at the shared dir
	LinkWrong                     // a link, but to the wrong target
	LinkRealEmpty                 // a real (empty) dir — safe to replace with a link
	LinkRealData                  // a real dir WITH contents — divergent; never auto-clobbered
	LinkFile                      // a regular file is in the way
)

func (s LinkState) OK() bool    { return s == LinkOK }
func (s LinkState) Drift() bool { return s == LinkWrong || s == LinkRealData || s == LinkFile }

func (s LinkState) String() string {
	switch s {
	case LinkOK:
		return "linked"
	case LinkMissing:
		return "link missing"
	case LinkWrong:
		return "link to wrong target"
	case LinkRealEmpty:
		return "real empty dir (safe to link)"
	case LinkRealData:
		return "real dir WITH data (left untouched)"
	case LinkFile:
		return "a regular file is in the way"
	default:
		return "?"
	}
}

// DirReport is the state of one account data dir.
type DirReport struct {
	Name  string
	State LinkState
}

// AccountReport is the audited state of one account.
type AccountReport struct {
	Account   accounts.Account
	ConfigDir string
	Exists    bool
	LoggedIn  bool
	HasConfig bool // .claude.json present
	Dirs      []DirReport
}

// HasDrift reports whether anything about this account needs fixing.
func (r AccountReport) HasDrift() bool {
	if !r.Exists || !r.HasConfig {
		return true
	}
	for _, d := range r.Dirs {
		if d.State != LinkOK {
			return true
		}
	}
	return false
}

// Audit inspects the shared store and every account without changing anything.
func Audit(accs []accounts.Account) (sharedMissing []string, reports []AccountReport) {
	shared := SharedDir()
	for _, d := range ShareDirs {
		if !isDir(filepath.Join(shared, d)) {
			sharedMissing = append(sharedMissing, d)
		}
	}
	for _, a := range accs {
		cd := a.ResolveConfigDir()
		r := AccountReport{
			Account:   a,
			ConfigDir: cd,
			Exists:    isDir(cd),
			LoggedIn:  a.LoggedIn(),
			HasConfig: fileExists(filepath.Join(cd, ".claude.json")),
		}
		for _, d := range ShareDirs {
			r.Dirs = append(r.Dirs, DirReport{Name: d, State: classify(filepath.Join(cd, d), filepath.Join(shared, d))})
		}
		reports = append(reports, r)
	}
	return sharedMissing, reports
}

// FixResult records what a repair pass changed and what it refused to touch.
type FixResult struct {
	Created  []string // human-readable "what" lines
	Skipped  []string // divergent real dirs / files left untouched (need manual action)
}

// EnsureShared makes the shared store and its data dirs exist as real dirs.
func EnsureShared() error {
	shared := SharedDir()
	if err := os.MkdirAll(shared, 0o755); err != nil {
		return err
	}
	for _, d := range ShareDirs {
		if err := os.MkdirAll(filepath.Join(shared, d), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// Fix repairs the layout: ensures the shared store, each account dir, its seeded
// .claude.json (identity stripped) + settings/mcp, and the data-dir links. It
// never overwrites a real dir that holds data — those are returned in Skipped.
func Fix(accs []accounts.Account) (FixResult, error) {
	var res FixResult
	if err := EnsureShared(); err != nil {
		return res, err
	}
	shared := SharedDir()
	seed := seedConfigJSON() // template: ~/.claude.json minus oauthAccount

	for _, a := range accs {
		cd := a.ResolveConfigDir()
		if cd == "" {
			continue
		}
		if !isDir(cd) {
			if err := os.MkdirAll(cd, 0o700); err != nil {
				return res, err
			}
			res.Created = append(res.Created, "dir "+cd)
		}
		// seed .claude.json (only if missing — never clobber a logged-in identity)
		cj := filepath.Join(cd, ".claude.json")
		if !fileExists(cj) {
			if err := os.WriteFile(cj, seed, 0o600); err != nil {
				return res, err
			}
			res.Created = append(res.Created, "account-"+a.ID+"/.claude.json (seed)")
		}
		// seed settings/mcp from shared if present and missing here
		for _, f := range seedFiles {
			src, dst := filepath.Join(shared, f), filepath.Join(cd, f)
			if fileExists(src) && !fileExists(dst) {
				if b, err := os.ReadFile(src); err == nil {
					_ = os.WriteFile(dst, b, 0o644)
					res.Created = append(res.Created, "account-"+a.ID+"/"+f)
				}
			}
		}
		// link each data dir
		for _, d := range ShareDirs {
			link, target := filepath.Join(cd, d), filepath.Join(shared, d)
			switch classify(link, target) {
			case LinkOK:
				// nothing to do
			case LinkMissing:
				if err := makeLink(link, target); err != nil {
					return res, err
				}
				res.Created = append(res.Created, "account-"+a.ID+"/"+d+" → linked")
			case LinkRealEmpty:
				_ = os.Remove(link)
				if err := makeLink(link, target); err != nil {
					return res, err
				}
				res.Created = append(res.Created, "account-"+a.ID+"/"+d+" → linked (empty dir replaced)")
			case LinkWrong:
				_ = os.Remove(link) // removes only the link, not the target
				if err := makeLink(link, target); err != nil {
					return res, err
				}
				res.Created = append(res.Created, "account-"+a.ID+"/"+d+" → re-linked (target fixed)")
			case LinkRealData:
				res.Skipped = append(res.Skipped, "account-"+a.ID+"/"+d+": real dir with data; merge it into "+target+" manually and re-run")
			case LinkFile:
				res.Skipped = append(res.Skipped, "account-"+a.ID+"/"+d+": a file sits where the link should go")
			}
		}
	}
	return res, nil
}

// ResyncSettings force-copies the shared seed files (settings.json, mcp.json)
// into every account, OVERWRITING the per-account copies. Settings are seeded
// (copied), not linked — each account keeps its own file so they *can*
// diverge — which means edits to the shared file never propagate on their
// own; this is the explicit propagation step (`houston doctor
// --resync-settings`).
func ResyncSettings(accs []accounts.Account) (FixResult, error) {
	var res FixResult
	shared := SharedDir()
	for _, a := range accs {
		cd := a.ResolveConfigDir()
		if cd == "" || !isDir(cd) {
			res.Skipped = append(res.Skipped, "account-"+a.ID+": config dir missing (run doctor --fix first)")
			continue
		}
		for _, f := range seedFiles {
			src := filepath.Join(shared, f)
			if !fileExists(src) {
				continue
			}
			b, err := os.ReadFile(src)
			if err != nil {
				return res, err
			}
			dst := filepath.Join(cd, f)
			// Per-account settings may have diverged on purpose (model, plugins,
			// effort...): skip identical files, and keep a .bak of anything this
			// overwrites so a resync is never a silent data loss.
			if old, err := os.ReadFile(dst); err == nil {
				if string(old) == string(b) {
					continue
				}
				if err := os.WriteFile(dst+".bak", old, 0o644); err != nil {
					return res, err
				}
			}
			if err := os.WriteFile(dst, b, 0o644); err != nil {
				return res, err
			}
			res.Created = append(res.Created, "account-"+a.ID+"/"+f+" ← shared (previous kept as "+f+".bak)")
		}
	}
	return res, nil
}

// --- helpers ---------------------------------------------------------------

func classify(link, target string) LinkState {
	if _, err := os.Lstat(link); err != nil {
		return LinkMissing
	}
	// Detect links by readlink rather than the mode bits: a Windows junction
	// (mklink /J) reports ModeIrregular, not ModeSymlink, but os.Readlink resolves
	// both junctions and symlinks on Go 1.23+.
	if isLink(link) {
		if sameTarget(link, target) {
			return LinkOK
		}
		return LinkWrong
	}
	fi, err := os.Stat(link)
	if err != nil || !fi.IsDir() {
		return LinkFile
	}
	if dirEmpty(link) {
		return LinkRealEmpty
	}
	return LinkRealData
}

func isLink(p string) bool {
	_, err := os.Readlink(p)
	return err == nil
}

// sameTarget reports whether the link points at target. It compares file
// identity with os.SameFile (volume+index on Windows, dev+inode on POSIX), which
// is robust to 8.3 short names and casing — unlike filepath.EvalSymlinks, which
// on Windows does not traverse junctions and would report a false mismatch.
func sameTarget(link, target string) bool {
	li, err1 := os.Stat(link)
	ti, err2 := os.Stat(target)
	if err1 == nil && err2 == nil && os.SameFile(li, ti) {
		return true
	}
	// fallback when target doesn't exist yet: compare the raw readlink target.
	if t, err := os.Readlink(link); err == nil {
		return strings.EqualFold(filepath.Clean(strings.TrimPrefix(t, `\??\`)), filepath.Clean(target))
	}
	return false
}

// seedConfigJSON builds the .claude.json template: the user's ~/.claude.json
// with oauthAccount stripped (so the new account onboards as itself), or a
// minimal onboarded stub if there's no source config.
func seedConfigJSON() []byte {
	h, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(h, ".claude.json"))
	if err == nil {
		var m map[string]json.RawMessage
		if json.Unmarshal(b, &m) == nil {
			delete(m, "oauthAccount")
			if out, err := json.MarshalIndent(m, "", "  "); err == nil {
				return out
			}
		}
	}
	return []byte("{\n  \"hasCompletedOnboarding\": true\n}\n")
}

func isDir(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

func fileExists(p string) bool { fi, err := os.Stat(p); return err == nil && !fi.IsDir() }

func dirEmpty(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	return err != nil // io.EOF => empty
}
