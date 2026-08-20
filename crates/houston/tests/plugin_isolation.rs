//! The security property behind process-isolated plugins: a WASM guest that
//! loops forever must be KILLED, not merely abandoned. With the old
//! thread-based host the call timed out but the runaway thread kept a CPU core
//! pegged for the rest of the session, and wasm fuel/epoch interruption is
//! unusable here (its trap aborts the whole process on this toolchain).
//!
//! These tests drive the real binary through `houston wasm-host`, so they cover
//! the actual parent/child protocol rather than a stand-in.

use houston_api as api;
use houston_plugins::ProcHost;
use std::time::{Duration, Instant};

/// A guest whose `houston_call` never returns.
const SPINNER: &str = r#"(module
  (memory (export "memory") 1)
  (func (export "houston_alloc") (param i32) (result i32) (i32.const 0))
  (func (export "houston_call") (param i32) (param i32) (result i64)
    (loop $l br $l) (i64.const 0))
)"#;

/// A well-behaved guest that answers with a fixed Response.
fn responder(json: &str) -> String {
    let off = 1024u64;
    let packed = (off << 32) | json.len() as u64;
    let escaped: String = json.bytes().map(|b| format!("\\{b:02x}")).collect();
    format!(
        r#"(module
  (memory (export "memory") 1)
  (global $bump (mut i32) (i32.const 8192))
  (data (i32.const {off}) "{escaped}")
  (func (export "houston_alloc") (param $len i32) (result i32)
    (local $p i32)
    global.get $bump local.set $p
    global.get $bump local.get $len i32.add global.set $bump
    local.get $p)
  (func (export "houston_call") (param i32) (param i32) (result i64) (i64.const {packed}))
)"#
    )
}

fn host_for(source: &str, timeout: Duration) -> (ProcHost, tempfile::TempDir) {
    let dir = tempfile::tempdir().unwrap();
    let module = dir.path().join("guest.wat");
    std::fs::write(&module, source).unwrap();
    let exe = std::path::PathBuf::from(env!("CARGO_BIN_EXE_houston"));
    (ProcHost::new(exe, module, timeout), dir)
}

fn render() -> api::Call {
    api::Call::Render { req: api::RenderRequest { api: api::API_VERSION, ..Default::default() } }
}

#[test]
fn a_runaway_guest_is_killed_not_left_spinning() {
    let (host, _dir) = host_for(SPINNER, Duration::from_millis(400));
    assert!(!host.is_running(), "nothing spawns until the first call");

    let t0 = Instant::now();
    let err = host.call(&render()).expect_err("a spinning guest must not return Ok");
    let waited = t0.elapsed();

    assert!(err.to_string().contains("timed out"), "unexpected error: {err}");
    // The UI is not held hostage: the call returns near the timeout, not later.
    assert!(waited < Duration::from_secs(5), "call took {waited:?}");
    // THE point: no child survives to burn a core.
    assert!(!host.is_running(), "the runaway child must be dead, not abandoned");
    assert!(host.pid().is_none());
}

#[test]
fn the_host_recovers_after_killing_a_runaway() {
    // A timeout must not poison the widget: the next call gets a fresh child.
    let (spin, _d1) = host_for(SPINNER, Duration::from_millis(300));
    assert!(spin.call(&render()).is_err());
    assert!(spin.call(&render()).is_err(), "a second attempt still times out cleanly");
    assert!(!spin.is_running());

    let json = r#"{"title":"alive","lines":[{"spans":[{"text":"ok"}]}]}"#;
    let (good, _d2) = host_for(&responder(json), Duration::from_secs(10));
    let r = good.call(&render()).expect("a healthy guest answers");
    assert_eq!(r.title.as_deref(), Some("alive"));
    assert!(good.is_running(), "a healthy child stays up for the next call");
    // State persists across calls: the same child answers again.
    let pid = good.pid();
    assert!(good.call(&render()).is_ok());
    assert_eq!(good.pid(), pid, "the child is reused, not respawned per call");
}

#[test]
fn a_broken_module_fails_without_leaving_a_child() {
    let (host, _dir) = host_for("(module (func))", Duration::from_secs(5));
    // No `memory`/`houston_alloc` exports: the child reports and exits.
    assert!(host.call(&render()).is_err());
    assert!(!host.is_running());
}

#[test]
fn dropping_the_host_reaps_its_child() {
    let json = r#"{"lines":[]}"#;
    let (host, _dir) = host_for(&responder(json), Duration::from_secs(10));
    host.call(&render()).unwrap();
    let pid = host.pid().expect("a child is live");
    drop(host);
    // The pid must no longer be a live houston process. Give the OS a moment to
    // finish the kill+reap before looking.
    std::thread::sleep(Duration::from_millis(300));
    assert!(!pid_is_live(pid), "pid {pid} outlived its host");
}

/// Whether a pid is still a running process (best effort, per platform).
fn pid_is_live(pid: u32) -> bool {
    #[cfg(windows)]
    {
        let out = std::process::Command::new("tasklist")
            .args(["/FI", &format!("PID eq {pid}"), "/NH"])
            .output();
        match out {
            Ok(o) => {
                let s = String::from_utf8_lossy(&o.stdout);
                s.contains(&pid.to_string())
            }
            Err(_) => false,
        }
    }
    #[cfg(not(windows))]
    {
        std::path::Path::new(&format!("/proc/{pid}")).exists()
            || std::process::Command::new("kill")
                .args(["-0", &pid.to_string()])
                .status()
                .map(|s| s.success())
                .unwrap_or(false)
    }
}
