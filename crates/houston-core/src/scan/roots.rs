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

/// Every projects directory worth scanning. Non-existent ones are dropped, and
/// so are repeats — an account reached both by its registry entry and by the
/// conventional sweep is one root.
///
/// Every location comes from the accessor that OWNS it rather than from a
/// literal path. That is the whole change here: this function used to spell
/// `~/.claude-shared` and `~/.claude-accounts` itself, so `$HOUSTON_SHARED_DIR`
/// and `$HOUSTON_ACCOUNTS_DIR` moved the store while the scan kept reading the
/// old place, and an account with an explicit `configDir` was invisible to the
/// scan no matter what. Two halves of Houston disagreeing about where the
/// conversations live is not a bug that announces itself — it just shows fewer
/// missions than exist.
pub fn project_roots() -> Vec<PathBuf> {
    let mut roots: Vec<PathBuf> = Vec::new();
    if let Some(cd) = std::env::var_os("CLAUDE_CONFIG_DIR") {
        if !cd.is_empty() {
            push_root(&mut roots, PathBuf::from(cd).join("projects"));
        }
    }
    push_root(&mut roots, home().join(".claude").join("projects"));
    push_root(&mut roots, crate::heal::shared_dir().join("projects"));
    // Registered accounts by their RESOLVED config dir, so an explicit
    // `configDir` is followed wherever it points.
    for a in crate::accounts::load().unwrap_or_default() {
        if let Some(d) = a.resolve_config_dir() {
            push_root(&mut roots, d.join("projects"));
        }
    }
    // Then the conventional sweep, which still earns its keep: a dir left by a
    // removed account, or a half-finished provision, holds conversations that
    // no registry entry points at any more.
    if let Ok(entries) = fs::read_dir(crate::accounts::accounts_dir()) {
        let mut dirs: Vec<PathBuf> = entries
            .flatten()
            .filter(|e| e.path().is_dir())
            .filter(|e| e.file_name().to_string_lossy().starts_with("account-"))
            .map(|e| e.path())
            .collect();
        dirs.sort();
        for d in dirs {
            push_root(&mut roots, d.join("projects"));
        }
    }
    roots
}

/// Add a root if it exists and is not already listed.
fn push_root(roots: &mut Vec<PathBuf>, p: PathBuf) {
    if p.is_dir() && !roots.contains(&p) {
        roots.push(p);
    }
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
    use crate::accounts::Account;
    use std::io::Write;

    /// The roots have to come from the accessors that own each location, or a
    /// relocated store is simply invisible: `$HOUSTON_SHARED_DIR` and
    /// `$HOUSTON_ACCOUNTS_DIR` moved the store while this function kept reading
    /// the literal `~/.claude-shared` and `~/.claude-accounts`, and an account
    /// with an explicit `configDir` was never scanned at all.
    #[test]
    fn project_roots_follow_the_overrides_and_an_explicit_config_dir() {
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        let mk = |p: &Path| {
            fs::create_dir_all(p.join("projects")).unwrap();
            p.join("projects")
        };
        let shared = mk(&tmp.path().join("shared"));
        let accs_dir = tmp.path().join("accs");
        let conventional = mk(&accs_dir.join("account-conv"));
        // An account whose configDir points somewhere the sweep cannot reach.
        let elsewhere = mk(&tmp.path().join("way-over-here"));

        unsafe {
            std::env::set_var("HOUSTON_HOME", tmp.path());
            std::env::set_var("HOUSTON_SHARED_DIR", tmp.path().join("shared"));
            std::env::set_var("HOUSTON_ACCOUNTS_DIR", &accs_dir);
            std::env::remove_var("CLAUDE_CONFIG_DIR");
        }
        crate::accounts::save(&[
            Account { id: "conv".into(), ..Default::default() },
            Account {
                id: "odd".into(),
                config_dir: tmp.path().join("way-over-here").to_string_lossy().into_owned(),
                ..Default::default()
            },
        ])
        .unwrap();

        let roots = project_roots();
        assert!(roots.contains(&shared), "the relocated shared store is not scanned: {roots:?}");
        assert!(roots.contains(&elsewhere), "an explicit configDir is not scanned: {roots:?}");
        assert!(roots.contains(&conventional), "the conventional sweep still applies: {roots:?}");
        // The conventional account is reachable twice (registry + sweep) and
        // must appear once, or every mission in it gets parsed twice.
        assert_eq!(roots.iter().filter(|r| *r == &conventional).count(), 1, "{roots:?}");

        unsafe {
            std::env::remove_var("HOUSTON_SHARED_DIR");
            std::env::remove_var("HOUSTON_ACCOUNTS_DIR");
            std::env::remove_var("HOUSTON_HOME");
        }
    }

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
