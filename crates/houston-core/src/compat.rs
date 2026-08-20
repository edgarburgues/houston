//! What Houston assumes about Claude Code, and the version those assumptions
//! were last checked against.
//!
//! Houston is not a client of a versioned API. It parses `claude agents --json`,
//! consumes the status line's stdin payload, installs hooks by event name and
//! matcher, writes keys into Claude's own `settings.json`, passes launch flags,
//! and decodes the `projects/<encoded-cwd>` naming scheme. Not one of those is a
//! contract, and Claude Code **updates itself in the background** — so any of
//! them can stop being true because of a release nobody here asked for.
//!
//! What makes this worth a module rather than a comment is the failure mode:
//! nearly every one of these surfaces degrades **silently**.
//!
//! - A renamed field under `rate_limits` doesn't error; the line quietly falls
//!   back to a cached number, and looks exactly like a working line.
//! - A retired hook event installs fine and simply never fires. `hooks status`
//!   would still report it as ours.
//! - A changed key in `agents --json` turns every live session into no live
//!   session, which is indistinguishable from "nothing is running".
//! - A renamed settings key makes `policy sync` propagate a key Claude ignores.
//!
//! None of that shows up as a failure, which is why "it still works" is not
//! evidence. The defence is to write down what was verified and against which
//! version, and have `doctor` say when the installed binary has moved past it.
//!
//! This module deliberately does NOT try to detect the breakage itself. Probing
//! every surface would mean running Claude several times on every doctor run,
//! and a probe that guesses wrong is worse than a date-stamped list: it invites
//! trust. What it reports is drift — "you are N releases past the last check,
//! here is the list" — and the list is short enough to walk by hand.

use std::fmt;
use std::process::{Command, Stdio};
use std::time::Duration;

use crate::launch::claude_bin;

/// The Claude Code release every assumption below was verified against, by hand,
/// on a real binary.
///
/// Bump this ONLY after actually re-walking `ASSUMPTIONS` — its whole value is
/// that it means "checked", not "probably fine".
pub const VERIFIED_AGAINST: &str = "2.1.236";

/// How long to wait for `claude --version`.
///
/// Generous on purpose: this runs in `doctor`, which no one is watching a
/// keystroke on, and the alternative to waiting is reporting "unknown" — the one
/// answer that teaches nothing.
pub const VERSION_TIMEOUT: Duration = Duration::from_secs(10);

/// One thing Houston takes for granted about Claude Code.
pub struct Assumption {
    /// The surface, named the way the docs name it.
    pub surface: &'static str,
    /// What Houston relies on being true.
    pub assumption: &'static str,
    /// How to check it, concretely. A command where a command settles it.
    pub recheck: &'static str,
}

/// Everything Houston reads, writes or spawns that Claude Code owns.
///
/// Order is by blast radius: the ones at the top break the product, the ones at
/// the bottom degrade a display.
pub const ASSUMPTIONS: &[Assumption] = &[
    Assumption {
        surface: "projects/<encoded-cwd>/<id>.jsonl",
        assumption: "transcripts live under a per-cwd directory whose name is the path with every \
                     non-alphanumeric character replaced by '-', and `--resume` re-derives that key \
                     from the CURRENT cwd",
        recheck: "open a chat somewhere new and confirm a matching dir appears (see pathenc); \
                  CLAUDE_CODE_PROJECT_DIR_NAME overrides the name and breaks the decode",
    },
    Assumption {
        surface: "CLAUDE_CONFIG_DIR",
        assumption: "it moves settings, credentials, history and plugins wholesale, so one account \
                     per directory is a complete separation",
        recheck: "documented in the env-vars reference; `houston doctor` shows each account's state",
    },
    Assumption {
        surface: "claude agents --json",
        assumption: "prints an array of {pid, cwd, kind, startedAt, sessionId, name, status}, needs \
                     no TTY, and reports sessions from the shared `sessions/` registry — so asking \
                     through any account sees them all",
        recheck: "claude agents --json  (compare the keys against agents::Live)",
    },
    Assumption {
        surface: "status line stdin payload",
        assumption: "model.display_name, context_window.used_percentage, and rate_limits.{five_hour, \
                     seven_day}.{used_percentage, resets_at} — each of the rate_limits windows \
                     independently absent",
        recheck: "the status line reference's JSON input table; every field Houston reads is optional \
                  in code, so a rename degrades to the cache instead of erroring",
    },
    Assumption {
        surface: "statusLine settings block",
        assumption: "{type:\"command\", command, refreshInterval} with refreshInterval in SECONDS \
                     (minimum 1), rendered on its own row above the footer badges",
        recheck: "claude doctor, plus the status line reference",
    },
    Assumption {
        surface: "hook events and matchers",
        assumption: "SessionStart; StopFailure matched on `rate_limit` and `authentication_failed`; \
                     Notification matched on `auth_success`. StopFailure matchers are exact-match on \
                     letters/digits/_ with `|` the only separator",
        recheck: "open a chat, then `houston journal --tail 5` — a hook that stopped firing still \
                  reports as installed",
    },
    Assumption {
        surface: "launch flags",
        assumption: "--resume <id>, --fork-session, --model, --effort (low|medium|high|xhigh|max), \
                     --permission-mode (acceptEdits|auto|bypassPermissions|manual|dontAsk|plan), \
                     --worktree with an OPTIONAL value",
        recheck: "claude --help",
    },
    Assumption {
        surface: "cleanupPeriodDays",
        assumption: "default 30, minimum 1; sweeps transcripts under projects/ but EXCLUDES \
                     projects/<proj>/memory/ (since 2.1.228), and pauses entirely when a settings \
                     file cannot be read or parsed",
        recheck: "the .claude directory reference, \"Cleaned up automatically\"",
    },
    Assumption {
        surface: "settings.json keys Houston propagates",
        assumption: "the `policy` allowlist names keys Claude still reads, under those exact spellings",
        recheck: "the settings reference; note that Claude may REWRITE some keys to a canonical \
                  spelling when it updates the file (marketplace aliases do this)",
    },
];

/// A Claude Code version, compared the only way that is meaningful here: as
/// ordered numbers, not as a string.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub struct Version(pub u32, pub u32, pub u32);

impl Version {
    /// Parse a leading dotted triple, ignoring whatever follows.
    ///
    /// `claude --version` prints `2.1.236 (Claude Code)`, and the tail is not
    /// ours to depend on — a build suffix or a channel name appearing there must
    /// not turn a known version into an unknown one.
    pub fn parse(s: &str) -> Option<Self> {
        let head = s.split_whitespace().next()?;
        let mut it = head.split('.');
        let mut next = || it.next()?.parse::<u32>().ok();
        let (a, b, c) = (next()?, next()?, next()?);
        Some(Version(a, b, c))
    }
}

impl fmt::Display for Version {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}.{}.{}", self.0, self.1, self.2)
    }
}

/// The version in `VERIFIED_AGAINST`. Infallible in practice — a test pins it —
/// but returned as an Option so a typo cannot panic a doctor run.
pub fn verified() -> Option<Version> {
    Version::parse(VERIFIED_AGAINST)
}

/// Ask the installed binary what it is.
///
/// Returns None for every unhappy path (no claude on PATH, a timeout, output
/// that doesn't start with a version), because "I could not tell" and "it
/// drifted" are different reports and must not be merged.
pub fn installed() -> Option<Version> {
    let mut cmd = Command::new(claude_bin());
    cmd.arg("--version");
    // Same discipline as `agents::list`: this may run under a TUI that owns the
    // terminal, so nothing the child says reaches the screen.
    cmd.stdin(Stdio::null()).stdout(Stdio::piped()).stderr(Stdio::null());
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        cmd.creation_flags(CREATE_NO_WINDOW);
    }
    let child = cmd.spawn().ok()?;
    // Bound our wait, not the child's life — see agents::list for why killing it
    // is not worth a platform handle.
    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let _ = tx.send(child.wait_with_output().ok());
    });
    let out = rx.recv_timeout(VERSION_TIMEOUT).ok()??;
    Version::parse(&String::from_utf8_lossy(&out.stdout))
}

/// Where the installed binary stands relative to the last verification.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Drift {
    /// Could not read a version — say so rather than implying either verdict.
    Unknown,
    /// Exactly the release the assumptions were checked against.
    Verified,
    /// Newer. The assumptions above may have been invalidated by any release in
    /// between, silently.
    Ahead,
    /// Older than the checked release: a rollback, or a second install earlier on
    /// PATH. Assumptions verified on a NEWER binary may not hold on this one.
    Behind,
}

/// A drift report, ready to print.
#[derive(Debug, Clone)]
pub struct Report {
    pub verified: Option<Version>,
    pub installed: Option<Version>,
    pub drift: Drift,
}

impl Report {
    /// Whether a human should look at the assumption list.
    pub fn wants_attention(&self) -> bool {
        matches!(self.drift, Drift::Ahead | Drift::Behind | Drift::Unknown)
    }
}

/// Compare a known installed version against the verified one. Pure, so the
/// verdicts are tested without spawning anything.
pub fn compare(installed: Option<Version>) -> Report {
    let verified = verified();
    let drift = match (verified, installed) {
        (Some(v), Some(i)) if i == v => Drift::Verified,
        (Some(v), Some(i)) if i > v => Drift::Ahead,
        (Some(_), Some(_)) => Drift::Behind,
        _ => Drift::Unknown,
    };
    Report { verified, installed, drift }
}

/// Read the installed version and compare it. Costs one short subprocess.
pub fn check() -> Report {
    compare(installed())
}

impl fmt::Display for Report {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let v = self.verified.map(|v| v.to_string()).unwrap_or_else(|| VERIFIED_AGAINST.into());
        match (self.drift, self.installed) {
            (Drift::Verified, _) => write!(f, "claude {v} — the release Houston's assumptions were verified against"),
            (Drift::Ahead, Some(i)) => write!(
                f,
                "claude {i}, verified against {v} — {} assumptions unchecked on this release; they fail SILENTLY, so \"it works\" is not evidence",
                ASSUMPTIONS.len()
            ),
            (Drift::Behind, Some(i)) => write!(f, "claude {i} is OLDER than the verified {v} — a rollback, or another install earlier on PATH"),
            _ => write!(f, "could not read `claude --version` (verified against {v}) — is claude on PATH?"),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_verified_constant_parses() {
        // The whole module leans on this string. A typo here would silently turn
        // every drift verdict into "unknown".
        assert!(verified().is_some(), "VERIFIED_AGAINST is not a dotted triple: {VERIFIED_AGAINST}");
    }

    #[test]
    fn versions_compare_as_numbers_not_as_text() {
        // The bug this pins: "2.1.236" < "2.1.30" as strings, which would report
        // a newer binary as a rollback for the whole 2.1.9x → 2.1.2xx range.
        assert!(Version::parse("2.1.236").unwrap() > Version::parse("2.1.30").unwrap());
        assert!(Version::parse("2.1.9").unwrap() < Version::parse("2.1.100").unwrap());
        assert!(Version::parse("2.2.0").unwrap() > Version::parse("2.1.999").unwrap());
    }

    #[test]
    fn a_version_line_with_a_tail_still_parses() {
        assert_eq!(Version::parse("2.1.236 (Claude Code)"), Some(Version(2, 1, 236)));
        assert_eq!(Version::parse("  2.1.236\n"), Some(Version(2, 1, 236)));
        // Not a version: report unknown rather than guessing a number.
        assert_eq!(Version::parse(""), None);
        assert_eq!(Version::parse("claude 2.1.236"), None);
        assert_eq!(Version::parse("2.1"), None);
    }

    #[test]
    fn drift_distinguishes_unknown_from_both_directions() {
        let v = verified().unwrap();
        assert_eq!(compare(Some(v)).drift, Drift::Verified);
        assert_eq!(compare(Some(Version(v.0, v.1, v.2 + 1))).drift, Drift::Ahead);
        assert_eq!(compare(Some(Version(v.0, v.1, v.2.saturating_sub(1)))).drift, Drift::Behind);
        // "I could not tell" is its own answer, and must not read as verified.
        let unknown = compare(None);
        assert_eq!(unknown.drift, Drift::Unknown);
        assert!(unknown.wants_attention());
        assert!(!compare(Some(v)).wants_attention());
    }

    #[test]
    fn every_assumption_says_how_to_recheck_it() {
        // A list that names a surface without saying how to verify it is a list
        // nobody will walk. That is the only thing that makes this module useful.
        for a in ASSUMPTIONS {
            assert!(!a.surface.trim().is_empty());
            assert!(!a.assumption.trim().is_empty(), "{}", a.surface);
            assert!(!a.recheck.trim().is_empty(), "{} has no recheck", a.surface);
        }
    }

    #[test]
    fn the_report_names_both_versions() {
        let r = compare(Some(Version(9, 9, 9)));
        let s = r.to_string();
        assert!(s.contains("9.9.9") && s.contains(VERIFIED_AGAINST), "{s}");
    }
}
