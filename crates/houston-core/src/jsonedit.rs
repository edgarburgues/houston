//! Surgical edits to JSON config files that OTHER programs own (Claude Code's
//! `.claude.json` and `settings.json`): read → patch the top-level object →
//! atomic write, under an advisory lock. Unrelated fields are carried through
//! untouched, so nothing we don't model is ever dropped; the file is
//! re-indented with sorted keys, which matches how Claude Code itself rewrites
//! these files.

use crate::flock;
use serde_json::{Map, Value};
use std::path::Path;
use std::time::Duration;

/// How long a patch waits for another writer of the same file.
const LOCK_WAIT: Duration = Duration::from_secs(5);

/// A top-level JSON object. `serde_json::Map` preserves insertion order by
/// default; the crate is built with `preserve_order` off, so keys come out
/// sorted — matching Claude Code's own rewrites.
pub type Obj = Map<String, Value>;

/// Edit the top-level object of the JSON file at `path` under an advisory lock.
/// A missing file starts from `{}` when `create`, else it's an error. The result
/// is written via unique-temp + rename, keeping the original file's mode.
pub fn patch<F>(path: &Path, create: bool, f: F) -> std::io::Result<()>
where
    F: FnOnce(&mut Obj) -> std::io::Result<()>,
{
    // Append ".lock" to the FULL name rather than rewriting the extension: a
    // path with no extension would otherwise become "foo..lock", and two files
    // like `a.json` and `a.yaml` must not share one lock.
    let mut lock_name = path.file_name().map(|n| n.to_os_string()).unwrap_or_default();
    lock_name.push(".lock");
    let lock_path = path.with_file_name(lock_name);
    let _lk = flock::acquire(&lock_path, LOCK_WAIT)
        .map_err(|e| std::io::Error::other(format!("config busy in another process: {e}")))?;

    let bytes = match std::fs::read(path) {
        Ok(b) => b,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound && create => b"{}".to_vec(),
        Err(e) => return Err(e),
    };
    // Strip a UTF-8 BOM some editors prepend; serde would reject it.
    let bytes = bytes.strip_prefix(&[0xEF, 0xBB, 0xBF]).unwrap_or(&bytes).to_vec();
    let mut obj: Obj = match serde_json::from_slice::<Value>(&bytes) {
        Ok(Value::Object(m)) => m,
        Ok(Value::Null) => Obj::new(),
        Ok(_) => return Err(std::io::Error::other(format!("{} is not a JSON object", path.display()))),
        Err(e) => return Err(std::io::Error::other(format!("{} is not valid JSON: {e}", path.display()))),
    };
    f(&mut obj)?;
    let mut out = serde_json::to_vec_pretty(&Value::Object(obj))?;
    out.push(b'\n');
    write_atomic(path, &out)
}

/// Read and parse a JSON object from `path` (no lock: the rename-based writers
/// make reads atomic).
pub fn read_obj(path: &Path) -> std::io::Result<Obj> {
    let bytes = std::fs::read(path)?;
    let bytes = bytes.strip_prefix(&[0xEF, 0xBB, 0xBF]).unwrap_or(&bytes).to_vec();
    match serde_json::from_slice::<Value>(&bytes) {
        Ok(Value::Object(m)) => Ok(m),
        Ok(_) => Err(std::io::Error::other("not a JSON object")),
        Err(e) => Err(std::io::Error::other(format!("invalid JSON: {e}"))),
    }
}

/// The object stored under `key` (empty when the key is absent or null).
/// Errors if the key holds a non-object.
pub fn sub_object(obj: &Obj, key: &str) -> std::io::Result<Obj> {
    match obj.get(key) {
        None | Some(Value::Null) => Ok(Obj::new()),
        Some(Value::Object(m)) => Ok(m.clone()),
        Some(_) => Err(std::io::Error::other(format!("{key:?} is not a JSON object"))),
    }
}

/// Store `sub` under `key`. An empty sub-object removes the key entirely, which
/// keeps configs tidy.
pub fn set_sub_object(obj: &mut Obj, key: &str, sub: Obj) {
    if sub.is_empty() {
        obj.remove(key);
    } else {
        obj.insert(key.to_string(), Value::Object(sub));
    }
}

/// Write via a uniquely-named same-dir temp file + rename. Unique because a
/// fixed temp name can interleave between two writers and rename half-written
/// bytes into place. New files are owner-only: these configs embed tokens.
fn write_atomic(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
    let dir = path.parent().unwrap_or(Path::new("."));
    std::fs::create_dir_all(dir)?;
    let name = path.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_else(|| "cfg".into());
    let tmp = dir.join(format!(".{name}.{}.tmp", std::process::id()));

    let mut opts = std::fs::OpenOptions::new();
    opts.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        // Keep the original mode when there is one; 0600 for new files.
        let mode = std::fs::metadata(path).map(|m| {
            use std::os::unix::fs::PermissionsExt;
            m.permissions().mode() & 0o777
        });
        opts.mode(mode.unwrap_or(0o600));
    }
    {
        use std::io::Write;
        let mut f = opts.open(&tmp)?;
        f.write_all(bytes)?;
        f.flush()?;
    }
    if let Err(e) = std::fs::rename(&tmp, path) {
        let _ = std::fs::remove_file(&tmp);
        return Err(e);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn patch_preserves_unrelated_fields_exactly() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join(".claude.json");
        // A config with fields Houston knows nothing about, including a nested
        // object and a large number whose value must survive.
        std::fs::write(
            &p,
            r#"{"numStartups":42,"installMethod":"native","tipsHistory":{"x":1},
                "mcpServers":{"old":{"command":"a"}},"bigId":9007199254740993}"#,
        )
        .unwrap();

        patch(&p, false, |obj| {
            let mut sub = sub_object(obj, "mcpServers")?;
            sub.insert("new".into(), serde_json::json!({"command": "b"}));
            set_sub_object(obj, "mcpServers", sub);
            Ok(())
        })
        .unwrap();

        let back = read_obj(&p).unwrap();
        assert_eq!(back["numStartups"], 42);
        assert_eq!(back["installMethod"], "native");
        assert_eq!(back["tipsHistory"]["x"], 1);
        assert_eq!(back["bigId"], 9007199254740993i64, "large ints keep their value");
        assert_eq!(back["mcpServers"]["old"]["command"], "a", "existing entry untouched");
        assert_eq!(back["mcpServers"]["new"]["command"], "b");
    }

    #[test]
    fn patch_creates_when_asked_and_refuses_when_not() {
        let dir = tempfile::tempdir().unwrap();
        let missing = dir.path().join("new.json");
        patch(&missing, true, |o| {
            o.insert("a".into(), serde_json::json!(1));
            Ok(())
        })
        .unwrap();
        assert_eq!(read_obj(&missing).unwrap()["a"], 1);

        let other = dir.path().join("nope.json");
        assert!(patch(&other, false, |_| Ok(())).is_err());
        assert!(!other.exists());
    }

    #[test]
    fn empty_sub_object_drops_the_key_and_bad_shapes_error() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("s.json");
        std::fs::write(&p, r#"{"enabledPlugins":{"x@m":true},"keep":1}"#).unwrap();
        patch(&p, false, |obj| {
            set_sub_object(obj, "enabledPlugins", Obj::new());
            Ok(())
        })
        .unwrap();
        let back = read_obj(&p).unwrap();
        assert!(!back.contains_key("enabledPlugins"), "empty map removes the key");
        assert_eq!(back["keep"], 1);

        // A non-object under the key is an error, not a silent overwrite.
        std::fs::write(&p, r#"{"mcpServers":["not","an","object"]}"#).unwrap();
        let obj = read_obj(&p).unwrap();
        assert!(sub_object(&obj, "mcpServers").is_err());

        // A file that isn't JSON at all fails loudly.
        std::fs::write(&p, "{ nope").unwrap();
        assert!(patch(&p, false, |_| Ok(())).is_err());
    }

    #[test]
    fn a_failing_patch_leaves_the_file_untouched() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("c.json");
        std::fs::write(&p, r#"{"a":1}"#).unwrap();
        let err = patch(&p, false, |_| Err(std::io::Error::other("nope")));
        assert!(err.is_err());
        assert_eq!(std::fs::read_to_string(&p).unwrap(), r#"{"a":1}"#, "no partial write");
        // And no temp files were left behind.
        let leftovers: Vec<_> = std::fs::read_dir(dir.path())
            .unwrap()
            .filter_map(|e| e.ok())
            .filter(|e| e.file_name().to_string_lossy().ends_with(".tmp"))
            .collect();
        assert!(leftovers.is_empty(), "temp files cleaned up");
    }
}
