package fleet

// Skills install into the SHARED store (~/.claude-shared/skills), which every
// account already sees through its junction/symlink — so unlike MCP servers
// and plugin enablement, a skill needs no per-account propagation at all.
// The git-fetch path is hardened: remote-helper transports (ext::, fd::),
// file:// and option-injection ('-'-prefixed sources/refs) are rejected,
// clones run shallow with protocol.ext.allow=never and a timeout, symlinks
// are never copied (a malicious repo could point one at a local secret), and
// installs stage + rename so a failed copy never leaves a half skill behind.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"houston/internal/provision"
)

// SkillsDir is the shared skills directory every account sees.
func SkillsDir() string { return filepath.Join(provision.SharedDir(), "skills") }

// SkillSource describes where to fetch a skill from.
type SkillSource struct {
	URL   string // git remote, or "" when Local is set
	Path  string // subdir inside the repo holding SKILL.md
	Ref   string // branch/tag (default: repo default branch)
	Local string // local directory containing SKILL.md (alternative to URL)
	Name  string // install name (default: base of Path/Local/repo)
}

// InstallSkill fetches the skill and places it in the shared skills dir,
// returning the install path. With force=false an existing skill is an error.
func InstallSkill(s SkillSource, force bool) (string, error) {
	srcDir := s.Local
	if s.Local == "" {
		dir, cleanup, err := gitFetchSubdir(s.URL, s.Path, s.Ref)
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return "", err
		}
		srcDir = dir
	}
	if _, err := os.Stat(filepath.Join(srcDir, "SKILL.md")); err != nil {
		return "", fmt.Errorf("source has no SKILL.md in %q", srcDir)
	}
	name := s.Name
	if name == "" {
		name = filepath.Base(srcDir)
		if name == "repo" && s.URL != "" { // cloned repo root: name after the remote
			name = strings.TrimSuffix(filepath.Base(strings.TrimRight(s.URL, "/")), ".git")
		}
	}
	if err := validateSkillName(name); err != nil {
		return "", err
	}
	dst := filepath.Join(SkillsDir(), name)
	if !within(SkillsDir(), dst) {
		return "", fmt.Errorf("invalid skill name (escapes the store): %q", name)
	}
	if _, err := os.Stat(dst); err == nil && !force {
		return "", fmt.Errorf("skill %q is already installed (use --force to overwrite)", name)
	}
	if err := os.MkdirAll(SkillsDir(), 0o755); err != nil {
		return "", err
	}
	// Stage first and swap in only on success: a failed copy never leaves a
	// half-populated skill, and --force never destroys the previous skill
	// before the new one is fully in place.
	staging := dst + ".tmp-install"
	os.RemoveAll(staging)
	if err := copyTree(srcDir, staging); err != nil {
		os.RemoveAll(staging)
		return "", err
	}
	if err := os.RemoveAll(dst); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("could not replace the previous skill: %w", err)
	}
	if err := os.Rename(staging, dst); err != nil {
		os.RemoveAll(staging)
		return "", err
	}
	return dst, nil
}

// RemoveSkill deletes a skill from the shared store.
func RemoveSkill(name string) error {
	if err := validateSkillName(name); err != nil {
		return err
	}
	dst := filepath.Join(SkillsDir(), name)
	if !within(SkillsDir(), dst) {
		return fmt.Errorf("invalid skill name (escapes the store): %q", name)
	}
	if _, err := os.Stat(dst); err != nil {
		return fmt.Errorf("skill %q is not installed", name)
	}
	return os.RemoveAll(dst)
}

// ListSkills returns the names of the skills in the shared store (dirs
// containing a SKILL.md).
func ListSkills() ([]string, error) {
	entries, err := os.ReadDir(SkillsDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(SkillsDir(), e.Name(), "SKILL.md")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// validateSkillName rejects names that aren't a single clean path element (no
// separators, no "."/"..") — anything else could write or delete outside the
// shared skills dir.
func validateSkillName(name string) error {
	if name == "" {
		return fmt.Errorf("the skill has no name")
	}
	if name != filepath.Base(name) || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid skill name: %q", name)
	}
	return nil
}

// within reports whether p stays inside root (no "../" escape).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// gitFetchSubdir shallow-clones url into a temp dir and returns the path to
// sub within it, plus a cleanup func for the temp dir.
func gitFetchSubdir(url, sub, ref string) (string, func(), error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", nil, fmt.Errorf("git is not on the PATH; required to install skills from a repo")
	}
	if err := validateGitSource(url); err != nil {
		return "", nil, err
	}
	if strings.HasPrefix(ref, "-") {
		return "", nil, fmt.Errorf("invalid ref (starts with '-'): %q", ref)
	}
	tmp, err := os.MkdirTemp("", "houston-skill-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	repo := filepath.Join(tmp, "repo")
	// -c protocol.ext.allow=never neutralizes git's `ext::` remote helper (a
	// command-execution vector); "--" stops the URL from being parsed as an
	// option.
	args := []string{"-c", "protocol.ext.allow=never", "clone", "--depth", "1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, "--", url, repo)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0") // fail fast, don't hang on a credential prompt
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", cleanup, fmt.Errorf("git clone timed out (120s)")
		}
		return "", cleanup, fmt.Errorf("git clone failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	dir := filepath.Join(repo, filepath.FromSlash(sub))
	if !within(repo, dir) {
		return "", cleanup, fmt.Errorf("the skill path escapes the repository: %q", sub)
	}
	return dir, cleanup, nil
}

// validateGitSource rejects clone sources that could execute commands or
// inject options: git's "<transport>::<address>" remote-helper syntax (ext::,
// fd::…), file:// URLs, and anything beginning with '-'. Normal http(s)/git/
// ssh URLs and scp-like git@host:path remain allowed.
func validateGitSource(u string) error {
	if strings.TrimSpace(u) == "" {
		return fmt.Errorf("no source URL given")
	}
	if strings.HasPrefix(u, "-") {
		return fmt.Errorf("invalid source (starts with '-'): %q", u)
	}
	if strings.Contains(u, "::") {
		return fmt.Errorf("git transport not allowed in source: %q", u)
	}
	if strings.HasPrefix(strings.ToLower(u), "file://") {
		return fmt.Errorf("git transport not allowed in source: %q", u)
	}
	return nil
}

// copyTree recursively copies src to dst, skipping symlinks: a malicious repo
// could ship one pointing at a local secret and we'd otherwise copy the
// target's contents into the shared, Claude-visible skills dir.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
