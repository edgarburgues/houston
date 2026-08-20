//! Well-known paths: the user home and Houston's store dir.

use std::env;
use std::path::PathBuf;

/// The user's home directory. `USERPROFILE` on Windows, `HOME` elsewhere.
pub fn home() -> PathBuf {
    #[cfg(windows)]
    let var = "USERPROFILE";
    #[cfg(not(windows))]
    let var = "HOME";
    PathBuf::from(env::var_os(var).unwrap_or_default())
}

/// Houston's data dir: `~/.claude/houston`, overridable with `HOUSTON_HOME`
/// (same contract as v1, so 2.0 reads the existing store).
pub fn store_dir() -> PathBuf {
    if let Some(h) = env::var_os("HOUSTON_HOME") {
        if !h.is_empty() {
            return PathBuf::from(h);
        }
    }
    home().join(".claude").join("houston")
}
