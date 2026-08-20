//! The scan cache: remembers the parsed Mission of every transcript keyed by
//! path and validated by (size, mtime), so TUI launches and rescans only
//! re-read files that actually changed. A pure accelerator: any load/save
//! problem simply means a full re-parse. Same on-disk SHAPE as v1 but its own
//! FILE (`<store>/scan-cache-v2.json`): while v1 and v2 coexist on the same
//! machine, neither may clobber the other's warm cache.

use crate::model::Mission;
use crate::paths::store_dir;
use serde::{Deserialize, Serialize};
use std::collections::{HashMap, HashSet};
use std::fs;
use std::path::{Path, PathBuf};

#[derive(Serialize, Deserialize, Clone)]
struct Entry {
    size: u64,
    #[serde(rename = "mtimeNs")]
    mtime_ns: i64,
    mission: Mission,
}

#[derive(Serialize, Deserialize, Default)]
struct OnDisk {
    entries: HashMap<String, Entry>,
}

pub struct Cache {
    path: PathBuf,
    entries: HashMap<String, Entry>,
    /// Paths seen this run; save() prunes the rest.
    used: HashSet<String>,
    dirty: bool,
}

impl Cache {
    /// Read the scan cache from Houston's data dir (empty if missing/unreadable).
    pub fn load() -> Self {
        Self::load_from(store_dir().join("scan-cache-v2.json"))
    }

    pub fn load_from(path: PathBuf) -> Self {
        let entries = fs::read(&path)
            .ok()
            .and_then(|b| serde_json::from_slice::<OnDisk>(&b).ok())
            .map(|d| d.entries)
            .unwrap_or_default();
        Cache { path, entries, used: HashSet::new(), dirty: false }
    }

    /// The cached mission if the file is unchanged.
    pub fn lookup(&mut self, path: &Path, fi: &fs::Metadata) -> Option<Mission> {
        let key = path.to_string_lossy().into_owned();
        let e = self.entries.get(&key)?;
        if e.size != fi.len() || e.mtime_ns != super::mtime_ns(fi) {
            return None;
        }
        self.used.insert(key);
        Some(e.mission.clone())
    }

    pub fn put(&mut self, path: &Path, fi: &fs::Metadata, m: &Mission) {
        let key = path.to_string_lossy().into_owned();
        self.entries.insert(
            key.clone(),
            Entry { size: fi.len(), mtime_ns: super::mtime_ns(fi), mission: m.clone() },
        );
        self.used.insert(key);
        self.dirty = true;
    }

    /// Persist, pruning entries whose files were not seen this run. Best-effort.
    pub fn save(&mut self) {
        let pruned = self.entries.len() != self.used.len();
        if !self.dirty && !pruned {
            return;
        }
        self.entries.retain(|k, _| self.used.contains(k));
        let disk = OnDisk { entries: std::mem::take(&mut self.entries) };
        if let Some(dir) = self.path.parent() {
            let _ = fs::create_dir_all(dir);
        }
        if let Ok(b) = serde_json::to_vec(&disk) {
            // Uniquely-named temp: a fixed ".tmp" lets two processes writing at
            // once (two TUIs, or a TUI and `houston scan`) interleave into the
            // same file and rename half a document into place. The cache is only
            // an accelerator, but a corrupt one costs a full re-parse of every
            // transcript on the next launch.
            let tmp = self.path.with_extension(format!("json.{}.tmp", std::process::id()));
            if fs::write(&tmp, b).is_ok() {
                if fs::rename(&tmp, &self.path).is_err() {
                    let _ = fs::remove_file(&tmp);
                }
            } else {
                let _ = fs::remove_file(&tmp);
            }
        }
        self.entries = disk.entries;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    #[test]
    fn cache_roundtrip_hit_and_invalidation() {
        let tmp = tempfile::tempdir().unwrap();
        let file = tmp.path().join("s.jsonl");
        let mut f = fs::File::create(&file).unwrap();
        writeln!(f, r#"{{"type":"user","message":{{"content":"hi"}}}}"#).unwrap();
        drop(f);

        let cache_path = tmp.path().join("cache.json");
        let mut c = Cache::load_from(cache_path.clone());
        let fi = fs::metadata(&file).unwrap();
        assert!(c.lookup(&file, &fi).is_none(), "cold cache must miss");

        let m = crate::scan::parse_file(&file).unwrap();
        c.put(&file, &fi, &m);
        assert!(c.lookup(&file, &fi).is_some(), "hot cache must hit");
        c.save();

        // Reload: still hits for the unchanged file.
        let mut c2 = Cache::load_from(cache_path.clone());
        assert!(c2.lookup(&file, &fi).is_some(), "persisted cache must hit");

        // Grow the file: size changes → miss.
        let mut f = fs::OpenOptions::new().append(true).open(&file).unwrap();
        writeln!(f, r#"{{"type":"user","message":{{"content":"more"}}}}"#).unwrap();
        drop(f);
        let fi2 = fs::metadata(&file).unwrap();
        let mut c3 = Cache::load_from(cache_path);
        assert!(c3.lookup(&file, &fi2).is_none(), "changed file must miss");
    }

    #[test]
    fn save_prunes_unseen_entries() {
        let tmp = tempfile::tempdir().unwrap();
        let file = tmp.path().join("s.jsonl");
        fs::write(&file, "{}\n").unwrap();
        let cache_path = tmp.path().join("cache.json");

        let mut c = Cache::load_from(cache_path.clone());
        let fi = fs::metadata(&file).unwrap();
        c.put(&file, &fi, &Mission::default());
        c.save();

        // Next run never touches the file → save() drops it.
        let mut c2 = Cache::load_from(cache_path.clone());
        c2.dirty = true;
        c2.save();
        let c3 = Cache::load_from(cache_path);
        assert!(c3.entries.is_empty(), "unseen entries must be pruned");
    }
}
