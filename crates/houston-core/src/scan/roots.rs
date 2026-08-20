//! Multi-root discovery and the deduped full scan.
//!
//! Houston stores conversations across a few well-known roots: the active
//! Claude config dir, the global ~/.claude store, the shared store and each
//! per-account config dir. Missions are deduped by their LOGICAL key
//! (project + id), not by resolved path: on Windows, junction traversal makes
//! path canonicalization unreliable, while the same physical file always has
//! the same project+id under every root.

use super::cache::Cache;
use super::parse_file;
use crate::model::Mission;
use crate::paths::home;
use std::collections::HashSet;
use std::fs;
use std::path::{Path, PathBuf};

/// Every projects directory worth scanning. Non-existent ones are dropped.
pub fn project_roots() -> Vec<PathBuf> {
    let mut roots = Vec::new();
    let mut add = |p: PathBuf| {
        if p.is_dir() {
            roots.push(p);
        }
    };
    if let Some(cd) = std::env::var_os("CLAUDE_CONFIG_DIR") {
        if !cd.is_empty() {
            add(PathBuf::from(cd).join("projects"));
        }
    }
    let h = home();
    add(h.join(".claude").join("projects"));
    add(h.join(".claude-shared").join("projects"));
    if let Ok(entries) = fs::read_dir(h.join(".claude-accounts")) {
        let mut names: Vec<String> = entries
            .flatten()
            .filter(|e| e.path().is_dir())
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|n| n.starts_with("account-"))
            .collect();
        names.sort();
        for n in names {
            add(h.join(".claude-accounts").join(n).join("projects"));
        }
    }
    roots
}

/// Scan one root, reusing cached parses for unchanged transcripts when a
/// cache is given. Subagent transcripts are excluded. Sorted most-recent first.
pub fn scan_root(root: &Path, cache: Option<&mut Cache>) -> Vec<Mission> {
    let mut missions = Vec::new();
    let mut cache = cache;
    walk(root, &mut |path| {
        let meta = fs::metadata(path).ok();
        if let (Some(c), Some(fi)) = (cache.as_deref_mut(), meta.as_ref()) {
            if let Some(m) = c.lookup(path, fi) {
                missions.push(m);
                return;
            }
        }
        if let Some(m) = parse_file(path) {
            if !m.id.is_empty() {
                if let (Some(c), Some(fi)) = (cache.as_deref_mut(), meta.as_ref()) {
                    c.put(path, fi, &m);
                }
                missions.push(m);
            }
        }
    });
    missions.sort_by(|a, b| b.last_time.cmp(&a.last_time));
    missions
}

/// Depth-first walk collecting .jsonl files, skipping `subagents` dirs and
/// unreadable entries (keep going, never fail the scan).
fn walk(dir: &Path, f: &mut impl FnMut(&Path)) {
    let Ok(entries) = fs::read_dir(dir) else {
        return;
    };
    for e in entries.flatten() {
        let p = e.path();
        let Ok(ft) = e.file_type() else { continue };
        if ft.is_dir() {
            if e.file_name() == "subagents" {
                continue;
            }
            walk(&p, f);
        } else if p.extension().is_some_and(|x| x == "jsonl") {
            f(&p);
        }
    }
}

/// Scan every project root and dedupe missions reached through different
/// roots (the shared store plus each account dir junctioned to it).
pub fn scan_all() -> Vec<Mission> {
    let mut roots = project_roots();
    if roots.is_empty() {
        roots = vec![home().join(".claude").join("projects")];
    }
    let mut cache = Cache::load();
    let mut seen = HashSet::new();
    let mut all = Vec::new();
    for r in &roots {
        for m in scan_root(r, Some(&mut cache)) {
            if seen.insert(m.key()) {
                all.push(m);
            }
        }
    }
    cache.save();
    all.sort_by(|a, b| b.last_time.cmp(&a.last_time));
    all
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    #[test]
    fn scan_root_finds_sorts_and_skips_subagents() {
        let tmp = tempfile::tempdir().unwrap();
        let mk = |proj: &str, id: &str, ts: &str| {
            let d = tmp.path().join(proj);
            fs::create_dir_all(&d).unwrap();
            let mut f = fs::File::create(d.join(format!("{id}.jsonl"))).unwrap();
            writeln!(
                f,
                r#"{{"type":"user","timestamp":"{ts}","message":{{"content":"hey"}}}}"#
            )
            .unwrap();
        };
        mk("C--a", "old", "2026-01-01T00:00:00Z");
        mk("C--a", "new", "2026-06-01T00:00:00Z");
        // A subagent transcript must NOT become a mission.
        let sub = tmp.path().join("C--a").join("new").join("subagents");
        fs::create_dir_all(&sub).unwrap();
        fs::write(sub.join("agent.jsonl"), "{}\n").unwrap();

        let ms = scan_root(tmp.path(), None);
        assert_eq!(ms.len(), 2);
        assert_eq!(ms[0].id, "new"); // most recent first
        assert_eq!(ms[1].id, "old");
    }
}
