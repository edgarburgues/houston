//! One atomic file write, shared.
//!
//! Every file Houston owns is replaced rather than edited in place, so a crash
//! or a kill mid-write can never leave a truncated document where a valid one
//! was. Two details in here are the ones that actually bite, and both were
//! learned the hard way:
//!
//! - **The temp file must be uniquely named.** A fixed `.tmp` lets two writers
//!   interleave — A writes half, B writes half, A renames — and rename half a
//!   document into place. The process id makes the collision impossible.
//! - **The rename installs the TEMP file's permissions**, not the target's. So
//!   creating the temp with the default umask silently WIDENS a 0600 file on
//!   every write, which is how a credential store quietly became world-readable
//!   once. When the destination exists, its mode is copied onto the temp before
//!   the rename.

use std::fs;
use std::io;
use std::path::Path;

/// Replace `path` with `bytes` atomically, keeping the destination's
/// permissions when it already exists.
pub fn write(path: &Path, bytes: &[u8]) -> io::Result<()> {
    let dir = path.parent().unwrap_or(Path::new("."));
    let base = path.file_name().unwrap_or_default().to_string_lossy();
    // Hidden and pid-tagged: unique per writer, and never mistaken for content
    // by a directory listing that globs for the real name.
    let tmp = dir.join(format!(".{base}.{}.tmp", std::process::id()));
    fs::write(&tmp, bytes)?;
    if let Ok(meta) = fs::metadata(path) {
        // Best effort: a filesystem that cannot carry the mode is not a reason
        // to fail the write, but it IS a reason to try before renaming.
        let _ = fs::set_permissions(&tmp, meta.permissions());
    }
    match fs::rename(&tmp, path) {
        Ok(()) => Ok(()),
        Err(e) => {
            let _ = fs::remove_file(&tmp);
            Err(e)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn replaces_content_and_leaves_no_temp_behind() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("f.json");
        write(&p, b"first").unwrap();
        write(&p, b"second").unwrap();
        assert_eq!(fs::read(&p).unwrap(), b"second");
        let leftovers: Vec<_> = fs::read_dir(tmp.path())
            .unwrap()
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|n| n != "f.json")
            .collect();
        assert!(leftovers.is_empty(), "temp files must not survive a successful write: {leftovers:?}");
    }

    /// The regression this helper exists to prevent: a rename installs the temp
    /// file's mode, so a restrictive destination must not be widened by a write.
    #[cfg(unix)]
    #[test]
    fn an_owner_only_file_stays_owner_only() {
        use std::os::unix::fs::PermissionsExt;
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("secret.json");
        fs::write(&p, b"{}").unwrap();
        fs::set_permissions(&p, fs::Permissions::from_mode(0o600)).unwrap();
        write(&p, b"{\"token\":\"x\"}").unwrap();
        let mode = fs::metadata(&p).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "the write widened a credential file");
    }

    #[test]
    fn a_missing_parent_is_an_error_not_a_panic() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("nope").join("f.json");
        assert!(write(&p, b"x").is_err());
    }
}
