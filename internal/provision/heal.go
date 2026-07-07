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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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

// healBudget bounds one merge pass's lock hold and, with it, the stall a
// synchronous caller (launch, a statusline render, the TUI enter key) can
// suffer. It must stay well under flock's 30 s staleAfter: an overrun merge
// would get its lock broken mid-walk, invite a concurrent merger, and then
// delete that merger's lock on release — the same constraint the segment
// cache solves with its 10 s batch budget. Merges are resumable by design;
// whatever the budget cuts off is picked up by the next pass, seconds away
// at the statusline cadence. A var so tests can shrink it.
var healBudget = 10 * time.Second

// errHealBudget aborts the walk when the pass budget expires.
var errHealBudget = errors.New("pass budget expired")

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
	// Re-check under the lock: between the caller's classify and this acquire
	// a concurrent heal may have merged and linked already — walking on would
	// send the merge through the fresh junction into the shared store itself.
	if state := classify(link, target); state != LinkRealData {
		if state != LinkOK {
			res.Skipped = append(res.Skipped, name+": state changed mid-heal ("+state.String()+"); retried on next pass")
		}
		return
	}
	moved, err := mergeTree(link, target, accID, time.Now().Add(healBudget))
	if errors.Is(err, errHealBudget) {
		res.Skipped = append(res.Skipped, fmt.Sprintf("%s: merge paused after %d file(s) (pass budget); continuing next pass", name, moved))
		return
	}
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

// mergeTree moves every file under src into dst, preserving relative paths,
// until deadline expires (errHealBudget; the remainder waits for the next
// pass). Collisions never overwrite: identical content is deduplicated,
// different content lands under a ".from-<account>" conflict name. Returns
// how many files were moved (dedups included) and the first error, leaving
// the remainder in place.
func mergeTree(src, dst, accID string, deadline time.Time) (int, error) {
	moved := 0
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return errHealBudget
		}
		// The walk root must stay a real directory for the cached entry paths
		// to mean what they meant when WalkDir listed them: if a concurrent
		// heal (say, after a broken lock) linked it meanwhile, p would now
		// resolve THROUGH the junction into the shared store — and the dedup
		// below would delete the very files already merged there.
		if fi, err := os.Lstat(src); err != nil || !fi.IsDir() || isLink(src) {
			return errors.New("source is no longer a real directory (concurrent heal?)")
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
			before, statErr := os.Stat(p)
			same, err := sameContent(p, to)
			if err == nil && same && statErr == nil && unchangedSince(p, before) {
				// The re-stat guards the compare-then-remove window: a live
				// claude session appending between the two would lose
				// everything written after the content snapshot.
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

// unchangedSince reports whether the file still has the size and mtime it had
// at fi — the guard against discarding a file a live writer touched between a
// content snapshot and its removal.
func unchangedSince(p string, fi os.FileInfo) bool {
	cur, err := os.Stat(p)
	return err == nil && cur.Size() == fi.Size() && cur.ModTime().Equal(fi.ModTime())
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

// moveFile moves src to dst without overwriting an existing dst or ever
// leaving a truncated dst behind. Same volume: a hard link claims the
// destination name exclusively (EEXIST if dst appeared since the caller's
// check, where a bare rename would silently replace it) and content follows
// the inode — a live writer's fd keeps landing in the moved file; rename is
// the fallback for filesystems without hard links. Cross-volume: copy
// through a same-dir temp file renamed into place only after a complete
// close (an interrupted copy must not install truncated bytes under the
// canonical name — the next pass would demote the intact original to a
// conflict copy), and src is removed only if it provably did not change
// during the copy — a live writer keeps its file for the next pass.
func moveFile(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return os.Remove(src)
	} else if errors.Is(err, fs.ErrExist) {
		return err // never overwrite
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	before, err := os.Stat(src)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".heal-tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if !unchangedSince(src, before) {
		os.Remove(tmp.Name())
		return errors.New("changed during copy (live writer?); left for the next pass")
	}
	if fileExists(dst) || isDir(dst) {
		os.Remove(tmp.Name())
		return errors.New("destination appeared during copy")
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
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
