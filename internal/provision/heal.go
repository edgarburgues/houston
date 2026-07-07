package provision

// Heal is the automatic self-repair for the shared data links, run at every
// launch (`houston run`, the TUI's resume and account-launch paths, TUI
// start) and on every statusline render. The plans/todos junctions drift
// recurrently: something removes the link, and the next plan/todo write
// recreates the path as a real directory that traps content in one account.
// Repair used to depend on someone noticing and running `doctor --fix` — by
// which time context was already split. Heal closes that loop unattended:
//
//   - safe states (link missing / wrong target / real empty dir) re-link
//     silently;
//   - a real dir WITH data is merged into the shared store — collision-safe,
//     nothing is removed before it provably exists in shared — then linked;
//   - anything it cannot fix without risk is reported, never touched.
//
// The happy path (everything linked) does no writes at all — a handful of
// lstats per account — which is what makes the statusline cadence affordable.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"houston/internal/accounts"
	"houston/internal/flock"
)

// HealResult records what one pass changed. Relinked entries are the silent
// fixes; Merged and Skipped are the Notices a caller should surface.
type HealResult struct {
	Relinked []string
	Merged   []string
	Skipped  []string
}

// Quiet reports whether nothing surface-worthy happened.
func (r HealResult) Quiet() bool { return len(r.Merged) == 0 && len(r.Skipped) == 0 }

// Notices returns the lines worth showing to the user (merges and skips —
// silent relinks are by design not among them).
func (r HealResult) Notices() []string {
	return append(append([]string{}, r.Merged...), r.Skipped...)
}

// Heal audits every existing account's data-dir links and repairs what it can
// without risk. Best-effort by contract: it never returns an error, because no
// launch or render must ever fail on account of a repair — problems land in
// Skipped. Accounts whose config dir does not exist are doctor's business,
// not launch drift, and are left alone.
func Heal(accs []accounts.Account) HealResult {
	var res HealResult
	shared := SharedDir()
	ensured := false
	for _, a := range accs {
		cd := a.ResolveConfigDir()
		if cd == "" || !isDir(cd) {
			continue
		}
		for _, d := range ShareDirs {
			link, target := filepath.Join(cd, d), filepath.Join(shared, d)
			state := classify(link, target)
			if state == LinkOK {
				continue
			}
			if !ensured {
				if err := EnsureShared(); err != nil {
					res.Skipped = append(res.Skipped, "shared store: "+err.Error())
					return res
				}
				ensured = true
			}
			name := "account-" + a.ID + "/" + d
			switch state {
			case LinkMissing:
				relink(&res, name, link, target)
			case LinkRealEmpty, LinkWrong:
				// Removes only the empty dir / the link itself, never the target.
				_ = os.Remove(link)
				relink(&res, name, link, target)
			case LinkRealData:
				mergeAndLink(&res, a.ID, name, link, target)
			case LinkFile:
				res.Skipped = append(res.Skipped, name+": a regular file sits where the link should go; move it aside and re-run")
			}
		}
	}
	return res
}

// relink creates the junction/symlink, tolerating the race where a concurrent
// heal (another launch, a statusline render) won first.
func relink(res *HealResult, name, link, target string) {
	if err := makeLink(link, target); err != nil {
		if classify(link, target) == LinkOK {
			res.Relinked = append(res.Relinked, name+" relinked (by a concurrent heal)")
			return
		}
		res.Skipped = append(res.Skipped, name+": relink failed: "+err.Error())
		return
	}
	res.Relinked = append(res.Relinked, name+" relinked")
}

// mergeAndLink drains a drifted real dir into the shared target and links it.
// Serialized across processes by a try-lock: a loser just leaves the dir for
// the next pass — with the statusline healing every render, "next pass" is
// seconds away. On any failure the remainder stays in place and is reported;
// the link is only created once the dir is verifiably empty.
func mergeAndLink(res *HealResult, accID, name, link, target string) {
	lk, ok := flock.TryAcquire(filepath.Join(SharedDir(), ".heal.lock"))
	if !ok {
		res.Skipped = append(res.Skipped, name+": real dir with data; another heal is running — retried on next launch/render")
		return
	}
	defer lk.Release()
	moved, err := mergeTree(link, target, accID)
	if err != nil {
		res.Skipped = append(res.Skipped, fmt.Sprintf("%s: merge into shared incomplete (%d file(s) moved, remainder kept): %v", name, moved, err))
		return
	}
	if err := removeEmptyTree(link); err != nil {
		// Something landed in the dir between the merge and the removal (a
		// live claude writing a plan, most likely). Nothing is lost; the next
		// pass merges the newcomers too.
		res.Skipped = append(res.Skipped, name+": dir not empty after merge (still being written?); retried on next pass")
		return
	}
	if err := makeLink(link, target); err != nil {
		if classify(link, target) != LinkOK {
			res.Skipped = append(res.Skipped, name+": merged but relink failed: "+err.Error())
			return
		}
	}
	res.Merged = append(res.Merged, fmt.Sprintf("%s: %d file(s) merged into shared, relinked", name, moved))
}

// mergeTree moves every file under src into dst, preserving relative paths.
// Collisions never overwrite: identical content is deduplicated, different
// content lands under a ".from-<account>" conflict name. Returns how many
// files were moved (dedups included) and the first error, leaving the
// remainder in place.
func mergeTree(src, dst, accID string) (int, error) {
	moved := 0
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		to := filepath.Join(dst, rel)
		if d.Type() != 0 {
			// A link inside the drifted dir: relocate it as-is (rename moves
			// the link itself, never follows it). Content behind it is not
			// ours to copy.
			if err := os.Rename(p, uniqueDest(to, accID)); err != nil {
				return fmt.Errorf("%s: %v", rel, err)
			}
			moved++
			return nil
		}
		if fileExists(to) {
			same, err := sameContent(p, to)
			if err == nil && same {
				if err := os.Remove(p); err != nil {
					return fmt.Errorf("%s: %v", rel, err)
				}
				moved++
				return nil
			}
			to = uniqueDest(to, accID)
		}
		if err := moveFile(p, to); err != nil {
			return fmt.Errorf("%s: %v", rel, err)
		}
		moved++
		return nil
	})
	return moved, err
}

// uniqueDest returns a collision-free variant of path: "plan.md" becomes
// "plan.from-work2.md", then "plan.from-work2-2.md" and so on.
func uniqueDest(path, accID string) string {
	if !fileExists(path) && !isDir(path) {
		return path
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for i := 0; ; i++ {
		c := stem + ".from-" + accID + ext
		if i > 0 {
			c = fmt.Sprintf("%s.from-%s-%d%s", stem, accID, i+1, ext)
		}
		if !fileExists(c) && !isDir(c) {
			return c
		}
	}
}

// sameContentMax bounds the byte-compare; anything bigger is treated as
// different (a conflict copy is safe, an unbounded read is not).
const sameContentMax = 8 << 20

func sameContent(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if fa.Size() != fb.Size() || fa.Size() > sameContentMax {
		return false, nil
	}
	ba, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ba, bb), nil
}

// moveFile renames, falling back to an exclusive-create copy for cross-volume
// moves. The source is removed only after the copy is fully flushed.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(dst)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

// removeEmptyTree removes root and its subdirectories bottom-up with
// os.Remove, which refuses non-empty dirs — exactly the safety wanted after a
// merge: if anything reappeared meanwhile, the removal fails and the dir
// stays.
func removeEmptyTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return fmt.Errorf("unexpected file %s", p)
		}
		dirs = append(dirs, p)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		if err := os.Remove(d); err != nil {
			return err
		}
	}
	return nil
}
