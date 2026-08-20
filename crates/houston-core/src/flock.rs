//! A cross-platform advisory lock that serializes Houston's multi-process
//! critical sections — credential refresh, accounts.json mutations, usage-cache
//! probes, config patches, heal merges — where several processes (statusline
//! renders, `run`, the TUI) can collide.
//!
//! The lock is an OS lock held on an open file handle (`File::try_lock`), NOT
//! the mere existence of a lock file. That distinction is the whole point: an
//! OS lock is released by the kernel when the owning process dies, so a crashed
//! owner can never wedge the next one. The previous create-exclusive design
//! needed a "break the lock if it looks older than 30s" rule, and that rule was
//! a race — two processes could each decide the lock was stale, both break it,
//! and both proceed into the same critical section. It also silently stole the
//! lock from any operation that legitimately ran longer than the timeout.
//!
//! The lock FILE is left on disk between runs; it carries no state, so that is
//! harmless.

use std::fs::{File, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

/// Poll interval while waiting for a busy lock.
const RETRY_EVERY: Duration = Duration::from_millis(25);

/// A held lock. Dropping it releases (the kernel would anyway, on exit).
#[derive(Debug)]
pub struct Lock {
    file: File,
    path: PathBuf,
}

impl Lock {
    /// The lock file's path (diagnostics only).
    pub fn path(&self) -> &Path {
        &self.path
    }
}

impl Drop for Lock {
    fn drop(&mut self) {
        // Explicit for clarity; closing the handle also releases the lock. The
        // file itself is deliberately NOT removed: another waiter may already
        // hold this same path open, and unlinking it under them would let a
        // third process create a fresh file and take a second "exclusive" lock
        // on a different inode.
        let _ = self.file.unlock();
    }
}

fn open_lock_file(path: &Path) -> std::io::Result<File> {
    if let Some(dir) = path.parent() {
        std::fs::create_dir_all(dir)?;
    }
    let mut opts = OpenOptions::new();
    opts.read(true).write(true).create(true).truncate(false);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    opts.open(path)
}

/// Record who holds the lock — a debugging aid, never part of the protocol.
fn stamp(f: &mut File) {
    use std::io::Seek;
    let _ = f.set_len(0);
    let _ = f.rewind();
    let _ = write!(f, "pid {}", std::process::id());
    let _ = f.flush();
}

/// Take the lock without waiting. `None` means another process holds it.
pub fn try_acquire(path: &Path) -> Option<Lock> {
    let mut file = open_lock_file(path).ok()?;
    // Err is either "held elsewhere" (WouldBlock) or a filesystem that cannot
    // lock; both mean we did not get it, and treating them alike keeps callers
    // from proceeding into a critical section they don't own.
    if file.try_lock().is_err() {
        return None;
    }
    stamp(&mut file);
    Some(Lock { file, path: path.to_path_buf() })
}

/// Take the lock, polling for up to `wait` before giving up.
pub fn acquire(path: &Path, wait: Duration) -> std::io::Result<Lock> {
    let deadline = Instant::now() + wait;
    loop {
        if let Some(l) = try_acquire(path) {
            return Ok(l);
        }
        if Instant::now() >= deadline {
            return Err(std::io::Error::new(
                std::io::ErrorKind::WouldBlock,
                format!("lock busy: {}", path.display()),
            ));
        }
        std::thread::sleep(RETRY_EVERY);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exclusive_while_held_released_on_drop() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("x.lock");
        let l = try_acquire(&p).expect("first acquire");
        assert!(try_acquire(&p).is_none(), "held lock must refuse");
        drop(l);
        assert!(try_acquire(&p).is_some(), "released lock must acquire");
    }

    #[test]
    fn acquire_times_out_on_busy_lock() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("busy.lock");
        let _held = try_acquire(&p).unwrap();
        let err = acquire(&p, Duration::from_millis(80)).unwrap_err();
        assert_eq!(err.kind(), std::io::ErrorKind::WouldBlock);
    }

    #[test]
    fn a_long_held_lock_is_never_stolen() {
        // The old design broke any lock whose file looked older than 30s, which
        // let a slow-but-healthy owner lose its critical section. An OS lock has
        // no such rule: age is irrelevant while the owner lives.
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("slow.lock");
        let held = try_acquire(&p).unwrap();
        // Backdate the lock file well beyond any old staleness threshold.
        let ancient = std::time::SystemTime::now() - Duration::from_secs(3600);
        let f = File::options().write(true).open(&p).unwrap();
        let _ = f.set_times(std::fs::FileTimes::new().set_modified(ancient));
        drop(f);
        assert!(try_acquire(&p).is_none(), "an old lock is still a held lock");
        drop(held);
        assert!(try_acquire(&p).is_some());
    }

    #[test]
    fn a_dead_owner_does_not_wedge_the_lock() {
        // Simulates the crash case the old stale-break existed for: the handle
        // goes away without an orderly release, and the next acquire succeeds.
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("crash.lock");
        {
            let f = open_lock_file(&p).unwrap();
            f.try_lock().expect("took the lock");
            // No unlock() — just drop the handle, as a dying process would.
        }
        assert!(try_acquire(&p).is_some(), "the lock must be free again");
    }

    #[test]
    fn many_threads_serialize_through_the_lock() {
        // The property the stale-break race broke: only ONE holder at a time.
        use std::sync::atomic::{AtomicUsize, Ordering};
        use std::sync::Arc;
        let tmp = tempfile::tempdir().unwrap();
        let p = Arc::new(tmp.path().join("race.lock"));
        let inside = Arc::new(AtomicUsize::new(0));
        let max_seen = Arc::new(AtomicUsize::new(0));
        let mut hs = Vec::new();
        for _ in 0..8 {
            let (p, inside, max_seen) = (p.clone(), inside.clone(), max_seen.clone());
            hs.push(std::thread::spawn(move || {
                for _ in 0..20 {
                    if let Ok(l) = acquire(&p, Duration::from_secs(5)) {
                        let n = inside.fetch_add(1, Ordering::SeqCst) + 1;
                        max_seen.fetch_max(n, Ordering::SeqCst);
                        std::thread::sleep(Duration::from_micros(200));
                        inside.fetch_sub(1, Ordering::SeqCst);
                        drop(l);
                    }
                }
            }));
        }
        for h in hs {
            h.join().unwrap();
        }
        assert_eq!(max_seen.load(Ordering::SeqCst), 1, "two holders were inside the critical section at once");
    }
}
