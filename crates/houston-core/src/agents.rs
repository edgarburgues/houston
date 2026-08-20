//! Live sessions, from `claude agents --json`.
//!
//! This is the one thing Houston cannot learn by scanning: a transcript on disk
//! says a conversation exists, never whether a claude is running on it right
//! now. One cheap subprocess call answers it — pid, cwd, busy/idle — and the
//! `sessionId` it reports is exactly Houston's mission id, so it joins straight
//! onto the mission list.
//!
//! Two measured facts shape the whole module:
//!
//! - **It costs ~1.2 s** (node startup), which rules out polling on any UI
//!   cadence. Callers refresh it on the scan's rhythm — start, `r`, and after a
//!   session returns — and treat the answer as a snapshot with an age, not as
//!   live truth.
//! - **The query must name a config dir.** The live registry lives in
//!   `sessions/`, which Houston junctions into every account, so asking through
//!   ANY account sees EVERY Houston session. Asking with `CLAUDE_CONFIG_DIR`
//!   unset reads `~/.claude/sessions`, which Houston leaves empty — that returns
//!   `[]` and would look exactly like "nothing is running".
//!
//! Consequence worth stating: a claude started outside Houston, on the default
//! config, is invisible here. Finding it would cost a second 1.2 s call for a
//! case Houston's premise (you launch through it) makes rare.

use crate::accounts;
use crate::launch::claude_bin;
use serde::{Deserialize, Serialize};
use std::process::{Command, Stdio};
use std::time::Duration;

/// How long we wait for the query before giving up on it.
pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(10);

/// One running session.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Live {
    pub pid: i64,
    pub cwd: String,
    /// `interactive` for a session at a terminal, or a background agent.
    pub kind: String,
    /// Unix **milliseconds** — Claude's own unit here, kept as reported rather
    /// than silently converted, so a mismatch is visible instead of subtle.
    pub started_at: i64,
    /// Claude's session id === Houston's mission id.
    pub session_id: String,
    /// The session's name (`claude -n`, `/rename`), when it has one.
    pub name: String,
    /// `busy`, `idle`, … taken verbatim: Claude owns this vocabulary and may
    /// extend it, and a Houston that mapped it to an enum would show a session
    /// in an unknown state as nothing at all.
    pub status: String,
}

impl Live {
    /// Whether the session is working right now (as opposed to waiting).
    pub fn busy(&self) -> bool {
        self.status.eq_ignore_ascii_case("busy")
    }

    /// Whether this is a process on THIS machine — something the pid identifies,
    /// and something a local resume would collide with.
    ///
    /// The registry is not only local sessions: Claude also lists cloud sessions
    /// and disconnected Remote Control ones (labelling them `cloud` and `offline`
    /// since 2.1.229). Houston deliberately does not enumerate that vocabulary —
    /// see `status` above — so it checks the thing it can actually verify. A
    /// missing pid changes the advice rather than the marker: the chat is open, so
    /// say so, but "two claudes on one transcript" is not what attaching would do
    /// when the other one is on someone else's machine.
    pub fn local(&self) -> bool {
        self.pid > 0
    }

    /// Seconds this session has been up, given the current unix seconds.
    pub fn uptime_secs(&self, now: i64) -> i64 {
        (now - self.started_at / 1000).max(0)
    }
}

/// Decode the command's output. Pure, so the shape is tested without spawning
/// anything.
///
/// Unknown fields are ignored and `kind`/`status` are Strings taken verbatim, so
/// a release that extends either vocabulary — as 2.1.229 did, adding `cloud` and
/// `offline` — adds information here instead of erasing the session.
///
/// Tolerates a non-array payload (an error object, an empty stream) by returning
/// nothing, and tolerates junk *around* the array — a version notice or a
/// warning line printed before the JSON would otherwise turn every live session
/// into no live sessions.
pub fn parse(text: &str) -> Vec<Live> {
    if let Ok(v) = serde_json::from_str::<Vec<Live>>(text) {
        return v;
    }
    let start = text.find('[');
    let end = text.rfind(']');
    if let (Some(s), Some(e)) = (start, end) {
        if e > s {
            if let Ok(v) = serde_json::from_str::<Vec<Live>>(&text[s..=e]) {
                return v;
            }
        }
    }
    Vec::new()
}

/// The config dir to ask through: any account Houston manages, since they all
/// share one `sessions/` registry. `None` means "let claude use its default",
/// which is right for a machine with no Houston accounts and wrong everywhere
/// else — see the module docs.
fn query_config_dir() -> Option<std::path::PathBuf> {
    let accs = accounts::load().unwrap_or_default();
    accs.iter().find_map(|a| a.resolve_config_dir().filter(|d| d.is_dir()))
}

/// Ask claude for its live sessions. Never an error: anything unexpected — no
/// claude on PATH, an old version without the verb, a timeout, unparseable
/// output — means "no live information", which the UI shows as an unadorned
/// mission list.
pub fn list(timeout: Duration) -> Vec<Live> {
    let mut cmd = Command::new(claude_bin());
    cmd.args(["agents", "--json"]);
    // Piped stdout and null stderr are not tidiness: this runs while a TUI owns
    // the terminal, and a child writing to it would corrupt the screen.
    cmd.stdin(Stdio::null()).stdout(Stdio::piped()).stderr(Stdio::null());
    if let Some(dir) = query_config_dir() {
        cmd.env("CLAUDE_CONFIG_DIR", dir);
    }
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        cmd.creation_flags(CREATE_NO_WINDOW);
    }
    let Ok(child) = cmd.spawn() else { return Vec::new() };

    // Bound OUR WAIT, not the child's life. Killing it would need a platform
    // handle we deliberately do not depend on, and it does not matter here: the
    // query is short-lived and read-only, holds no lock and owns no terminal, so
    // a straggler costs nothing while the caller carries on. Reading the pipe in
    // the same thread as the wait is what avoids the classic deadlock of a child
    // blocked on a full pipe buffer.
    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let _ = tx.send(child.wait_with_output().ok());
    });
    match rx.recv_timeout(timeout) {
        Ok(Some(out)) => parse(&String::from_utf8_lossy(&out.stdout)),
        _ => Vec::new(),
    }
}

// --- the snapshot on disk, so the first paint is not 1.2 s late ----------------

/// Why this is a cache and not an event-sourced live set.
///
/// The tempting design is to keep the live set up to date from `session-start`
/// hooks and skip the query. It cannot work: `SessionStart`'s payload carries no
/// pid, and `SessionEnd` was retired because it is always cancelled — so events
/// can say a session *began* and never that it stopped. A set built from starts
/// alone would mark every chat ever opened as live, permanently, and that lie
/// changes behaviour: `Enter` would offer to fork a chat nobody has open.
///
/// So liveness stays a *measurement* (pids, from `agents --json`) and the events
/// do what Decision 1 allows them to do — say "now is a good moment to measure
/// again". This cache is the other half: the last measurement, with its age, so
/// the markers appear instantly and the query happens behind them.
#[derive(Serialize, Deserialize, Default)]
struct Snapshot {
    /// Unix seconds the query ran.
    taken_at: i64,
    live: Vec<Live>,
}

fn cache_path() -> std::path::PathBuf {
    crate::paths::store_dir().join("live-cache-v2.json")
}

fn now_secs() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// The last snapshot and how many seconds ago it was taken. Instant: one file
/// read, no subprocess. `None` age = never measured, which the UI must not
/// confuse with "nothing is running".
pub fn read_cached() -> (Vec<Live>, Option<i64>) {
    let Ok(b) = std::fs::read(cache_path()) else { return (Vec::new(), None) };
    let Ok(s) = serde_json::from_slice::<Snapshot>(&b) else { return (Vec::new(), None) };
    (s.live, Some((now_secs() - s.taken_at).max(0)))
}

/// Measure, and write the snapshot. Blocking (~1.2 s) — the caller must be off
/// any render path.
///
/// Single-flighted across processes: a `session-start` hook and a TUI refresh can
/// easily coincide, and two node startups for one answer is the kind of waste that
/// makes people turn features off.
pub fn refresh(timeout: Duration) -> Vec<Live> {
    let path = cache_path();
    let _ = std::fs::create_dir_all(path.parent().unwrap_or(std::path::Path::new(".")));
    let mut lock_name = path.file_name().map(|n| n.to_os_string()).unwrap_or_default();
    lock_name.push(".lock");
    let Some(_lk) = crate::flock::try_acquire(&path.with_file_name(lock_name)) else {
        // Somebody else is asking right now; their answer will land in the cache.
        return read_cached().0;
    };
    let live = list(timeout);
    if let Ok(b) = serde_json::to_vec(&Snapshot { taken_at: now_secs(), live: live.clone() }) {
        let _ = crate::atomic::write(&path, &b);
    }
    live
}

/// Start `houston live --refresh` detached and return at once.
///
/// Used by the `session-start` hook: the one moment the answer is known to have
/// changed. The hook itself must not wait 1.2 s inside somebody's session.
pub fn spawn_background_refresh() {
    let exe = std::env::current_exe().unwrap_or_else(|_| std::path::PathBuf::from("houston"));
    let mut cmd = Command::new(exe);
    cmd.args(["live", "--refresh"])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const DETACHED_PROCESS: u32 = 0x0000_0008;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        cmd.creation_flags(DETACHED_PROCESS | CREATE_NO_WINDOW);
    }
    let _ = cmd.spawn();
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The exact shape a real `claude agents --json` returns.
    const REAL: &str = r#"[
      { "pid": 12164, "cwd": "C:\\Users\\me", "kind": "interactive",
        "startedAt": 1785018546730, "sessionId": "916d51ed-5c03-4e43-a1e3-2837fc2a1af7",
        "name": "houston-store-format-design", "status": "busy" }
    ]"#;

    #[test]
    fn parses_the_real_payload() {
        let v = parse(REAL);
        assert_eq!(v.len(), 1);
        assert_eq!(v[0].pid, 12164);
        assert_eq!(v[0].session_id, "916d51ed-5c03-4e43-a1e3-2837fc2a1af7");
        assert_eq!(v[0].name, "houston-store-format-design");
        assert!(v[0].busy());
        assert_eq!(v[0].kind, "interactive");
        // startedAt is MILLIS; uptime is seconds.
        assert_eq!(v[0].uptime_secs(1785018546 + 90), 90);
        // A clock behind the session's start reads as 0, not negative.
        assert_eq!(v[0].uptime_secs(0), 0);
    }

    /// Claude extends `kind` and `status` over time — 2.1.229 added `cloud` and
    /// `offline` — and it may carry fields Houston has never heard of. Neither may
    /// turn a live session into no live session, which is what a strict enum or
    /// `deny_unknown_fields` would do.
    #[test]
    fn an_extended_vocabulary_and_unknown_fields_survive() {
        let v = parse(
            r#"[{ "pid": 0, "cwd": "", "kind": "cloud", "status": "offline",
                  "sessionId": "s1", "name": "n", "startedAt": 1,
                  "someFieldFromTheFuture": {"a":1} }]"#,
        );
        assert_eq!(v.len(), 1, "an unknown field must not drop the session");
        assert_eq!(v[0].kind, "cloud", "taken verbatim, not mapped to an enum");
        assert_eq!(v[0].status, "offline");
        assert!(!v[0].busy());
        // No pid: the chat is open, but not here. The prompt says something else.
        assert!(!v[0].local());
        assert!(parse(REAL)[0].local(), "a real local session has a pid");
    }

    #[test]
    fn no_sessions_and_junk_both_mean_nothing_is_running() {
        assert!(parse("[]").is_empty());
        assert!(parse("").is_empty());
        assert!(parse("not json at all").is_empty());
        // An error object rather than a list.
        assert!(parse(r#"{"error":"unknown flag"}"#).is_empty());
        assert!(parse("[").is_empty());
    }

    /// A notice printed before the JSON must not hide every live session.
    #[test]
    fn junk_around_the_array_is_stripped() {
        let noisy = format!("(node:1) ExperimentalWarning: something\n{REAL}\n");
        assert_eq!(parse(&noisy).len(), 1);
    }

    /// The cache exists so the markers are not 1.2 s late, and it must be honest
    /// about its own age — "never measured" is not "nothing is running".
    #[test]
    fn the_snapshot_survives_a_round_trip_and_reports_its_age() {
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("HOUSTON_HOME", tmp.path()) };

        let (live, age) = read_cached();
        assert!(live.is_empty() && age.is_none(), "never measured is distinguishable from empty");

        // Write a snapshot the way refresh() would, without spawning claude.
        let one = parse(REAL);
        let snap = Snapshot { taken_at: now_secs() - 7, live: one.clone() };
        std::fs::create_dir_all(cache_path().parent().unwrap()).unwrap();
        std::fs::write(cache_path(), serde_json::to_vec(&snap).unwrap()).unwrap();

        let (live, age) = read_cached();
        assert_eq!(live, one, "the whole Live survives, pid and all");
        assert_eq!(age, Some(7), "and says how stale it is");

        // Corruption reads as "never measured" rather than as an error or a panic.
        std::fs::write(cache_path(), "{not json").unwrap();
        assert_eq!(read_cached(), (Vec::new(), None));

        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }

    #[test]
    fn unknown_fields_and_unknown_statuses_survive() {
        let v = parse(
            r#"[{"pid":1,"sessionId":"s","status":"compacting","somethingNew":42},
                {"sessionId":"t"}]"#,
        );
        assert_eq!(v.len(), 2, "an unknown field does not reject the entry");
        assert_eq!(v[0].status, "compacting", "an unknown status is kept verbatim");
        assert!(!v[0].busy());
        // A sparse entry degrades to defaults rather than failing the batch.
        assert_eq!(v[1].pid, 0);
        assert!(v[1].status.is_empty());
    }
}
