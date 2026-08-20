//! The session journal: an append-only record of what Houston did, and later of
//! what Claude Code's hooks report.
//!
//! It exists because the filesystem answers "what conversations are there?" but
//! nothing answers "what happened, when, and with which account?". Tonight's
//! example: three consecutive resumes opened three different accounts and the
//! only surviving evidence was the `lastUse` stamps in the registry — a
//! side effect, not a record. One line per event fixes that.
//!
//! Three properties are deliberate:
//!
//! - **Advisory, never authoritative.** The scan remains the source of truth for
//!   conversations (Decision 1: events may only ever be an accelerator). Nothing
//!   here is allowed to fail a launch, so `append` returns nothing and swallows
//!   its errors — including a lock it could not get, in which case the line is
//!   dropped. Losing an event beats delaying a hook that sits in Claude's
//!   critical path.
//! - **`event` is a plain String.** Phase 3's hooks will write names this binary
//!   has never heard of, and an unknown event must be stored and shown, not
//!   rejected. Houston's own names are the constants below.
//! - **No conversation content, ever.** Account, cwd, ids, timing, reason. The
//!   transcripts already hold the prose; a journal that also held it would be a
//!   second copy of the thing most worth not duplicating.

use crate::atomic;
use crate::flock;
use crate::paths::store_dir;
use serde::{Deserialize, Serialize};
use std::fs::OpenOptions;
use std::io::Write;
use std::path::PathBuf;
use std::time::Duration;

/// Houston opened a NEW session on an account.
pub const EVENT_LAUNCH: &str = "launch";
/// Houston reopened an existing conversation.
pub const EVENT_RESUME: &str = "resume";

// Events reported by Claude Code's own hooks (Phase 3). These strings are BOTH
// the journal event and the `houston hook <verb>` argument, so there is one
// vocabulary instead of a mapping table nobody remembers to update.
/// A session started, resumed or forked (Claude's `SessionStart`).
pub const EVENT_SESSION_START: &str = "session-start";
/// A session ended (Claude's `SessionEnd`).
pub const EVENT_SESSION_END: &str = "session-end";
/// The account just hit a rate limit (`StopFailure` matching `rate_limit`) —
/// known the moment it happens instead of up to 60 s later from a poll.
pub const EVENT_RATE_LIMIT: &str = "rate-limit";
/// Authentication failed (`StopFailure` matching `authentication_failed`).
pub const EVENT_AUTH_FAILED: &str = "auth-failed";
/// A login just succeeded (`Notification` matching `auth_success`), so an account
/// that read as "off" is usable again.
pub const EVENT_AUTH_OK: &str = "auth-ok";

/// How long an append waits for another writer. Short on purpose: a hook or a
/// launch must not stall behind the journal.
const LOCK_WAIT: Duration = Duration::from_secs(1);
/// Size at which the journal is trimmed on the next append.
const MAX_BYTES: u64 = 1024 * 1024;
/// How many of the most recent lines survive a trim.
const KEEP_LINES: usize = 2000;

/// One recorded event. Every optional field is omitted when empty, so a line
/// stays readable at a glance and a future field costs nothing today.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Entry {
    /// Unix seconds, matching the `@unix` stamps the rest of the store uses.
    pub ts: i64,
    pub event: String,
    /// Claude's session id. Empty for a `launch`: a brand-new session has no id
    /// yet, and inventing one by guessing would poison the join. Phase 3's
    /// `SessionStart` hook is what closes that gap.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub session: String,
    /// Houston's mission key (project/id) when known.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub key: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub account: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub cwd: String,
    /// Why this ACCOUNT — the `usage::Pick` vocabulary, the same words
    /// `houston usage --pick` prints. Kept to that one vocabulary on purpose.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub reason: String,
    /// Whatever the event itself says: `SessionStart`'s source (startup, resume,
    /// fork…), `SessionEnd`'s reason, `StopFailure`'s error. A separate field
    /// because mixing two vocabularies in `reason` is how "why" stops meaning
    /// anything.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub detail: String,
}

impl Entry {
    /// An entry stamped now.
    pub fn now(event: &str) -> Self {
        Entry { ts: now_secs(), event: event.to_string(), ..Default::default() }
    }

    /// The timestamp in the reader's local time. Lives here rather than in the
    /// CLI so every consumer (the `journal` verb today, a TUI pane later) shows
    /// the same shape, and so `ts` stays a plain number on disk.
    pub fn when_local(&self) -> String {
        chrono::DateTime::from_timestamp(self.ts, 0)
            .map(|t| t.with_timezone(&chrono::Local).format("%Y-%m-%d %H:%M:%S").to_string())
            .unwrap_or_else(|| self.ts.to_string())
    }
}

fn now_secs() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

pub fn path() -> PathBuf {
    store_dir().join("journal.jsonl")
}

fn lock_path() -> PathBuf {
    store_dir().join("journal.jsonl.lock")
}

/// Record an event. Best effort by contract: any failure — unserialisable entry,
/// missing store dir, lock held elsewhere, disk error — is dropped silently.
pub fn append(e: &Entry) {
    let Ok(line) = serde_json::to_string(e) else { return };
    let path = path();
    let _ = std::fs::create_dir_all(path.parent().unwrap_or(std::path::Path::new(".")));
    // On timeout the line goes, not the caller's turn.
    let Ok(_lk) = flock::acquire(&lock_path(), LOCK_WAIT) else { return };
    trim_locked(&path);
    if let Ok(mut f) = OpenOptions::new().create(true).append(true).open(&path) {
        // Heal a dangling line before adding to it. A killed writer can leave a
        // line with no newline, and appending onto that would fuse two entries
        // into one unparseable line — losing OURS as well as theirs. One crash
        // should cost one line, not two.
        let lead = if ends_without_newline(&path) { "\n" } else { "" };
        // One write for the line AND its newline: a separate write for the
        // newline could interleave with another process's line if the lock ever
        // stopped covering this.
        let _ = f.write_all(format!("{lead}{line}\n").as_bytes());
    }
}

/// Whether the file's last byte is not a newline. Reads one byte, not the file.
fn ends_without_newline(path: &std::path::Path) -> bool {
    use std::io::{Read, Seek, SeekFrom};
    let Ok(mut f) = std::fs::File::open(path) else { return false };
    // Seeking to -1 fails on an empty file, which is the answer we want anyway.
    if f.seek(SeekFrom::End(-1)).is_err() {
        return false;
    }
    let mut b = [0u8; 1];
    f.read_exact(&mut b).is_ok() && b[0] != b'\n'
}

/// Trim an oversized journal to its newest `KEEP_LINES`. Caller holds the lock.
///
/// The rewrite is atomic (temp + rename) so a concurrent *reader*, which takes
/// no lock, can never observe a half-written journal — only the old file or the
/// new one.
fn trim_locked(path: &std::path::Path) {
    let too_big = std::fs::metadata(path).map(|m| m.len() > MAX_BYTES).unwrap_or(false);
    if !too_big {
        return;
    }
    let Ok(body) = std::fs::read_to_string(path) else { return };
    let lines: Vec<&str> = body.lines().filter(|l| !l.trim().is_empty()).collect();
    let keep = lines.len().saturating_sub(KEEP_LINES);
    let mut out = lines[keep..].join("\n");
    out.push('\n');
    let _ = atomic::write(path, out.as_bytes());
}

/// Every readable entry, oldest first.
///
/// Unparseable lines are skipped in silence, which is the normal case rather
/// than the exceptional one: a killed writer can leave a partial line, and a
/// newer Houston may write fields this build does not model. A journal that
/// could break a view by being slightly wrong would be worse than no journal.
pub fn read_all() -> Vec<Entry> {
    let Ok(body) = std::fs::read_to_string(path()) else { return Vec::new() };
    body.lines().filter_map(|l| serde_json::from_str::<Entry>(l).ok()).filter(|e| !e.event.is_empty()).collect()
}

/// The newest `n` entries, oldest first (so it reads like `tail`).
pub fn tail(n: usize) -> Vec<Entry> {
    let all = read_all();
    let start = all.len().saturating_sub(n);
    all[start..].to_vec()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn temp_home() -> tempfile::TempDir {
        let tmp = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("HOUSTON_HOME", tmp.path()) };
        tmp
    }

    #[test]
    fn appends_roundtrip_and_keep_their_order() {
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let _tmp = temp_home();

        append(&Entry { account: "one".into(), reason: "cache".into(), ..Entry::now(EVENT_LAUNCH) });
        append(&Entry {
            session: "abc".into(),
            key: "proj/abc".into(),
            account: "two".into(),
            cwd: "C:\\x".into(),
            reason: "probe".into(),
            ..Entry::now(EVENT_RESUME)
        });

        let all = read_all();
        assert_eq!(all.len(), 2);
        assert_eq!(all[0].event, EVENT_LAUNCH);
        assert_eq!(all[0].account, "one");
        assert!(all[0].session.is_empty(), "a new session has no id yet");
        assert_eq!(all[1].session, "abc");
        assert_eq!(all[1].reason, "probe");
        assert!(all[1].ts > 0);
        assert_eq!(tail(1).len(), 1);
        assert_eq!(tail(1)[0].event, EVENT_RESUME, "tail returns the NEWEST");
        assert_eq!(tail(99).len(), 2, "asking for more than exists is not an error");

        // Empty fields are omitted, so a line stays legible.
        let raw = std::fs::read_to_string(path()).unwrap();
        assert!(!raw.lines().next().unwrap().contains("session"), "empty fields are not written: {raw}");

        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }

    /// A hook from a future Houston writes an event this build never heard of;
    /// it must survive the round trip rather than be filtered out.
    #[test]
    fn unknown_events_and_corrupt_lines_are_tolerated() {
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let _tmp = temp_home();

        std::fs::create_dir_all(path().parent().unwrap()).unwrap();
        std::fs::write(
            path(),
            "{\"ts\":1,\"event\":\"rateLimit\",\"account\":\"one\"}\n\
             {\"ts\":2,\"event\":\"resume\",\"unknownFieldFromLater\":true}\n\
             {half a line, killed mid-write\n\
             \n\
             {\"ts\":3,\"event\":\"sessionEnd\"}\n",
        )
        .unwrap();

        let all = read_all();
        assert_eq!(all.len(), 3, "the corrupt and blank lines are skipped, the rest survive");
        assert_eq!(all[0].event, "rateLimit", "an event name this build does not know is kept");
        assert_eq!(all[1].event, "resume", "an unknown FIELD does not reject the line");
        assert_eq!(all[2].event, "sessionEnd");
        // Appending after corruption still works and does not try to repair it.
        append(&Entry::now(EVENT_RESUME));
        assert_eq!(read_all().len(), 4);

        // A line left WITHOUT its newline by a killed writer must not swallow the
        // next entry: appending onto it would fuse two into one unparseable line.
        std::fs::write(path(), "{\"ts\":1,\"event\":\"resume\"}\n{\"ts\":2,\"event\":\"partial\"").unwrap();
        append(&Entry { detail: "after".into(), ..Entry::now(EVENT_LAUNCH) });
        let all = read_all();
        assert_eq!(all.len(), 2, "the whole line survived; only the partial one was lost: {all:?}");
        assert_eq!(all[1].event, EVENT_LAUNCH);
        assert_eq!(all[1].detail, "after");

        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }

    #[test]
    fn an_oversized_journal_is_trimmed_to_its_newest_lines() {
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let _tmp = temp_home();

        // Write past the cap directly (faster than appending a million lines),
        // each line carrying its index so the survivors are identifiable.
        std::fs::create_dir_all(path().parent().unwrap()).unwrap();
        let pad = "x".repeat(400);
        let mut body = String::new();
        let total = 3_000;
        for i in 0..total {
            body.push_str(&format!("{{\"ts\":{i},\"event\":\"resume\",\"cwd\":\"{pad}\"}}\n"));
        }
        std::fs::write(path(), &body).unwrap();
        assert!(std::fs::metadata(path()).unwrap().len() > MAX_BYTES);

        append(&Entry { ts: total, ..Entry::now(EVENT_LAUNCH) });

        let all = read_all();
        assert_eq!(all.len(), KEEP_LINES + 1, "the newest KEEP_LINES survive, plus the new one");
        assert_eq!(all[0].ts, total - KEEP_LINES as i64, "the OLDEST lines are what got dropped");
        assert_eq!(all.last().unwrap().event, EVENT_LAUNCH);
        // Every surviving line is still a whole, parseable line.
        let raw = std::fs::read_to_string(path()).unwrap();
        assert!(raw.ends_with('\n'), "a trimmed journal still ends with a newline");
        assert_eq!(raw.lines().count(), KEEP_LINES + 1, "no half line was left behind");

        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }

    #[test]
    fn reading_a_journal_that_does_not_exist_is_empty_not_an_error() {
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let _tmp = temp_home();
        assert!(read_all().is_empty());
        assert!(tail(5).is_empty());
        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }
}
