//! Houston's mutable state: per-mission metadata (tags, notes, pin, archive,
//! cwd override) and the user's Programs. Missions themselves are scanned
//! live and never persisted here. Meta lives in `store.json`; each Program is
//! its own `.prog` manifest under `programs/`. Same files and shapes as v1 —
//! v2 operates on the same data.

use crate::atomic::write as write_atomic;
use crate::flock;
use crate::model::{Meta, Program};
use crate::paths::store_dir;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::time::Duration;

/// How long a mutation waits for another process editing the same file. These
/// are read-modify-write cycles over small JSON, so contention is brief.
const LOCK_WAIT: Duration = Duration::from_secs(3);

#[derive(Serialize, Deserialize, Default)]
struct OnDisk {
    meta: HashMap<String, Meta>,
}

pub struct Store {
    dir: PathBuf,
    /// Keyed by mission key (project/id).
    pub meta: HashMap<String, Meta>,
    /// Sorted by name.
    pub programs: Vec<Program>,
}

impl Store {
    /// Read meta + programs from the default data dir.
    pub fn load() -> io::Result<Self> {
        Self::load_from(store_dir())
    }

    /// Read meta + programs from `dir`, creating it if needed. Exposed so
    /// tests can use a temp dir instead of the real ~/.claude/houston.
    pub fn load_from(dir: PathBuf) -> io::Result<Self> {
        fs::create_dir_all(dir.join("programs"))?;
        let meta = Self::read_meta_from_disk(&dir.join("store.json"));
        let mut programs = Vec::new();
        if let Ok(entries) = fs::read_dir(dir.join("programs")) {
            for e in entries.flatten() {
                let p = e.path();
                if !p.is_file() || p.extension().is_none_or(|x| x != "prog") {
                    continue;
                }
                if let Some(prog) = fs::read(&p)
                    .ok()
                    .and_then(|b| serde_json::from_slice::<Program>(&b).ok())
                {
                    if !prog.name.is_empty() {
                        programs.push(prog);
                    }
                }
            }
        }
        programs.sort_by(|a, b| a.name.cmp(&b.name));
        Ok(Store { dir, meta, programs })
    }

    fn meta_path(&self) -> PathBuf {
        self.dir.join("store.json")
    }

    /// Read the meta map straight from disk, ignoring our in-memory copy.
    fn read_meta_from_disk(path: &Path) -> HashMap<String, Meta> {
        fs::read(path)
            .ok()
            .and_then(|b| serde_json::from_slice::<OnDisk>(&b).ok())
            .map(|d| d.meta)
            .unwrap_or_default()
    }

    fn save_program(&self, p: &Program) -> io::Result<()> {
        let path = prog_file(&self.dir, &p.name);
        let _lk = lock_for(&path)?;
        write_atomic(&path, serde_json::to_vec_pretty(p)?.as_slice())
    }

    // --- meta mutations ---

    pub fn meta_of(&self, key: &str) -> Meta {
        self.meta.get(key).cloned().unwrap_or_default()
    }

    /// Apply `f` to a mission's metadata and persist it.
    ///
    /// The whole map is rewritten on every edit, so this re-reads UNDER A LOCK
    /// and applies `f` to the value that is actually on disk. Two Houston
    /// windows are normal, and without this, pinning in one and tagging in the
    /// other would each write its own stale snapshot and silently drop the
    /// other's change — and a TOGGLE computed from a stale value could even flip
    /// to the state the other window just set. Re-reading also means an edit
    /// picks up the other window's work, so the two converge.
    ///
    /// An all-default meta is pruned rather than stored.
    fn mutate_meta(&mut self, key: &str, f: impl FnOnce(&mut Meta)) -> io::Result<()> {
        let path = self.meta_path();
        let _lk = lock_for(&path)?;
        let mut fresh = Self::read_meta_from_disk(&path);

        let mut m = fresh.get(key).cloned().unwrap_or_default();
        f(&mut m);
        // Every field must be listed here. A field left out of this check is a
        // field that gets DELETED the next time any other field is edited —
        // silently, and only for the missions that used it.
        let empty = m.tags.is_empty()
            && m.note.is_empty()
            && !m.pinned
            && !m.archived
            && m.cwd_override.is_empty()
            && m.launch.is_empty();
        if empty {
            fresh.remove(key);
        } else {
            fresh.insert(key.to_string(), m);
        }

        write_atomic(&path, serde_json::to_vec_pretty(&OnDisk { meta: fresh.clone() })?.as_slice())?;
        self.meta = fresh;
        Ok(())
    }

    pub fn toggle_pin(&mut self, key: &str) -> io::Result<()> {
        self.mutate_meta(key, |m| m.pinned = !m.pinned)
    }

    pub fn toggle_archive(&mut self, key: &str) -> io::Result<()> {
        self.mutate_meta(key, |m| m.archived = !m.archived)
    }

    pub fn set_note(&mut self, key: &str, note: &str) -> io::Result<()> {
        let note = note.trim().to_string();
        self.mutate_meta(key, |m| m.note = note)
    }

    /// Re-point a mission's working directory (empty clears). When a project
    /// folder moves, this override keeps resume and every cwd probe pointing
    /// somewhere real.
    pub fn set_cwd_override(&mut self, key: &str, cwd: &str) -> io::Result<()> {
        let cwd = cwd.trim().to_string();
        self.mutate_meta(key, |m| m.cwd_override = cwd)
    }

    /// This mission's launch preferences (all-default when it has none).
    pub fn launch_of(&self, key: &str) -> crate::model::Launch {
        self.meta_of(key).launch
    }

    /// Replace a mission's launch preferences. An all-default `Launch` clears
    /// them, and prunes the metadata entry if nothing else is set.
    pub fn set_launch(&mut self, key: &str, l: crate::model::Launch) -> io::Result<()> {
        self.mutate_meta(key, |m| m.launch = l)
    }

    pub fn add_tag(&mut self, key: &str, tag: &str) -> io::Result<()> {
        let tag = tag.trim().to_string();
        if tag.is_empty() {
            return Ok(());
        }
        self.mutate_meta(key, |m| {
            if !m.tags.iter().any(|t| t.eq_ignore_ascii_case(&tag)) {
                m.tags.push(tag);
            }
        })
    }

    pub fn remove_tag(&mut self, key: &str, tag: &str) -> io::Result<()> {
        let tag = tag.to_string();
        self.mutate_meta(key, |m| m.tags.retain(|t| !t.eq_ignore_ascii_case(&tag)))
    }

    // --- program mutations ---

    /// By the name shown, or failing that by the file it maps to — see
    /// `prog_key`. Exact first so an unambiguous name never loses to a
    /// case-folded neighbour.
    pub fn program_by_name(&self, name: &str) -> Option<&Program> {
        if let Some(p) = self.programs.iter().find(|p| p.name == name) {
            return Some(p);
        }
        let key = prog_key(name);
        self.programs.iter().find(|p| prog_key(&p.name) == key)
    }

    /// `program_by_name`'s identity rule, for the mutating paths.
    fn program_mut(&mut self, name: &str) -> Option<&mut Program> {
        let i = self
            .programs
            .iter()
            .position(|p| p.name == name)
            .or_else(|| {
                let key = prog_key(name);
                self.programs.iter().position(|p| prog_key(&p.name) == key)
            })?;
        self.programs.get_mut(i)
    }

    pub fn create_program(&mut self, name: &str, desc: &str) -> io::Result<()> {
        let name = name.trim();
        if name.is_empty() || self.program_by_name(name).is_some() {
            return Ok(());
        }
        let p = Program {
            name: name.to_string(),
            description: desc.to_string(),
            missions: Vec::new(),
        };
        self.save_program(&p)?;
        self.programs.push(p);
        self.programs.sort_by(|a, b| a.name.cmp(&b.name));
        Ok(())
    }

    pub fn add_to_program(&mut self, name: &str, mission_key: &str) -> io::Result<()> {
        let Some(p) = self.program_mut(name) else {
            return Ok(());
        };
        if p.missions.iter().any(|k| k == mission_key) {
            return Ok(());
        }
        p.missions.push(mission_key.to_string());
        let snapshot = p.clone();
        self.save_program(&snapshot)
    }

    pub fn remove_from_program(&mut self, name: &str, mission_key: &str) -> io::Result<()> {
        let Some(p) = self.program_mut(name) else {
            return Ok(());
        };
        p.missions.retain(|k| k != mission_key);
        let snapshot = p.clone();
        self.save_program(&snapshot)
    }
}

/// The advisory lock guarding read-modify-write on a store file.
fn lock_for(path: &Path) -> io::Result<flock::Lock> {
    let mut name = path.file_name().map(|n| n.to_os_string()).unwrap_or_default();
    name.push(".lock");
    flock::acquire(&path.with_file_name(name), LOCK_WAIT)
}

/// Device names Windows refuses as a file stem — with or without an extension
/// ("CON.prog" is as invalid as "CON").
const WINDOWS_RESERVED: [&str; 22] = [
    "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8",
    "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
];

/// The file stem a program's `.prog` gets, sanitized for Windows.
fn prog_stem(name: &str) -> String {
    let mut safe: String = name
        .chars()
        .map(|r| {
            if r#"<>:"/\|?*"#.contains(r) || (r as u32) < 32 {
                '_'
            } else {
                r
            }
        })
        .collect();
    // Windows silently drops trailing dots/spaces ("foo." collides with
    // "foo"), and refuses reserved device stems outright.
    safe = safe.trim_end_matches(['.', ' ']).to_string();
    let stem = safe.split('.').next().unwrap_or("").to_ascii_uppercase();
    if safe.is_empty() || WINDOWS_RESERVED.contains(&stem.as_str()) {
        safe = format!("_{safe}");
    }
    safe
}

/// A program's IDENTITY: the file it would land on, case-folded.
///
/// Not the displayed name. `prog_stem` maps many names onto one file — every
/// character Windows forbids collapses to `_`, trailing dots and spaces are
/// dropped — and NTFS is case-insensitive on top of that. So `Work` and `work`,
/// or `a/b` and `a:b`, are one `.prog`, and comparing display names let the
/// second program silently overwrite the first's file while both sat in the
/// list. Whatever decides "same program" has to be what decides "same file".
fn prog_key(name: &str) -> String {
    prog_stem(name).to_lowercase()
}

fn prog_file(dir: &Path, name: &str) -> PathBuf {
    dir.join("programs").join(format!("{}.prog", prog_stem(name)))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn temp_store() -> (tempfile::TempDir, Store) {
        let tmp = tempfile::tempdir().unwrap();
        let s = Store::load_from(tmp.path().to_path_buf()).unwrap();
        (tmp, s)
    }

    #[test]
    fn meta_roundtrip_and_prune() {
        let (tmp, mut s) = temp_store();
        s.toggle_pin("p/1").unwrap();
        s.add_tag("p/1", "wip").unwrap();
        s.add_tag("p/1", "WIP").unwrap(); // case-insensitive dedup
        assert_eq!(s.meta_of("p/1").tags, vec!["wip"]);

        // Reload from disk: state survives.
        let s2 = Store::load_from(tmp.path().to_path_buf()).unwrap();
        assert!(s2.meta_of("p/1").pinned);
        assert_eq!(s2.meta_of("p/1").tags, vec!["wip"]);

        // Undo everything: the entry must be PRUNED, not stored as defaults.
        let mut s3 = s2;
        s3.toggle_pin("p/1").unwrap();
        s3.remove_tag("p/1", "wip").unwrap();
        assert!(!s3.meta.contains_key("p/1"));
    }

    /// Launch preferences are metadata like any other: they survive a reload,
    /// they survive OTHER fields being edited, and clearing them prunes.
    #[test]
    fn launch_prefs_survive_reload_and_unrelated_edits() {
        use crate::model::Launch;
        let (tmp, mut s) = temp_store();
        let prefs = Launch { model: "sonnet".into(), worktree: Some(String::new()), ..Default::default() };
        s.set_launch("p/1", prefs.clone()).unwrap();

        let mut s2 = Store::load_from(tmp.path().to_path_buf()).unwrap();
        assert_eq!(s2.launch_of("p/1"), prefs, "prefs come back from disk");

        // The trap: a mission whose ONLY metadata is its prefs must not be
        // pruned when something else is toggled and untoggled.
        s2.toggle_pin("p/1").unwrap();
        s2.toggle_pin("p/1").unwrap();
        assert_eq!(s2.launch_of("p/1"), prefs, "an unrelated toggle did not delete the prefs");
        assert!(s2.meta.contains_key("p/1"));

        // Clearing them prunes the entry, since nothing else is set.
        s2.set_launch("p/1", Launch::default()).unwrap();
        assert!(!s2.meta.contains_key("p/1"));
    }

    /// v1 is the rollback path and reads the same `store.json`, so a mission
    /// without preferences must serialize exactly as it did before the field
    /// existed.
    #[test]
    fn a_mission_without_prefs_writes_no_launch_key() {
        let (tmp, mut s) = temp_store();
        s.add_tag("p/1", "wip").unwrap();
        let raw = std::fs::read_to_string(tmp.path().join("store.json")).unwrap();
        assert!(!raw.contains("launch"), "an unused field must not appear in the file: {raw}");
        assert!(raw.contains("wip"));
    }

    #[test]
    fn programs_roundtrip_ordered_and_deduped() {
        let (tmp, mut s) = temp_store();
        s.create_program("zeta", "").unwrap();
        s.create_program("alpha", "first").unwrap();
        s.create_program("alpha", "dup ignored").unwrap();
        assert_eq!(s.programs.len(), 2);
        assert_eq!(s.programs[0].name, "alpha"); // sorted

        s.add_to_program("alpha", "p/1").unwrap();
        s.add_to_program("alpha", "p/1").unwrap(); // dedup
        s.add_to_program("alpha", "p/2").unwrap();
        s.remove_from_program("alpha", "p/1").unwrap();

        let s2 = Store::load_from(tmp.path().to_path_buf()).unwrap();
        let a = s2.program_by_name("alpha").unwrap();
        assert_eq!(a.missions, vec!["p/2"]);
        assert_eq!(a.description, "first");
    }

    #[test]
    fn two_open_stores_do_not_lose_each_others_edits() {
        // Two Houston windows on the same store — the normal case. Each holds
        // its own in-memory map, and every write rewrites the WHOLE file, so
        // without a re-read under the lock the second write would erase the
        // first mission's metadata entirely.
        let tmp = tempfile::tempdir().unwrap();
        let mut a = Store::load_from(tmp.path().to_path_buf()).unwrap();
        let mut b = Store::load_from(tmp.path().to_path_buf()).unwrap();

        a.toggle_pin("p/1").unwrap();
        b.add_tag("p/2", "wip").unwrap(); // b's map never saw p/1

        let fresh = Store::load_from(tmp.path().to_path_buf()).unwrap();
        assert!(fresh.meta_of("p/1").pinned, "the first window's pin survived");
        assert_eq!(fresh.meta_of("p/2").tags, vec!["wip"], "the second window's tag survived");
        // The writer also converged on the other window's change.
        assert!(b.meta_of("p/1").pinned, "b picked up p/1 when it wrote");
    }

    #[test]
    fn a_toggle_acts_on_the_value_on_disk_not_a_stale_one() {
        // b pinned it already; a's toggle must UNpin (acting on the real state),
        // not re-pin from its own stale "not pinned" snapshot.
        let tmp = tempfile::tempdir().unwrap();
        let mut a = Store::load_from(tmp.path().to_path_buf()).unwrap();
        let mut b = Store::load_from(tmp.path().to_path_buf()).unwrap();

        b.toggle_pin("p/x").unwrap();
        assert!(b.meta_of("p/x").pinned);
        a.toggle_pin("p/x").unwrap();

        let fresh = Store::load_from(tmp.path().to_path_buf()).unwrap();
        assert!(!fresh.meta_of("p/x").pinned, "the toggle flipped the on-disk value");
    }

    /// Two programs whose names differ only in ways `prog_file` erases are ONE
    /// program, because they are one file. Comparing display names let the
    /// second overwrite the first's `.prog` while both sat in the list.
    #[test]
    fn programs_that_map_to_one_file_are_one_program() {
        let (tmp, mut s) = temp_store();
        s.create_program("Work", "the real one").unwrap();
        // Case only: NTFS gives these the same file.
        s.create_program("work", "would clobber it").unwrap();
        // And every forbidden character collapses to `_`, so do these.
        s.create_program("a/b", "first").unwrap();
        s.create_program("a:b", "same file as a/b").unwrap();

        assert_eq!(s.programs.len(), 2, "one program per file: {:?}", s.programs.iter().map(|p| &p.name).collect::<Vec<_>>());
        assert_eq!(s.program_by_name("Work").unwrap().description, "the real one");
        // The lookup follows the same identity rule, so the second spelling
        // reaches the program that owns the file rather than nothing.
        assert_eq!(s.program_by_name("work").unwrap().description, "the real one");

        // One file on disk per program. `.lock` files live here too (see
        // `lock_for`), so count only the documents.
        let mut files: Vec<String> = std::fs::read_dir(tmp.path().join("programs"))
            .unwrap()
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|n| n.ends_with(".prog"))
            .collect();
        files.sort();
        assert_eq!(files, vec!["Work.prog".to_string(), "a_b.prog".to_string()], "one .prog per program");

        // A mission added under the second spelling lands on the one program.
        s.add_to_program("work", "p/1").unwrap();
        let reloaded = Store::load_from(tmp.path().to_path_buf()).unwrap();
        assert_eq!(reloaded.program_by_name("Work").unwrap().missions, vec!["p/1".to_string()]);
    }

    #[test]
    fn prog_file_neutralizes_windows_hazards() {
        let d = Path::new("x");
        let f = |n: &str| {
            prog_file(d, n)
                .file_name()
                .unwrap()
                .to_string_lossy()
                .into_owned()
        };
        assert_eq!(f("a/b:c"), "a_b_c.prog"); // separators/colons
        assert_eq!(f("trailing. "), "trailing.prog"); // dropped dot/space
        assert_eq!(f("CON"), "_CON.prog"); // reserved stem
        assert_eq!(f("nul.sync"), "_nul.sync.prog"); // reserved stem w/ ext
        assert_eq!(f(""), "_.prog");
    }
}
