//! Automatic self-repair for the shared data links, run at every launch
//! (`run`, the TUI's resume and account-launch paths, TUI start) and on every
//! statusline render. The plans/todos junctions drift recurrently: something
//! removes the link, and the next plan/todo write recreates the path as a real
//! directory that traps content in one account. Repair used to depend on
//! someone noticing and running a fix — by which time context was already
//! split. `heal` closes that loop unattended:
//!
//! - safe states (link missing / wrong target / real empty dir) re-link silently;
//! - a real dir WITH data is merged into the shared store — collision-safe,
//!   nothing is removed before it provably exists in shared — then linked;
//! - anything it cannot fix without risk is reported, never touched.
//!
//! The happy path (everything linked) does no writes at all — a handful of
//! lstats per account — which is what makes the statusline cadence affordable.

use crate::accounts::Account;
use crate::flock;
use crate::paths::home;
use std::fs;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

/// The data/customization dirs linked from each account into the shared store,
/// so every account sees the same conversations, plugins, skills, commands,
/// subagents, workflows, rules, styles and themes. Identity files
/// (.claude.json / .credentials.json) are deliberately NOT here — they stay
/// per-account so each account keeps its own login and onboarding.
pub const SHARE_DIRS: &[&str] = &[
    // data
    "projects",
    "sessions",
    "plugins",
    "plans",
    "todos",
    // user customizations
    "skills",
    "commands",
    "agents",
    "workflows",
    "rules",
    "output-styles",
    "themes",
];

/// The real store every account links into. Override with $HOUSTON_SHARED_DIR.
pub fn shared_dir() -> PathBuf {
    if let Some(d) = std::env::var_os("HOUSTON_SHARED_DIR") {
        if !d.is_empty() {
            return PathBuf::from(d);
        }
    }
    home().join(".claude-shared")
}

/// What currently sits at an account's data-dir path.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LinkState {
    /// Nothing there yet.
    Missing,
    /// Junction/symlink already points at the shared dir.
    Ok,
    /// A link, but to the wrong target.
    Wrong,
    /// A real (empty) dir — safe to replace with a link.
    RealEmpty,
    /// A real dir WITH contents — divergent; never auto-clobbered.
    RealData,
    /// A regular file is in the way.
    File,
}

impl std::fmt::Display for LinkState {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let s = match self {
            LinkState::Ok => "linked",
            LinkState::Missing => "link missing",
            LinkState::Wrong => "link to wrong target",
            LinkState::RealEmpty => "real empty dir (safe to link)",
            LinkState::RealData => "real dir WITH data (left untouched)",
            LinkState::File => "a regular file is in the way",
        };
        f.write_str(s)
    }
}

/// What one pass changed. `relinked` are the silent fixes; `merged` and
/// `skipped` are what a caller should surface.
#[derive(Debug, Default, Clone)]
pub struct HealResult {
    pub relinked: Vec<String>,
    pub merged: Vec<String>,
    pub skipped: Vec<String>,
}

impl HealResult {
    /// Whether nothing surface-worthy happened.
    pub fn quiet(&self) -> bool {
        self.merged.is_empty() && self.skipped.is_empty()
    }

    /// The lines worth showing to the user (merges and skips — silent relinks
    /// are by design not among them).
    pub fn notices(&self) -> Vec<String> {
        self.merged.iter().chain(self.skipped.iter()).cloned().collect()
    }
}

fn is_dir(p: &Path) -> bool {
    fs::metadata(p).map(|m| m.is_dir()).unwrap_or(false)
}

fn file_exists(p: &Path) -> bool {
    fs::metadata(p).map(|m| !m.is_dir()).unwrap_or(false)
}

fn dir_empty(p: &Path) -> bool {
    fs::read_dir(p).map(|mut it| it.next().is_none()).unwrap_or(false)
}

/// Detect links by readlink rather than mode bits: a Windows junction
/// (mklink /J) is a reparse point, not a symlink, and read_link resolves both.
fn is_link(p: &Path) -> bool {
    fs::read_link(p).is_ok()
}

/// Whether the link points at target. Compares canonical paths — on Windows
/// `canonicalize` goes through GetFinalPathNameByHandle, which DOES traverse
/// junctions (Go's EvalSymlinks does not, hence v1's SameFile dance).
fn same_target(link: &Path, target: &Path) -> bool {
    if let (Ok(a), Ok(b)) = (fs::canonicalize(link), fs::canonicalize(target)) {
        return path_eq(&a, &b);
    }
    // Fallback when the target doesn't exist yet: compare the raw readlink.
    if let Ok(t) = fs::read_link(link) {
        let raw = t.to_string_lossy().replace(r"\??\", "");
        return path_eq(Path::new(&raw), target);
    }
    false
}

/// Path equality, case-insensitive on Windows.
fn path_eq(a: &Path, b: &Path) -> bool {
    #[cfg(windows)]
    {
        a.to_string_lossy().eq_ignore_ascii_case(&b.to_string_lossy())
    }
    #[cfg(not(windows))]
    {
        a == b
    }
}

pub fn classify(link: &Path, target: &Path) -> LinkState {
    if fs::symlink_metadata(link).is_err() {
        return LinkState::Missing;
    }
    if is_link(link) {
        return if same_target(link, target) { LinkState::Ok } else { LinkState::Wrong };
    }
    if !is_dir(link) {
        return LinkState::File;
    }
    if dir_empty(link) {
        LinkState::RealEmpty
    } else {
        LinkState::RealData
    }
}

/// Create a directory junction (Windows) / symlink (unix) at `link` → `target`.
#[cfg(windows)]
fn make_link(link: &Path, target: &Path) -> std::io::Result<()> {
    use std::os::windows::process::CommandExt;
    // Junctions need no admin, unlike symlinks. The command line is assembled
    // by hand with BOTH paths quoted: an unquoted path holding cmd.exe
    // metacharacters (&, ^, |, parentheses) would be parsed — and executed — by
    // cmd. Inside double quotes they are literal. Two characters can't be
    // passed safely even quoted — '"' itself and '%' (environment expansion
    // happens even inside quotes) — so those are rejected outright.
    for p in [link, target] {
        let s = p.to_string_lossy();
        if s.contains(['"', '%', '\r', '\n']) {
            return Err(std::io::Error::other(format!(
                "path not representable on a cmd.exe command line: {s}"
            )));
        }
    }
    // CREATE_NO_WINDOW: heal runs from the console-less statusline render, and
    // without it every relink attempt would flash a conhost window — every few
    // hundred ms while a relink keeps failing. mklink needs no console.
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    let status = std::process::Command::new("cmd")
        .raw_arg(format!(r#"/d /c mklink /J "{}" "{}""#, link.display(), target.display()))
        .creation_flags(CREATE_NO_WINDOW)
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status()?;
    if status.success() {
        Ok(())
    } else {
        Err(std::io::Error::other(format!("mklink /J failed ({status})")))
    }
}

#[cfg(not(windows))]
fn make_link(link: &Path, target: &Path) -> std::io::Result<()> {
    std::os::unix::fs::symlink(target, link)
}

/// Make the shared store and its data dirs exist as real dirs.
pub fn ensure_shared() -> std::io::Result<()> {
    let shared = shared_dir();
    fs::create_dir_all(&shared)?;
    for d in SHARE_DIRS {
        fs::create_dir_all(shared.join(d))?;
    }
    Ok(())
}

/// The files copied (not linked) into each account dir if missing, so a fresh
/// account starts from the same settings/MCP config without sharing mutable
/// state.
pub const SEED_FILES: &[&str] = &["settings.json", "mcp.json"];

/// What a `fix` / `resync_settings` pass created or refused to touch.
#[derive(Debug, Default, Clone)]
pub struct FixResult {
    pub created: Vec<String>,
    pub skipped: Vec<String>,
}

/// The `.claude.json` template for a new account: the user's own
/// `~/.claude.json` with `oauthAccount` STRIPPED (so the account onboards as
/// itself rather than impersonating the default login), or a minimal onboarded
/// stub when there's no source config.
fn seed_config_json() -> Vec<u8> {
    if let Ok(b) = fs::read(home().join(".claude.json")) {
        if let Ok(mut m) = serde_json::from_slice::<serde_json::Map<String, serde_json::Value>>(&b) {
            m.remove("oauthAccount");
            if let Ok(out) = serde_json::to_vec_pretty(&m) {
                return out;
            }
        }
    }
    b"{\n  \"hasCompletedOnboarding\": true\n}\n".to_vec()
}

/// Build the layout for the given accounts: the shared store, each account dir,
/// its seeded `.claude.json` (identity stripped) plus settings/mcp, and the
/// data-dir links. It NEVER overwrites a real dir that holds data — those land
/// in `skipped` (that's `heal`'s job, which can merge them safely).
pub fn fix(accs: &[Account]) -> std::io::Result<FixResult> {
    let mut res = FixResult::default();
    ensure_shared()?;
    let shared = shared_dir();
    let seed = seed_config_json();

    for a in accs {
        let Some(cd) = a.resolve_config_dir() else { continue };
        if !is_dir(&cd) {
            fs::create_dir_all(&cd)?;
            res.created.push(format!("dir {}", cd.display()));
        }
        // Seed .claude.json only if missing — never clobber a logged-in identity.
        let cj = cd.join(".claude.json");
        if !file_exists(&cj) {
            fs::write(&cj, &seed)?;
            res.created.push(format!("account-{}/.claude.json (seed)", a.id));
        }
        // Seed settings/mcp from shared if present there and missing here.
        for f in SEED_FILES {
            let (src, dst) = (shared.join(f), cd.join(f));
            if file_exists(&src) && !file_exists(&dst) {
                if let Ok(b) = fs::read(&src) {
                    let _ = fs::write(&dst, b);
                    res.created.push(format!("account-{}/{f}", a.id));
                }
            }
        }
        // Link each data dir.
        for d in SHARE_DIRS {
            let (link, target) = (cd.join(d), shared.join(d));
            match classify(&link, &target) {
                LinkState::Ok => {}
                LinkState::Missing => {
                    make_link(&link, &target)?;
                    res.created.push(format!("account-{}/{d} → linked", a.id));
                }
                LinkState::RealEmpty => {
                    let _ = remove_link_or_empty_dir(&link);
                    make_link(&link, &target)?;
                    res.created.push(format!("account-{}/{d} → linked (empty dir replaced)", a.id));
                }
                LinkState::Wrong => {
                    let _ = remove_link_or_empty_dir(&link); // the link only, never the target
                    make_link(&link, &target)?;
                    res.created.push(format!("account-{}/{d} → re-linked (target fixed)", a.id));
                }
                LinkState::RealData => res.skipped.push(format!(
                    "account-{}/{d}: real dir with data — heal merges it into {}",
                    a.id,
                    target.display()
                )),
                LinkState::File => {
                    res.skipped.push(format!("account-{}/{d}: a file sits where the link should go", a.id))
                }
            }
        }
    }
    Ok(res)
}

/// Force-copy the shared seed files (settings.json, mcp.json) into every
/// account, OVERWRITING the per-account copies. Settings are seeded (copied),
/// not linked — each account keeps its own file so they *can* diverge — which
/// means edits to the shared file never propagate on their own; this is the
/// explicit propagation step. Anything overwritten is kept as `.bak`, so a
/// resync is never a silent data loss.
pub fn resync_settings(accs: &[Account]) -> std::io::Result<FixResult> {
    let mut res = FixResult::default();
    let shared = shared_dir();
    for a in accs {
        let cd = match a.resolve_config_dir() {
            Some(cd) if is_dir(&cd) => cd,
            _ => {
                res.skipped.push(format!("account-{}: config dir missing (run doctor --fix first)", a.id));
                continue;
            }
        };
        for f in SEED_FILES {
            let src = shared.join(f);
            if !file_exists(&src) {
                continue;
            }
            let b = fs::read(&src)?;
            let dst = cd.join(f);
            // Per-account settings may have diverged on purpose (model, plugins,
            // effort…): skip identical files, back up anything replaced.
            if let Ok(old) = fs::read(&dst) {
                if old == b {
                    continue;
                }
                fs::write(dst.with_extension("json.bak"), &old)?;
            }
            fs::write(&dst, &b)?;
            res.created.push(format!("account-{}/{f} ← shared (previous kept as {f}.bak)", a.id));
        }
    }
    Ok(res)
}

/// Audit every existing account's data-dir links and repair what it can
/// without risk. Best-effort BY CONTRACT: it never returns an error, because
/// no launch or render must ever fail on account of a repair — problems land
/// in `skipped`. Accounts whose config dir does not exist are a `doctor`
/// matter, not launch drift, and are left alone.
pub fn heal(accs: &[Account]) -> HealResult {
    let mut res = HealResult::default();
    let shared = shared_dir();

    // The shared store is ensured UNCONDITIONALLY, not lazily on first drift.
    // A link can point at exactly the right path while that path no longer
    // exists — `classify` then reads it as Ok (the raw target matches), so heal
    // would report everything linked while every read through it failed. Making
    // the store's existence an invariant closes that, and costs only a handful
    // of stats: create_dir_all on an existing directory does not write.
    if let Err(e) = ensure_shared() {
        res.skipped.push(format!("shared store: {e}"));
        return res;
    }

    for a in accs {
        let Some(cd) = a.resolve_config_dir() else { continue };
        if !is_dir(&cd) {
            continue;
        }
        for d in SHARE_DIRS {
            let (link, target) = (cd.join(d), shared.join(d));
            let state = classify(&link, &target);
            if state == LinkState::Ok {
                continue;
            }
            let name = format!("account-{}/{}", a.id, d);
            match state {
                LinkState::Missing => relink(&mut res, &name, &link, &target),
                LinkState::RealEmpty | LinkState::Wrong => {
                    // Removes only the empty dir / the link itself, never the target.
                    let _ = remove_link_or_empty_dir(&link);
                    relink(&mut res, &name, &link, &target);
                }
                LinkState::RealData => merge_and_link(&mut res, &a.id, &name, &link, &target),
                LinkState::File => res.skipped.push(format!(
                    "{name}: a regular file sits where the link should go; move it aside and re-run"
                )),
                LinkState::Ok => {}
            }
        }
    }
    res
}

/// Remove a link or an empty dir at `p`. A junction must be removed as a
/// directory entry, never recursively (that would delete the shared target's
/// contents through it).
fn remove_link_or_empty_dir(p: &Path) -> std::io::Result<()> {
    if is_link(p) {
        // A directory symlink/junction: remove_dir unlinks it without touching
        // what it points at. Fall back to remove_file for file symlinks.
        return fs::remove_dir(p).or_else(|_| fs::remove_file(p));
    }
    fs::remove_dir(p)
}

/// Create the junction/symlink, tolerating the race where a concurrent heal
/// (another launch, a statusline render) won first.
fn relink(res: &mut HealResult, name: &str, link: &Path, target: &Path) {
    match make_link(link, target) {
        Ok(()) => res.relinked.push(format!("{name} relinked")),
        Err(e) => {
            if classify(link, target) == LinkState::Ok {
                res.relinked.push(format!("{name} relinked (by a concurrent heal)"));
            } else {
                res.skipped.push(format!("{name}: relink failed: {e}"));
            }
        }
    }
}

/// Bounds one merge pass's lock hold and, with it, the stall a synchronous
/// caller (a launch, a statusline render, the TUI's enter key) can suffer. It
/// bounds how long OTHER processes can be kept waiting: the lock is an OS lock
/// now, so it is never stolen from a live owner — which makes a bounded hold the
/// only thing standing between a big merge and a stalled statusline. Merges are
/// resumable by design; whatever the budget cuts off is picked up by the next
/// pass, seconds away at the statusline cadence.
const HEAL_BUDGET: Duration = Duration::from_secs(10);

/// Why a merge stopped early: the pass budget expired (resumed next pass), or
/// an I/O problem left the remainder in place.
enum MergeErr {
    Budget,
    Io(String),
}

/// Drain a drifted real dir into the shared target and link it. Serialized
/// across processes by a try-lock: a loser just leaves the dir for the next
/// pass — with the statusline healing every render, "next pass" is seconds
/// away. On any failure the remainder stays in place and is reported; the link
/// is only created once the dir is verifiably empty.
fn merge_and_link(res: &mut HealResult, acc_id: &str, name: &str, link: &Path, target: &Path) {
    let lock_path = shared_dir().join(".heal.lock");
    let Some(_lk) = flock::try_acquire(&lock_path) else {
        res.skipped
            .push(format!("{name}: real dir with data; another heal is running — retried on next launch/render"));
        return;
    };
    // Re-check UNDER the lock: between the caller's classify and this acquire a
    // concurrent heal may have merged and linked already — walking on would
    // send the merge through the fresh junction into the shared store itself.
    let state = classify(link, target);
    if state != LinkState::RealData {
        if state != LinkState::Ok {
            res.skipped.push(format!("{name}: state changed mid-heal ({state}); retried on next pass"));
        }
        return;
    }
    let deadline = Instant::now() + HEAL_BUDGET;
    let mut moved = 0usize;
    match merge_tree(link, target, link, acc_id, deadline, &mut moved) {
        Err(MergeErr::Budget) => {
            res.skipped
                .push(format!("{name}: merge paused after {moved} file(s) (pass budget); continuing next pass"));
            return;
        }
        Err(MergeErr::Io(e)) => {
            res.skipped.push(format!(
                "{name}: merge into shared incomplete ({moved} file(s) moved, remainder kept): {e}"
            ));
            return;
        }
        Ok(()) => {}
    }
    if remove_empty_tree(link).is_err() {
        // Something landed in the dir between the merge and the removal (a live
        // claude writing a plan, most likely). Nothing is lost; the next pass
        // merges the newcomers too.
        res.skipped.push(format!("{name}: dir not empty after merge (still being written?); retried on next pass"));
        return;
    }
    if make_link(link, target).is_err() && classify(link, target) != LinkState::Ok {
        res.skipped.push(format!("{name}: merged but relink failed"));
        return;
    }
    res.merged.push(format!("{name}: {moved} file(s) merged into shared, relinked"));
}

/// Move every file under `src` into `dst`, preserving relative paths, until the
/// deadline expires (the remainder waits for the next pass). Collisions never
/// overwrite: identical content is deduplicated, different content lands under
/// a ".from-<account>" conflict name. `root` is the walk root, re-checked at
/// every step.
fn merge_tree(
    src: &Path,
    dst: &Path,
    root: &Path,
    acc_id: &str,
    deadline: Instant,
    moved: &mut usize,
) -> Result<(), MergeErr> {
    if Instant::now() > deadline {
        return Err(MergeErr::Budget);
    }
    // The walk root must stay a REAL directory: if a concurrent heal (say,
    // after a broken lock) linked it meanwhile, paths would now resolve THROUGH
    // the junction into the shared store — and the dedup below would delete the
    // very files already merged there.
    if !is_dir(root) || is_link(root) {
        return Err(MergeErr::Io("source is no longer a real directory (concurrent heal?)".into()));
    }
    // Snapshot the listing BEFORE mutating the directory. Walking a live
    // `read_dir` while removing and renaming its entries is
    // implementation-defined — the platform may skip or repeat entries — which
    // would leave files behind for no visible reason and make every merge need a
    // second pass. Nothing was lost that way, but "it took two passes" is a bad
    // thing to have to explain.
    let entries: Vec<std::fs::DirEntry> =
        fs::read_dir(src).map_err(|e| MergeErr::Io(e.to_string()))?.collect::<Result<_, _>>().map_err(|e| MergeErr::Io(e.to_string()))?;
    for entry in entries {
        if Instant::now() > deadline {
            return Err(MergeErr::Budget);
        }
        let p = entry.path();
        let name = entry.file_name();
        let to = dst.join(&name);
        let meta = fs::symlink_metadata(&p).map_err(|e| MergeErr::Io(e.to_string()))?;

        if meta.file_type().is_symlink() || is_link(&p) {
            // A link inside the drifted dir: relocate it as-is (rename moves the
            // link itself, never follows it). Content behind it is not ours.
            fs::rename(&p, unique_dest(&to, acc_id)).map_err(|e| MergeErr::Io(format!("{}: {e}", name.to_string_lossy())))?;
            *moved += 1;
            continue;
        }
        if meta.is_dir() {
            fs::create_dir_all(&to).map_err(|e| MergeErr::Io(e.to_string()))?;
            merge_tree(&p, &to, root, acc_id, deadline, moved)?;
            continue;
        }
        // A regular file.
        let mut to = to;
        if file_exists(&to) {
            let before = fs::metadata(&p).map_err(|e| MergeErr::Io(e.to_string()))?;
            if same_content(&p, &to).unwrap_or(false) && unchanged_since(&p, &before) {
                // The re-stat guards the compare-then-remove window: a live
                // claude session appending between the two would otherwise lose
                // everything written after the content snapshot.
                fs::remove_file(&p).map_err(|e| MergeErr::Io(format!("{}: {e}", name.to_string_lossy())))?;
                *moved += 1;
                continue;
            }
            to = unique_dest(&to, acc_id);
        }
        move_file(&p, &to).map_err(|e| MergeErr::Io(format!("{}: {e}", name.to_string_lossy())))?;
        *moved += 1;
    }
    Ok(())
}

/// Whether the file still has the size and mtime it had at `before` — the guard
/// against discarding a file a live writer touched between a content snapshot
/// and its removal.
fn unchanged_since(p: &Path, before: &fs::Metadata) -> bool {
    match fs::metadata(p) {
        Ok(cur) => cur.len() == before.len() && cur.modified().ok() == before.modified().ok(),
        Err(_) => false,
    }
}

/// A collision-free variant of `path`: "plan.md" becomes "plan.from-work2.md",
/// then "plan.from-work2-2.md" and so on.
fn unique_dest(path: &Path, acc_id: &str) -> PathBuf {
    if !file_exists(path) && !is_dir(path) {
        return path.to_path_buf();
    }
    let ext = path.extension().map(|e| format!(".{}", e.to_string_lossy())).unwrap_or_default();
    let stem = {
        let s = path.to_string_lossy().to_string();
        s.strip_suffix(&ext).map(|s| s.to_string()).unwrap_or(s)
    };
    for i in 0.. {
        let c = if i == 0 {
            format!("{stem}.from-{acc_id}{ext}")
        } else {
            format!("{stem}.from-{acc_id}-{}{ext}", i + 1)
        };
        let c = PathBuf::from(c);
        if !file_exists(&c) && !is_dir(&c) {
            return c;
        }
    }
    unreachable!()
}

/// Bounds the byte-compare; anything bigger is treated as different (a conflict
/// copy is safe, an unbounded read is not).
const SAME_CONTENT_MAX: u64 = 8 << 20;

fn same_content(a: &Path, b: &Path) -> std::io::Result<bool> {
    let (fa, fb) = (fs::metadata(a)?, fs::metadata(b)?);
    if fa.len() != fb.len() || fa.len() > SAME_CONTENT_MAX {
        return Ok(false);
    }
    Ok(fs::read(a)? == fs::read(b)?)
}

/// Move `src` to `dst` without overwriting an existing `dst` or ever leaving a
/// truncated `dst` behind. Same volume: a hard link claims the destination name
/// EXCLUSIVELY (AlreadyExists if dst appeared since the caller's check, where a
/// bare rename would silently replace it) and content follows the inode — a
/// live writer's handle keeps landing in the moved file; rename is the fallback
/// for filesystems without hard links. Cross-volume: copy through a same-dir
/// temp file renamed into place only after a complete write (an interrupted
/// copy must not install truncated bytes under the canonical name — the next
/// pass would demote the intact original to a conflict copy), and `src` is
/// removed only if it provably did not change during the copy.
fn move_file(src: &Path, dst: &Path) -> std::io::Result<()> {
    match fs::hard_link(src, dst) {
        Ok(()) => return fs::remove_file(src),
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => return Err(e), // never overwrite
        Err(_) => {}
    }
    // `fs::rename` OVERWRITES an existing file. The hard link above normally
    // catches that case exclusively, but it can fail for unrelated reasons (a
    // filesystem without hard links, a network share) while dst exists — and
    // then a bare rename would destroy the shared store's copy. "Never
    // overwrite" has to hold on every path, not just the common one.
    if file_exists(dst) || is_dir(dst) {
        return Err(std::io::Error::new(
            std::io::ErrorKind::AlreadyExists,
            "destination exists; refusing to overwrite",
        ));
    }
    if fs::rename(src, dst).is_ok() {
        return Ok(());
    }
    let before = fs::metadata(src)?;
    let bytes = fs::read(src)?;
    let tmp = dst
        .parent()
        .unwrap_or(Path::new("."))
        .join(format!(".heal-tmp-{}-{}", std::process::id(), before.len()));
    fs::write(&tmp, &bytes)?;
    if !unchanged_since(src, &before) {
        let _ = fs::remove_file(&tmp);
        return Err(std::io::Error::other("changed during copy (live writer?); left for the next pass"));
    }
    if file_exists(dst) || is_dir(dst) {
        let _ = fs::remove_file(&tmp);
        return Err(std::io::Error::other("destination appeared during copy"));
    }
    if let Err(e) = fs::rename(&tmp, dst) {
        let _ = fs::remove_file(&tmp);
        return Err(e);
    }
    fs::remove_file(src)
}

/// Remove `root` and its subdirectories bottom-up with `remove_dir`, which
/// refuses non-empty dirs — exactly the safety wanted after a merge: if
/// anything reappeared meanwhile, the removal fails and the dir stays.
fn remove_empty_tree(root: &Path) -> std::io::Result<()> {
    let mut dirs = Vec::new();
    collect_dirs(root, &mut dirs)?;
    // Deepest first.
    dirs.sort_by_key(|p| std::cmp::Reverse(p.components().count()));
    for d in dirs {
        fs::remove_dir(&d)?;
    }
    Ok(())
}

fn collect_dirs(root: &Path, out: &mut Vec<PathBuf>) -> std::io::Result<()> {
    out.push(root.to_path_buf());
    for entry in fs::read_dir(root)? {
        let entry = entry?;
        let p = entry.path();
        let meta = fs::symlink_metadata(&p)?;
        if !meta.is_dir() || meta.file_type().is_symlink() {
            return Err(std::io::Error::other(format!("unexpected file {}", p.display())));
        }
        collect_dirs(&p, out)?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A temp dir that survives the test body (leaked on purpose, like the
    /// other suites here) plus a shared dir override.
    fn scratch() -> PathBuf {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().to_path_buf();
        std::mem::forget(tmp);
        p
    }

    #[test]
    fn classify_reads_every_state() {
        let root = scratch();
        let target = root.join("shared-projects");
        fs::create_dir_all(&target).unwrap();

        // Missing.
        let missing = root.join("nothing");
        assert_eq!(classify(&missing, &target), LinkState::Missing);

        // A regular file in the way.
        let f = root.join("afile");
        fs::write(&f, b"x").unwrap();
        assert_eq!(classify(&f, &target), LinkState::File);

        // A real empty dir, then the same dir with data.
        let real = root.join("real");
        fs::create_dir_all(&real).unwrap();
        assert_eq!(classify(&real, &target), LinkState::RealEmpty);
        fs::write(real.join("plan.md"), b"data").unwrap();
        assert_eq!(classify(&real, &target), LinkState::RealData);

        // A correct link, and one pointing elsewhere.
        let link = root.join("link");
        if make_link(&link, &target).is_ok() {
            assert_eq!(classify(&link, &target), LinkState::Ok, "junction to target is Ok");
            let other = root.join("other");
            fs::create_dir_all(&other).unwrap();
            assert_eq!(classify(&link, &other), LinkState::Wrong, "junction to another dir is Wrong");
        }
    }

    #[test]
    fn merge_dedups_identical_and_conflicts_differing() {
        let root = scratch();
        let (src, dst) = (root.join("drifted"), root.join("shared"));
        fs::create_dir_all(src.join("sub")).unwrap();
        fs::create_dir_all(&dst).unwrap();

        // same: identical content already in shared → deduplicated (removed).
        fs::write(src.join("same.md"), b"same").unwrap();
        fs::write(dst.join("same.md"), b"same").unwrap();
        // diff: different content → kept under a .from-<acct> name.
        fs::write(src.join("diff.md"), b"mine").unwrap();
        fs::write(dst.join("diff.md"), b"theirs").unwrap();
        // fresh: not in shared at all → moved across, nested path preserved.
        fs::write(src.join("sub").join("fresh.md"), b"new").unwrap();

        let mut moved = 0;
        let deadline = Instant::now() + Duration::from_secs(30);
        assert!(merge_tree(&src, &dst, &src, "work2", deadline, &mut moved).is_ok());
        assert_eq!(moved, 3);

        // Nothing in shared was overwritten.
        assert_eq!(fs::read(dst.join("same.md")).unwrap(), b"same");
        assert_eq!(fs::read(dst.join("diff.md")).unwrap(), b"theirs");
        assert_eq!(fs::read(dst.join("diff.from-work2.md")).unwrap(), b"mine");
        assert_eq!(fs::read(dst.join("sub").join("fresh.md")).unwrap(), b"new");
        // The drifted dir is drained (only empty dirs remain) and removable.
        assert!(!src.join("same.md").exists());
        assert!(!src.join("diff.md").exists());
        assert!(remove_empty_tree(&src).is_ok());
        assert!(!src.exists());
    }

    #[test]
    fn move_file_never_overwrites_an_existing_destination() {
        let root = scratch();
        let (a, b) = (root.join("a"), root.join("b"));
        fs::write(&a, b"aaa").unwrap();
        fs::write(&b, b"bbb").unwrap();
        assert!(move_file(&a, &b).is_err(), "must refuse to clobber");
        assert_eq!(fs::read(&b).unwrap(), b"bbb");
        assert!(a.exists(), "source stays put on refusal");
    }

    #[test]
    fn unique_dest_walks_up_the_suffixes() {
        let root = scratch();
        let p = root.join("plan.md");
        // Free name → returned as-is.
        assert_eq!(unique_dest(&p, "w2"), p);
        fs::write(&p, b"x").unwrap();
        let first = unique_dest(&p, "w2");
        assert!(first.ends_with("plan.from-w2.md"));
        fs::write(&first, b"x").unwrap();
        assert!(unique_dest(&p, "w2").ends_with("plan.from-w2-2.md"));
    }

    #[test]
    fn heal_is_a_noop_for_accounts_with_no_config_dir() {
        // heal ensures the shared store exists, so the test must point that
        // somewhere disposable — otherwise it would create directories in the
        // developer's real ~/.claude-shared.
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let tmp = scratch();
        std::env::set_var("HOUSTON_SHARED_DIR", tmp.join("shared"));

        // Best-effort contract: never panics, nothing to report.
        let res = heal(&[Account::default()]);
        assert!(res.quiet());
        assert!(res.relinked.is_empty());
        // The store it ensured is the temp one, and it really is there.
        assert!(tmp.join("shared").join("projects").is_dir());

        std::env::remove_var("HOUSTON_SHARED_DIR");
    }

    #[test]
    fn a_link_to_a_vanished_target_is_repaired_not_reported_as_fine() {
        // A junction can point at exactly the right path while that path no
        // longer exists. classify() reads that as Ok (the raw target matches),
        // so heal used to leave it alone — and every read through it failed.
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let tmp = scratch();
        let shared = tmp.join("shared");
        std::env::set_var("HOUSTON_SHARED_DIR", &shared);

        let cd = tmp.join("account-x");
        fs::create_dir_all(&cd).unwrap();
        let acc = Account { id: "x".into(), config_dir: cd.to_string_lossy().into_owned(), ..Default::default() };

        // First pass builds the store and the links.
        let res = heal(std::slice::from_ref(&acc));
        assert!(res.skipped.is_empty(), "unexpected: {:?}", res.skipped);
        let link = cd.join("projects");
        assert_eq!(classify(&link, &shared.join("projects")), LinkState::Ok);

        // Now the shared target disappears while the link survives.
        fs::remove_dir_all(shared.join("projects")).unwrap();
        assert!(!shared.join("projects").is_dir());

        // The next pass must put the target back, so reads through the link work.
        heal(std::slice::from_ref(&acc));
        assert!(shared.join("projects").is_dir(), "the vanished target was not restored");
        assert!(fs::read_dir(&link).is_ok(), "the link resolves again");

        std::env::remove_var("HOUSTON_SHARED_DIR");
    }

    #[test]
    fn remove_empty_tree_refuses_when_a_file_reappears() {
        let root = scratch();
        let d = root.join("d/sub");
        fs::create_dir_all(&d).unwrap();
        fs::write(d.join("late.md"), b"appeared mid-heal").unwrap();
        assert!(remove_empty_tree(&root.join("d")).is_err());
        assert!(d.join("late.md").exists(), "nothing is lost");
    }
}
