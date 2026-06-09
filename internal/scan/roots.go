package scan

import (
	"os"
	"path/filepath"
	"sort"

	"houston/internal/model"
)

// Houston stores conversations across a few well-known roots: the active
// Claude config dir, the global ~/.claude store, the shared store and each
// per-account config dir. Missions are deduped by their resolved real path,
// so accounts linked (junction/symlink) to a shared store collapse to one.

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

// ProjectRoots returns every projects directory worth scanning. Non-existent
// ones are dropped.
func ProjectRoots() []string {
	var roots []string
	add := func(p string) {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			roots = append(roots, p)
		}
	}
	// Single-folder model: the one Claude config dir's projects.
	if cd := os.Getenv("CLAUDE_CONFIG_DIR"); cd != "" {
		add(filepath.Join(cd, "projects"))
	}
	add(filepath.Join(home(), ".claude", "projects"))
	// Shared store + per-account config dirs.
	add(filepath.Join(home(), ".claude-shared", "projects"))
	if entries, err := os.ReadDir(filepath.Join(home(), ".claude-accounts")); err == nil {
		var names []string
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() && len(n) > 8 && n[:8] == "account-" {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			add(filepath.Join(home(), ".claude-accounts", n, "projects"))
		}
	}
	return roots
}

// ScanAll scans every project root and dedupes missions that resolve to the
// same physical file (accounts linked to a shared store point at one inode).
func ScanAll() ([]model.Mission, error) {
	roots := ProjectRoots()
	if len(roots) == 0 {
		roots = []string{filepath.Join(home(), ".claude", "projects")}
	}
	seen := map[string]bool{}
	var all []model.Mission
	for _, r := range roots {
		ms, err := Scan(r)
		if err != nil {
			continue
		}
		for _, m := range ms {
			key := resolvePath(m.Path)
			if seen[key] {
				continue
			}
			seen[key] = true
			all = append(all, m)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].LastTime.After(all[j].LastTime) })
	return all, nil
}

// resolvePath resolves junctions/symlinks so links to a shared store collapse
// to one key. Falls back to a cleaned path if resolution fails.
func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
