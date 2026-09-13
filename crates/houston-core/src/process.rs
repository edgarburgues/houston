//! Bounded capture for non-interactive commands. Never use for a Claude session.

use std::io::{self, Read};
use std::process::{Child, Command, Output, Stdio};
use std::sync::mpsc;
use std::time::{Duration, Instant};

#[cfg(windows)]
mod windows;

/// Read at most `limit` bytes, rejecting excess rather than retaining it.
pub fn read_bounded(reader: impl Read, limit: usize) -> io::Result<Vec<u8>> {
    let mut bytes = Vec::new();
    reader.take(limit as u64 + 1).read_to_end(&mut bytes)?;
    if bytes.len() > limit {
        return Err(io::Error::other(format!("output exceeded {limit} bytes")));
    }
    Ok(bytes)
}

/// Capture stdout and stderr independently, with one deadline for the command
/// and its pipes. Failure terminates the child tree and reaps the direct child.
pub fn output(cmd: &mut Command, timeout: Duration, limit: usize) -> io::Result<Output> {
    cmd.stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        cmd.process_group(0);
    }
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        cmd.creation_flags(0x0800_0000 | 0x0000_0004); // hidden, suspended until assigned to our job
    }
    let mut child = cmd.spawn()?;
    #[cfg(windows)]
    let job = match windows::Job::attach_and_resume(&child) {
        Ok(job) => job,
        Err(error) => {
            let _ = child.kill();
            let _ = child.wait();
            return Err(error);
        }
    };
    let (tx, rx) = mpsc::channel();
    let stdout = child.stdout.take().expect("piped stdout");
    let stderr = child.stderr.take().expect("piped stderr");
    let err_tx = tx.clone();
    std::thread::spawn(move || {
        let _ = tx.send((false, read_bounded(stdout, limit)));
    });
    std::thread::spawn(move || {
        let _ = err_tx.send((true, read_bounded(stderr, limit)));
    });
    let deadline = Instant::now() + timeout;
    let result = (|| {
        let (mut stdout, mut stderr, mut status) = (None, None, None);
        loop {
            while let Ok((is_err, bytes)) = rx.try_recv() {
                if is_err {
                    stderr = Some(bytes?);
                } else {
                    stdout = Some(bytes?);
                }
            }
            if status.is_none() {
                status = child.try_wait()?;
            }
            if let (Some(status), Some(stdout), Some(stderr)) =
                (status, stdout.as_ref(), stderr.as_ref())
            {
                return Ok(Output {
                    status,
                    stdout: stdout.clone(),
                    stderr: stderr.clone(),
                });
            }
            if Instant::now() >= deadline {
                return Err(io::Error::new(io::ErrorKind::TimedOut, "command timed out"));
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    })();
    // Closing the job terminates descendants even when their parent has exited.
    #[cfg(windows)]
    drop(job);
    kill_tree(&mut child);
    result
}

fn kill_tree(child: &mut Child) {
    #[cfg(unix)]
    // SAFETY: the child was created in its own process group. A negative pid
    // targets that group, including descendants holding our output pipes open.
    unsafe {
        libc::kill(-(child.id() as i32), libc::SIGKILL);
    }
    let _ = child.kill();
    let _ = child.wait();
}

#[cfg(test)]
mod tests {
    use super::*;

    // Re-enter only this test in a child process; never invoke installed apps.
    #[test]
    #[allow(clippy::zombie_processes)] // Deliberate orphan; the enclosing job test must reap it.
    fn command_fixture() {
        let Ok(mode) = std::env::var("HOUSTON_PROCESS_FIXTURE") else {
            return;
        };
        if mode == "flood" {
            use io::Write;
            loop {
                let _ = io::stdout().write_all(&[b'x'; 8192]);
            }
        }
        if mode == "tree" {
            let mut child = fixture("sleep").spawn().unwrap();
            let path = std::env::var("HOUSTON_PROCESS_PID").unwrap();
            let until = Instant::now() + Duration::from_secs(5);
            while !std::path::Path::new(&path).exists() && Instant::now() < until {
                std::thread::sleep(Duration::from_millis(10));
            }
            // Intentionally orphan the fixture to exercise descendant cleanup.
            if !std::path::Path::new(&path).exists() {
                let _ = child.kill();
                let _ = child.wait();
            }
            return;
        }
        if mode == "sleep" {
            if let Ok(path) = std::env::var("HOUSTON_PROCESS_PID") {
                std::fs::write(path, std::process::id().to_string()).unwrap();
            }
            std::thread::sleep(Duration::from_secs(60));
        }
    }

    fn fixture(mode: &str) -> Command {
        let mut cmd = Command::new(std::env::current_exe().unwrap());
        cmd.args(["--exact", "process::tests::command_fixture", "--nocapture"])
            .env("HOUSTON_PROCESS_FIXTURE", mode);
        cmd
    }

    #[test]
    fn oversized_output_is_rejected_while_the_writer_is_running() {
        let start = Instant::now();
        let err = output(&mut fixture("flood"), Duration::from_secs(10), 1024).unwrap_err();
        assert!(err.to_string().contains("exceeded"), "{err}");
        assert!(start.elapsed() < Duration::from_secs(8));
        assert!(read_bounded(&b"12345"[..], 4).is_err());
        assert_eq!(read_bounded(&b"1234"[..], 4).unwrap(), b"1234");
    }

    #[cfg(windows)]
    #[test]
    fn descendants_are_killed_even_after_their_parent_exits() {
        use windows_sys::Win32::{
            Foundation::{CloseHandle, WAIT_OBJECT_0},
            System::Threading::{OpenProcess, WaitForSingleObject, PROCESS_SYNCHRONIZE},
        };
        let dir = tempfile::tempdir().unwrap();
        let pid_path = dir.path().join("descendant-pid");
        let mut cmd = fixture("tree");
        cmd.env("HOUSTON_PROCESS_PID", &pid_path);
        assert_eq!(
            output(&mut cmd, Duration::from_secs(2), 4096)
                .unwrap_err()
                .kind(),
            io::ErrorKind::TimedOut
        );
        let pid: u32 = std::fs::read_to_string(pid_path).unwrap().parse().unwrap();
        // SAFETY: handle is checked and closed once; only wait access is requested.
        unsafe {
            let handle = OpenProcess(PROCESS_SYNCHRONIZE, 0, pid);
            if !handle.is_null() {
                let state = WaitForSingleObject(handle, 5000);
                CloseHandle(handle);
                assert_eq!(state, WAIT_OBJECT_0, "orphaned descendant survived");
            }
        }
    }

    #[test]
    fn timeout_reaps_the_process() {
        let dir = tempfile::tempdir().unwrap();
        let pid_path = dir.path().join("pid");
        let mut cmd = fixture("sleep");
        cmd.env("HOUSTON_PROCESS_PID", &pid_path);
        let err = output(&mut cmd, Duration::from_secs(1), 4096).unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::TimedOut);
        let pid = std::fs::read_to_string(pid_path).expect("child reached its sleep");
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt;
            let out = Command::new("tasklist")
                .args(["/FI", &format!("PID eq {pid}"), "/NH"])
                .creation_flags(0x0800_0000)
                .output()
                .unwrap();
            assert!(!String::from_utf8_lossy(&out.stdout).contains(&pid));
        }
        #[cfg(unix)]
        assert!(!Command::new("kill")
            .args(["-0", &pid])
            .stderr(Stdio::null())
            .status()
            .unwrap()
            .success());
    }
}
