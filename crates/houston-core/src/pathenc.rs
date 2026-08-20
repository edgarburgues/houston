//! Claude's project-dir encoding. Claude stores a session at
//! `projects/<encoded-cwd>/<id>.jsonl`, where the encoded cwd replaces EVERY
//! non-alphanumeric character with '-' (verified against real stores: ':' '\'
//! '/' '.' and ' ' all encode to '-'; a UNC path like
//! `\\192.0.2.20\Media\South Park` becomes `--192-0-2-20-Media-South-Park`).
//! `claude --resume` re-derives that key from the CURRENT shell cwd, so to
//! resume we must cd to the path that encodes back to the session's project
//! dir — the cwd at session *creation*, not the cwd of the last message.

use std::fs;
use std::path::{Path, PathBuf};

/// Encode a real path into Claude's project-dir name: every char outside
/// [a-zA-Z0-9] becomes '-'.
pub fn encode(p: &str) -> String {
    p.chars()
        .map(|c| if c.is_ascii_alphanumeric() { c } else { '-' })
        .collect()
}

fn is_dir(p: &Path) -> bool {
    fs::metadata(p).map(|m| m.is_dir()).unwrap_or(false)
}

/// Reconstruct a real path from a project-dir name. The encoding is lossy — a
/// '-' may stand for a real '-', '.', ' ' or any other punctuation — so at
/// each level we list the parent directory and pick the child whose ENCODED
/// name matches the longest prefix of the remaining tokens. When nothing
/// matches, a best-effort single token keeps us moving. Returns None if it
/// can't get started.
///
/// Windows-only: it keys off the "--" a drive root ("C:\" -> "C--") leaves in
/// the encoded name. POSIX roots encode "/Users" -> "-Users" (no "--"), so
/// this returns None there and resolve_resume_dir falls back to the stored cwd.
pub fn decode_project_dir(proj: &str) -> Option<PathBuf> {
    let i = proj.find("--")?;
    if i == 0 {
        return None; // UNC "--..." or POSIX: can't reconstruct
    }
    let mut cur = PathBuf::from(format!("{}:\\", &proj[..i]));
    let mut rest = &proj[i + 2..];
    while !rest.is_empty() {
        match best_child(&cur, rest) {
            Some((name, n)) => {
                cur.push(name);
                rest = rest[n..].strip_prefix('-').unwrap_or(&rest[n..]);
            }
            None => {
                // Nothing on disk encodes to a prefix of rest (dir renamed or
                // removed): take one raw token and keep going, best effort.
                let (name, next) = match rest.find('-') {
                    Some(j) => (&rest[..j], &rest[j + 1..]),
                    None => (rest, ""),
                };
                cur.push(name);
                rest = next;
            }
        }
    }
    Some(cur)
}

/// The child directory of `cur` whose encoded name is the longest match for a
/// prefix of `rest` (the full rest, or a prefix followed by the '-'
/// separator), and how many bytes of rest its encoding consumes.
fn best_child(cur: &Path, rest: &str) -> Option<(String, usize)> {
    let entries = fs::read_dir(cur).ok()?;
    let mut best: Option<(String, usize)> = None;
    for e in entries.flatten() {
        if !e.file_type().map(|t| t.is_dir()).unwrap_or(false) {
            continue;
        }
        let name = e.file_name().to_string_lossy().into_owned();
        let enc = encode(&name);
        if enc.is_empty() || best.as_ref().is_some_and(|(_, l)| enc.len() <= *l) {
            continue;
        }
        let matches = rest == enc
            || (rest.starts_with(&enc) && rest.as_bytes().get(enc.len()) == Some(&b'-'));
        if matches {
            let n = enc.len();
            best = Some((name, n));
        }
    }
    best
}

/// Pick the directory to cd into so `claude --resume` finds the session that
/// lives under project dir `proj`.
pub fn resolve_resume_dir(first_cwd: &str, last_cwd: &str, proj: &str) -> String {
    if !first_cwd.is_empty() && encode(first_cwd) == proj {
        return first_cwd.to_string();
    }
    if !last_cwd.is_empty() && encode(last_cwd) == proj {
        return last_cwd.to_string();
    }
    if let Some(d) = decode_project_dir(proj) {
        if is_dir(&d) {
            return d.to_string_lossy().into_owned();
        }
    }
    if !first_cwd.is_empty() {
        return first_cwd.to_string();
    }
    last_cwd.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn encode_replaces_everything_non_alnum() {
        assert_eq!(encode(r"C:\Users\me\my.proj"), "C--Users-me-my-proj");
        assert_eq!(encode("/home/me/x y"), "-home-me-x-y");
        assert_eq!(
            encode(r"\\192.0.2.20\Media\South Park"),
            "--192-0-2-20-Media-South-Park"
        );
    }

    #[test]
    fn resolve_prefers_cwd_that_encodes_back() {
        // first_cwd wins when it round-trips.
        let got = resolve_resume_dir(r"C:\Users\me\proj", r"C:\elsewhere", "C--Users-me-proj");
        assert_eq!(got, r"C:\Users\me\proj");
        // last_cwd wins when only it round-trips.
        let got = resolve_resume_dir(r"C:\old", r"C:\Users\me\proj", "C--Users-me-proj");
        assert_eq!(got, r"C:\Users\me\proj");
    }

    #[test]
    fn decode_needs_a_drive_prefix() {
        assert!(decode_project_dir("-home-me-proj").is_none()); // POSIX shape
        assert!(decode_project_dir("--10-66-77-20-share").is_none()); // UNC shape
    }

    #[cfg(windows)]
    #[test]
    fn decode_walks_real_directories_resolving_hyphens() {
        // Build <tmp>/a-b/c.d and encode its path; decode must reconstruct it
        // even though '-' and '.' both encode to '-'.
        //
        // The temp path is canonicalized FIRST, because the walk matches each
        // token against directory names as listed on disk. On GitHub's runners
        // %TEMP% goes through an 8.3 short name (`RUNNER~1`): its `~` encodes to
        // `-`, the listing shows the long name, nothing matches, and decode
        // fabricates a path that cannot canonicalize. That is a genuine limit of
        // the lossy encoding (mitigated in production by `resolve_resume_dir`
        // falling back to the stored cwd), not what this test pins — this test
        // pins hyphen/dot resolution against real directories.
        let tmp = tempfile::tempdir().unwrap();
        let canon = fs::canonicalize(tmp.path()).unwrap();
        // canonicalize returns the `\\?\` extended form, which encodes with a
        // leading "--" that decode rightly refuses; strip it for the test.
        let base = canon.to_str().unwrap().trim_start_matches(r"\\?\").to_string();
        let deep = Path::new(&base).join("a-b").join("c.d");
        fs::create_dir_all(&deep).unwrap();
        let enc = encode(deep.to_str().unwrap());
        let got = decode_project_dir(&enc).unwrap();
        assert_eq!(
            fs::canonicalize(&got).unwrap(),
            fs::canonicalize(&deep).unwrap()
        );
    }
}
