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
//!   once. The temp is therefore CREATED with the destination's mode (0600 when
//!   there is no destination yet) rather than chmod'ed afterwards — patching it
//!   after the write still leaves the contents exposed while the write runs.
//!
//! This is the one atomic writer in the crate. There used to be three: this
//! one, a copy in `accounts` that got the mode right, and a hand-rolled
//! temp+rename for the refresh stamp in `usage`. Three writers meant the
//! lesson above was learned in one of them and not the others.

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
    // A temp left by a previous crash of this same pid would be REOPENED
    // below, keeping whatever mode it already had, so it goes first.
    let _ = fs::remove_file(&tmp);

    let mut opts = fs::OpenOptions::new();
    opts.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
        // The mode is set when the temp is CREATED, not patched afterwards.
        // Writing under the default umask and chmod'ing after still exposes
        // the new contents for the length of the write, and for a credential
        // file that is the whole problem rather than a smaller version of it.
        // An existing destination lends its mode; a new file gets 0600,
        // because everything routed through here is Houston's own state.
        let mode = fs::metadata(path).map(|m| m.permissions().mode() & 0o777).unwrap_or(0o600);
        opts.mode(mode);
    }
    {
        use io::Write;
        let mut f = opts.open(&tmp)?;
        f.write_all(bytes)?;
        f.flush()?;
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
