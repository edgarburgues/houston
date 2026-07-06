package module

// Installs are snapshots: `add` materializes the source (a local directory
// copy or a hardened shallow git clone), validates the name and manifest
// BEFORE anything lands under modules/, stages inside the destination
// filesystem and finishes with a same-directory rename. Nothing is ever
// executed at install time — the trust boundary is `module enable`, and what
// the user reviews in modules/<name>/ must be the exact bytes that run.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"houston/internal/accounts"
)

// cloneTimeout bounds the whole git clone; output is captured, so anything
// that would block interactively (credentials, a hung remote) must die here.
const cloneTimeout = 120 * time.Second

// Add installs a module from a local directory or a git URL and registers it
// DISABLED — installing code from a URL must not arm it in the same breath.
// forcedName, when set, becomes the install name (the escape hatch the
// collision errors point at with "pick --name"): loading requires manifest
// name == directory name, so the staged module.json's "name" field is
// rewritten to match before anything lands.
func Add(src, forcedName string) (Entry, error) {
	if forcedName != "" {
		if err := SafeName(forcedName); err != nil {
			return Entry{}, err
		}
	}
	srcDir := src
	source := src
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		dir, cleanup, err := gitClone(src)
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return Entry{}, err
		}
		srcDir = dir
	} else if abs, err := filepath.Abs(src); err == nil {
		source = abs
	}
	// A junctioned/symlinked source ROOT is the user's own arrangement (this
	// deployment reaches the store through junctions); links INSIDE the tree
	// are what copyTree rejects.
	if resolved, err := filepath.EvalSymlinks(srcDir); err == nil {
		srcDir = resolved
	}

	b, err := os.ReadFile(filepath.Join(srcDir, "module.json"))
	if err != nil {
		return Entry{}, fmt.Errorf("source has no readable module.json: %v", err)
	}
	man, err := ParseManifest(b)
	if err != nil {
		return Entry{}, err
	}
	name := man.Name
	if forcedName != "" {
		name = forcedName
	}
	dst := filepath.Join(Dir(), name)
	if !within(Dir(), dst) {
		return Entry{}, fmt.Errorf("invalid module name (escapes the store): %q", name)
	}
	// Fast pre-check outside the lock for an early, friendly error; re-checked
	// under the lock below so two concurrent adds can't both win.
	if err := checkCollision(name); err != nil {
		return Entry{}, err
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return Entry{}, err
	}
	// Stage inside the destination filesystem, never %TEMP%: the store can sit
	// behind a junction on another volume and a cross-volume os.Rename fails
	// with ERROR_NOT_SAME_DEVICE. The TUI sweeps orphaned .staging-* dirs.
	staging, err := os.MkdirTemp(Dir(), ".staging-")
	if err != nil {
		return Entry{}, err
	}
	if err := copyTree(srcDir, staging); err != nil {
		os.RemoveAll(staging)
		return Entry{}, err
	}
	if name != man.Name {
		if err := renameManifest(staging, name); err != nil {
			os.RemoveAll(staging)
			return Entry{}, err
		}
	}

	lk, err := lockRegistry()
	if err != nil {
		os.RemoveAll(staging)
		return Entry{}, err
	}
	defer lk.Release()
	if err := checkCollision(name); err != nil {
		os.RemoveAll(staging)
		return Entry{}, err
	}
	// An unregistered dir is a supported state and must never be silently
	// clobbered — the stat must FAIL before the rename lands.
	if _, err := os.Stat(dst); err == nil {
		os.RemoveAll(staging)
		return Entry{}, fmt.Errorf("module directory %q already exists; remove it or pick --name", dst)
	}
	if err := os.Rename(staging, dst); err != nil {
		os.RemoveAll(staging)
		return Entry{}, err
	}
	list, err := RegLoad()
	if err == nil {
		e := Entry{Name: name, Source: source, AddedAt: accounts.Now(), Enabled: false}
		if err = RegSave(append(list, e)); err == nil {
			return e, nil
		}
	}
	// Registration failed after the dir landed: roll the files back rather
	// than leave a surprise unregistered dir behind.
	os.RemoveAll(dst)
	return Entry{}, err
}

// Remove uninstalls a module. The registry entry goes first (under the lock —
// nothing new can execute once deregistered), then the directory. A partial
// RemoveAll (on Windows a running handler holds its script open — sharing
// violation mid-tree) is surfaced, never silent: the remainder stays visible
// as an unregistered dir in ls/doctor.
func Remove(name string) error {
	if err := SafeName(name); err != nil {
		return err
	}
	target := filepath.Join(Dir(), name)
	if !within(Dir(), target) {
		return fmt.Errorf("invalid module name (escapes the store): %q", name)
	}
	lk, err := lockRegistry()
	if err != nil {
		return err
	}
	list, err := RegLoad()
	if err != nil {
		lk.Release()
		return err
	}
	registered := false
	kept := list[:0]
	for _, e := range list {
		if e.Name == name {
			registered = true
			continue
		}
		kept = append(kept, e)
	}
	if registered {
		if err := RegSave(kept); err != nil {
			lk.Release()
			return err
		}
	}
	lk.Release()
	if _, err := os.Stat(target); err != nil {
		if registered {
			return nil // entry removed; there was no directory (the "missing" state)
		}
		return fmt.Errorf("module %q is not installed", name)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("module files could not be fully removed (a handler may still be running); retry after handlers exit: %v", err)
	}
	return nil
}

// renameManifest rewrites the staged module.json "name" field so the
// loader's manifest-name == directory-name rule holds for a --name install.
// Every other top-level field keeps its exact bytes (RawMessage passthrough);
// only key order and indentation are normalized.
func renameManifest(stagingDir, name string) error {
	p := filepath.Join(stagingDir, "module.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	quoted, err := json.Marshal(name)
	if err != nil {
		return err
	}
	fields["name"] = quoted
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(out, '\n'), 0o600)
}

// checkCollision rejects names already taken by a registry entry or by any
// entry under modules/. Both checks are case-insensitive: NTFS and APFS fold
// case variants onto one directory, so "Hello" and "hello" are the same slot.
func checkCollision(name string) error {
	list, err := RegLoad()
	if err != nil {
		return err
	}
	for _, e := range list {
		if strings.EqualFold(e.Name, name) {
			return fmt.Errorf("module %q is already registered (as %q); remove it first with: houston module rm %s", name, e.Name, e.Name)
		}
	}
	dirents, _ := os.ReadDir(Dir())
	for _, d := range dirents {
		if strings.EqualFold(d.Name(), name) {
			return fmt.Errorf("module directory %q already exists; remove it or pick --name", filepath.Join(Dir(), d.Name()))
		}
	}
	return nil
}

// gitClone shallow-clones url into a temp dir with the hardened invocation
// shared with fleet's skill installer: remote-helper transports and option
// injection rejected up front, protocol.ext.allow=never, shallow
// single-branch clone without submodules, a hard timeout, and prompts
// disabled so a private repo fails fast with git's error text instead of
// hanging on invisible credential input. The clone dir is never moved into
// place — its files go through the same link-rejecting copy as local installs.
func gitClone(url string) (string, func(), error) {
	// Source validation comes first: a hostile source must be rejected for
	// being hostile, not accepted-then-failed because git happens to be absent.
	if err := validateGitSource(url); err != nil {
		return "", nil, err
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", nil, errors.New("git is not on the PATH; required to install modules from a repo")
	}
	tmp, err := os.MkdirTemp("", "houston-module-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	repo := filepath.Join(tmp, "repo")
	ctx, cancel := context.WithTimeout(context.Background(), cloneTimeout)
	defer cancel()
	// -c protocol.ext.allow=never neutralizes git's `ext::` remote helper (a
	// command-execution vector); "--" stops the URL from being parsed as an
	// option.
	cmd := exec.CommandContext(ctx, "git",
		"-c", "protocol.ext.allow=never",
		"clone", "--depth", "1", "--single-branch", "--no-recurse-submodules",
		"--", url, repo)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // output is captured: a credential prompt would hang invisibly
		"GCM_INTERACTIVE=never", // ...and so would a Git Credential Manager popup
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", cleanup, fmt.Errorf("git clone timed out (%s)", cloneTimeout)
		}
		return "", cleanup, fmt.Errorf("git clone failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// Snapshot install: history is dead weight and .git could hide hooks.
	if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
		return "", cleanup, err
	}
	return repo, cleanup, nil
}

// validateGitSource rejects clone sources that could execute commands or
// inject options: git's "<transport>::<address>" remote-helper syntax (ext::,
// fd::…), file:// URLs, and anything beginning with '-'. Normal http(s)/git/
// ssh URLs and scp-like git@host:path remain allowed. Copied from fleet to
// keep the dependency graph flat.
func validateGitSource(u string) error {
	if strings.TrimSpace(u) == "" {
		return errors.New("no source given")
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

// copyTree copies src into dst, FAILING (not skipping) on any entry that is
// not a plain file or directory: symlinks and Windows reparse points /
// junctions (ModeIrregular since Go 1.23) can alias content from outside the
// reviewed tree — a link at install time means the bytes the user reviews are
// not the bytes that run. Fleet's skill copy skips links; here a skipped
// entry would leave a module that reviews clean and behaves differently.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		if d.Type() != 0 { // symlink, reparse point, device, pipe…
			return fmt.Errorf("refusing to install: %q is not a regular file (symlink or reparse point)", rel)
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
}

// copyFile copies one regular file, preserving its permission bits (POSIX
// handlers may rely on an exec bit).
func copyFile(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
