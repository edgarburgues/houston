//! Houston's view of Claude Code's OWN per-account `settings.json` — the file
//! Claude reads, which Houston edits surgically (`jsonedit`) and never rewrites.
//!
//! Two concerns live here today, both discovered by reading Claude's settings
//! schema rather than by anything going wrong:
//!
//! - **`statusLine.refreshInterval`.** Status-line updates are event-driven, so
//!   they "can go quiet when the session is idle" — but quota changes on its own:
//!   a window resets whether or not you typed. Without an interval an idle
//!   session displays a figure that is silently wrong until the next keystroke.
//! - **`cleanupPeriodDays`.** Claude deletes transcripts older than this at
//!   startup (default 30). Houston's entire value is the history in those files,
//!   and because `projects` is SHARED across accounts, the account with the
//!   smallest period decides what survives for all of them.
//!
//! Everything here reports before it touches anything, and retention is only
//! ever reported — deleting less history is Houston's business, deleting more is
//! the user's decision.

use crate::accounts::Account;
use crate::jsonedit;
use serde_json::Value;
use std::path::PathBuf;

/// The command Houston installs. Kept as one literal so the writer and the
/// "is this ours?" reader cannot drift apart.
pub const STATUSLINE_COMMAND: &str = "houston statusline";

/// Seconds between forced status-line renders. Claude's schema:
/// *"Re-run the status line command every N seconds in addition to event-driven
/// updates"* (minimum 1). 60 matches the usage cache's TTL — a shorter interval
/// would re-render values that cannot have changed, a longer one would let a
/// refreshed number sit unseen in the cache.
pub const REFRESH_INTERVAL_SECS: u64 = 60;

/// Claude's default retention when `cleanupPeriodDays` is unset.
pub const DEFAULT_CLEANUP_DAYS: i64 = 30;

fn settings_path(a: &Account) -> Option<PathBuf> {
    Some(a.resolve_config_dir()?.join("settings.json"))
}

// ------------------------------------------------------------- status line --

/// What an account's `settings.json` says about the status line.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum StatusLine {
    /// No account dir or no `settings.json` yet.
    NoSettings,
    /// The file exists but cannot be parsed — Houston must not patch it blind.
    Unreadable(String),
    /// Valid settings with no `statusLine`: Claude shows its own default line.
    Absent,
    /// Someone else's status line. Never touched.
    Foreign(String),
    /// Houston's line, with the refresh interval it declares (None = only
    /// event-driven updates, so an idle session goes stale).
    Ours { refresh: Option<u64> },
}

impl std::fmt::Display for StatusLine {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            StatusLine::NoSettings => write!(f, "no settings.json (doctor --fix seeds one)"),
            StatusLine::Unreadable(e) => write!(f, "settings.json unreadable: {e}"),
            StatusLine::Absent => write!(f, "no statusLine — Houston's line is not installed"),
            StatusLine::Foreign(c) => write!(f, "a non-Houston statusLine ({c}) — left alone"),
            StatusLine::Ours { refresh: None } => {
                write!(f, "houston statusline, event-driven only (goes stale while idle)")
            }
            StatusLine::Ours { refresh: Some(n) } => write!(f, "houston statusline, every {n}s"),
        }
    }
}

impl StatusLine {
    /// Whether `ensure_statusline` would change anything.
    pub fn needs_fix(&self) -> bool {
        matches!(self, StatusLine::Absent | StatusLine::Ours { refresh: None })
    }
}

/// Whether a `statusLine.command` is Houston's, tolerating an absolute path, a
/// `.exe` suffix, quoting and extra arguments. Deliberately generous: a false
/// "foreign" would leave a stale line unfixed, while a false "ours" could only
/// add a refresh interval to a command that mentions both words.
fn is_ours(cmd: &str) -> bool {
    let c = cmd.to_ascii_lowercase();
    c.contains("houston") && c.contains("statusline")
}

/// Read the status-line state without writing anything.
pub fn statusline_state(a: &Account) -> StatusLine {
    let Some(p) = settings_path(a) else { return StatusLine::NoSettings };
    if !p.is_file() {
        return StatusLine::NoSettings;
    }
    let obj = match jsonedit::read_obj(&p) {
        Ok(o) => o,
        Err(e) => return StatusLine::Unreadable(e.to_string()),
    };
    match obj.get("statusLine") {
        None | Some(Value::Null) => StatusLine::Absent,
        Some(Value::Object(sl)) => {
            let cmd = sl.get("command").and_then(Value::as_str).unwrap_or_default();
            if !is_ours(cmd) {
                return StatusLine::Foreign(if cmd.is_empty() { "no command".into() } else { cmd.to_string() });
            }
            StatusLine::Ours { refresh: sl.get("refreshInterval").and_then(Value::as_u64) }
        }
        Some(_) => StatusLine::Unreadable("statusLine is not an object".into()),
    }
}

/// Install Houston's status line, or add the missing refresh interval to the one
/// already there. Returns what changed, or `None` when nothing needed to.
///
/// Never overrides a `refreshInterval` that is already set (a value is a
/// decision, even a slow one) and never replaces a foreign status line — the
/// state is reported instead, because silently taking over the line of a tool
/// the user chose is worse than leaving Houston's off.
pub fn ensure_statusline(a: &Account) -> std::io::Result<Option<String>> {
    let state = statusline_state(a);
    if !state.needs_fix() {
        return Ok(None);
    }
    let Some(p) = settings_path(a) else { return Ok(None) };
    let mut what = String::new();
    // create: false — a missing settings.json is heal/fix's business (it seeds
    // the shared one), and writing a file here that holds nothing but a status
    // line would make that seeding a merge conflict.
    jsonedit::patch(&p, false, |obj| {
        let mut sl = jsonedit::sub_object(obj, "statusLine")?;
        if sl.is_empty() {
            sl.insert("type".into(), Value::from("command"));
            sl.insert("command".into(), Value::from(STATUSLINE_COMMAND));
            what = format!("statusLine installed ({STATUSLINE_COMMAND}, every {REFRESH_INTERVAL_SECS}s)");
        } else {
            what = format!("statusLine refreshInterval set to {REFRESH_INTERVAL_SECS}s");
        }
        sl.insert("refreshInterval".into(), Value::from(REFRESH_INTERVAL_SECS));
        jsonedit::set_sub_object(obj, "statusLine", sl);
        Ok(())
    })?;
    Ok(Some(what))
}

// --------------------------------------------------------------- retention --

/// One account's `cleanupPeriodDays`: `None` when unset (Claude then uses
/// `DEFAULT_CLEANUP_DAYS`).
pub fn cleanup_period_days(a: &Account) -> Option<i64> {
    match cleanup_period(a) {
        Period::Set(d) => Some(d),
        _ => None,
    }
}

/// What one account contributes to the retention of the shared history.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Period {
    /// An explicit `cleanupPeriodDays`.
    Set(i64),
    /// No value: Claude applies its default when a session in this account
    /// sweeps.
    Unset,
    /// This account's `settings.json` cannot be read or parsed, so Claude
    /// **pauses** the sweep for its sessions rather than falling back to the
    /// default — it deletes nothing, at any period. (Claude's own name for it is
    /// the `settings_unknowable` skip reason: a period it cannot see might be
    /// higher than the default, and guessing low would delete data the setting
    /// was meant to keep.)
    ///
    /// The distinction matters because Houston used to read this as `Unset` and
    /// therefore as "30 days" — blaming a broken account for a limit it never
    /// enforces, and quietly hiding the actual problem, which is the broken file.
    SweepPaused(String),
}

/// Read one account's contribution, distinguishing "says nothing" from "cannot
/// be read".
///
/// A missing `settings.json` is `Unset`, not paused: Claude reads the merged
/// settings and applies its default. Only a file that exists and does not parse
/// stops the sweep.
///
/// Not detected here: Claude also pauses on `settings_invalid_key_set` —
/// settings that parse but fail *its* schema while `cleanupPeriodDays` is set.
/// Houston does not own that schema and will not guess at it; `claude doctor` is
/// the thing that knows.
pub fn cleanup_period(a: &Account) -> Period {
    let Some(p) = settings_path(a) else { return Period::Unset };
    match jsonedit::read_obj(&p) {
        Ok(obj) => match obj.get("cleanupPeriodDays").and_then(|v| v.as_i64()) {
            Some(d) => Period::Set(d),
            None => Period::Unset,
        },
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Period::Unset,
        Err(e) => Period::SweepPaused(e.to_string()),
    }
}

/// The retention that actually applies to the shared history.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Retention {
    /// Effective period in days: the smallest across accounts, because they all
    /// start against the same shared `projects` dir and whichever runs first
    /// deletes for everybody.
    pub days: i64,
    /// Whether the account that decided `days` set the value, as opposed to
    /// inheriting Claude's default.
    pub explicit: bool,
    /// The account whose period governs.
    pub decided_by: Option<String>,
    /// How many accounts say nothing (and therefore apply the default).
    pub unset: usize,
    pub accounts: usize,
    /// Accounts whose `settings.json` cannot be parsed. Claude pauses the sweep
    /// for their sessions, so they delete nothing and take no part in the fold —
    /// but a broken settings file is a problem in its own right, and one that
    /// silently disables the status line and hooks too, so it is carried out of
    /// here rather than swallowed.
    pub paused: Vec<String>,
    /// Whether the fold includes `~/.claude` — the default config dir, which is
    /// not a Houston account but sweeps its own `projects/` at its own period.
    pub includes_default: bool,
}

/// The id given to the default config dir in the fold. Not a valid account id —
/// `accounts` never contains a path — so a report naming it cannot be mistaken
/// for one of Houston's.
const DEFAULT_SCOPE_ID: &str = "~/.claude";

/// How a fold participant is named in a report. The default config dir is not an
/// account and must not be printed as `account-~/.claude`.
fn scope_label(a: &Account) -> String {
    if a.id == DEFAULT_SCOPE_ID { a.id.clone() } else { format!("account-{}", a.id) }
}

/// `~/.claude` as a fold participant, when it holds transcripts of its own.
///
/// Not an account, and given an id that could never collide with one so a report
/// naming it cannot be mistaken for one. Returns None when the directory has no
/// `projects/` — then it governs nothing the history count includes, and adding it
/// would report a limit over an empty set.
fn default_config_scope() -> Option<Account> {
    if !default_scope_allowed() {
        return None;
    }
    let dir = crate::paths::home().join(".claude");
    dir.join("projects").is_dir().then(|| Account {
        id: DEFAULT_SCOPE_ID.into(),
        config_dir: dir.to_string_lossy().into_owned(),
        ..Default::default()
    })
}

/// Whether the ambient `~/.claude` participant may be discovered at all.
///
/// `default_config_scope` is the only function here that reaches a path nobody
/// passed in, and it has now written to the real `~/.claude/settings.json` from
/// a test **twice**. The first time a houston-core unit test did it: the
/// fixtures said 3650 days and the write landed on this machine, which is why
/// the `_of` entry points exist and why a `cfg!(test)` guard was added.
///
/// The second time is why this function exists. `cfg!(test)` is evaluated when
/// **this crate** is compiled, so it is false for every OTHER crate's tests —
/// they link houston-core as an ordinary dependency. A houston-tui test that
/// pressed the retention row therefore went straight through the guard and
/// deleted a `cleanupPeriodDays` somebody had set with
/// `houston retention --keep 1000`.
///
/// So the guard is a RUNTIME one that any crate's harness can set, and
/// houston-tui's test setup sets it next to `HOUSTON_HOME` — the same place
/// that already makes forgetting harmless there.
///
/// Failing closed is cheap: skipping the participant under-reports one config
/// dir, while including it wrongly edits a file the caller never named.
fn default_scope_allowed() -> bool {
    !cfg!(test) && scope_env_allows(std::env::var("HOUSTON_DEFAULT_SCOPE").ok().as_deref())
}

/// The `$HOUSTON_DEFAULT_SCOPE` half of the guard, kept pure so it can be
/// tested. The `cfg!(test)` half cannot be: inside this crate's tests it is
/// false by construction, which is precisely the property that made it useless
/// for every other crate.
fn scope_env_allows(v: Option<&str>) -> bool {
    match v {
        Some(v) => !matches!(v.trim().to_ascii_lowercase().as_str(), "0" | "off" | "no" | "false"),
        None => true,
    }
}

impl Retention {
    /// How many accounts actually run the sweep. Zero means the history is not
    /// being cleaned at all right now — by accident, not by decision.
    pub fn sweepers(&self) -> usize {
        self.accounts - self.paused.len()
    }
}

/// Fold every account's setting into the one that governs the shared history.
///
/// An account that says nothing is **not** abstaining: Claude applies its
/// 30-day default when *that* account starts, against the same shared dir. So
/// one generous account cannot protect the history — the effective period is the
/// minimum over accounts of "its value, or the default".
///
/// The one account that genuinely abstains is one Claude **cannot read**: it
/// pauses the sweep instead of guessing, so it deletes nothing. Folding it in as
/// "the default" would report a limit nobody enforces.
/// The default config dir joins the fold when it holds transcripts of its own,
/// because `history` counts them: `scan::project_roots` includes
/// `~/.claude/projects`, and on this machine that is NOT a junction onto the
/// shared store — it is a separate set of conversations, from anything that ran
/// `claude` without a `CLAUDE_CONFIG_DIR`. Folding only the accounts reported
/// "3 of 3 accounts" over a figure that spanned four settings files, one of which
/// nobody had read.
pub fn retention(accs: &[Account]) -> Retention {
    retention_of(accs, default_config_scope())
}

/// The fold itself, over an explicit participant list.
///
/// Split out so discovering `~/.claude` stays a separate concern from folding:
/// the discovery touches the real filesystem, and a fold that did it inline made
/// every test depend on whether the machine running them happens to have a
/// default config dir — which is precisely how it failed the first time.
fn retention_of(accs: &[Account], default_dir: Option<Account>) -> Retention {
    let participants: Vec<&Account> = accs.iter().chain(default_dir.iter()).collect();
    let mut out = Retention {
        days: DEFAULT_CLEANUP_DAYS,
        explicit: false,
        decided_by: None,
        unset: 0,
        accounts: participants.len(),
        paused: Vec::new(),
        includes_default: default_dir.is_some(),
    };
    let mut first = true;
    for a in participants {
        let set = match cleanup_period(a) {
            Period::Set(d) => Some(d),
            Period::Unset => None,
            Period::SweepPaused(why) => {
                out.paused.push(format!("{}: {why}", scope_label(a)));
                continue;
            }
        };
        if set.is_none() {
            out.unset += 1;
        }
        let days = set.unwrap_or(DEFAULT_CLEANUP_DAYS);
        if first || days < out.days {
            out.days = days;
            out.explicit = set.is_some();
            out.decided_by = Some(scope_label(a));
            first = false;
        }
    }
    out
}

/// The history on disk, as a retention question.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct History {
    /// Distinct conversations — `<root>/<project>/<id>.jsonl` and nothing
    /// deeper. Subagent transcripts live in `<id>/subagents/…` and belong to the
    /// conversation above them, so counting them would inflate the figure by an
    /// order of magnitude (measured here: 49 conversations, 426 subagent files)
    /// and say nothing extra about retention.
    ///
    /// Deduped across roots: the per-account `projects` dirs are junctions onto
    /// the same shared files, so a plain count would multiply by the number of
    /// accounts.
    pub files: usize,
    /// Age in days of the least recently modified conversation.
    pub oldest_days: i64,
    /// How many are older than the period asked about.
    pub at_risk: usize,
    /// The same profile per root, because **retention is per root, not global**.
    /// Each config dir sweeps its own `projects/`, and a root nobody opens is
    /// never swept at all — so one aggregate age can describe two stores in
    /// opposite situations and reassure you about the wrong one. See
    /// `retention_notice`, whose earlier version did exactly that.
    pub roots: Vec<RootHistory>,
}

/// One transcript root's age profile.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RootHistory {
    /// The root as given, for naming it in a report.
    pub path: String,
    pub files: usize,
    pub oldest_days: i64,
    /// Age of the most recent conversation — the field that says whether anything
    /// still WRITES here. A dormant root cannot be swept, because the sweep runs
    /// when a session in that config dir starts.
    pub newest_days: i64,
    pub at_risk: usize,
}

impl RootHistory {
    /// Whether this root looks like it is being swept: something still writes to
    /// it, and nothing in it is older than the period.
    ///
    /// Not proof — a store younger than the period looks identical — which is why
    /// the report states the ages instead of only the verdict. But combined with a
    /// root that has existed far longer than its oldest file, it is the difference
    /// between "the sweep has not run" and "the sweep already took the evidence".
    pub fn looks_swept(&self, period_days: i64, live_within_days: i64) -> bool {
        self.files > 0 && self.newest_days <= live_within_days && self.oldest_days < period_days
    }

    /// Whether anything still writes here.
    pub fn dormant(&self, live_within_days: i64) -> bool {
        self.files > 0 && self.newest_days > live_within_days
    }
}

/// How recent the newest conversation must be for a root to count as live. A
/// fortnight: long enough to cover a holiday, short enough that a store nobody
/// has opened in months is not mistaken for one in use.
pub const LIVE_WITHIN_DAYS: i64 = 14;

/// Age-profile every conversation Houston can see against `period_days`.
///
/// Stat-only: no transcript is opened, so this stays cheap enough for `doctor`
/// to run unconditionally over hundreds of files.
pub fn history(period_days: i64) -> History {
    history_in(&crate::scan::project_roots(), period_days, std::time::SystemTime::now())
}

/// The walk itself, over given roots and against a given clock, so the dedup and
/// the age arithmetic are testable without a real store.
fn history_in(roots: &[PathBuf], period_days: i64, now: std::time::SystemTime) -> History {
    let mut seen = std::collections::HashSet::new();
    let mut out = History::default();
    for root in roots {
        let Ok(projects) = std::fs::read_dir(root) else { continue };
        // Per-root totals, kept beside the global ones. A root contributes only the
        // transcripts FIRST seen under it, so the junction copies of a shared store
        // land against the root that was walked first and are not double counted —
        // the same rule the global figure uses, applied consistently.
        let mut here = RootHistory { path: root.display().to_string(), newest_days: i64::MAX, ..Default::default() };
        for proj in projects.flatten() {
            if !proj.file_type().map(|t| t.is_dir()).unwrap_or(false) {
                continue;
            }
            let Ok(files) = std::fs::read_dir(proj.path()) else { continue };
            for f in files.flatten() {
                let path = f.path();
                if path.extension().and_then(|e| e.to_str()) != Some("jsonl") {
                    continue;
                }
                // Logical key, like the scanner: junction traversal makes
                // canonicalized paths unreliable on Windows, while project+file
                // identifies the same physical transcript under every root.
                let key = (proj.file_name(), f.file_name());
                if !seen.insert(key) {
                    continue;
                }
                out.files += 1;
                here.files += 1;
                let Ok(days) = f.metadata().and_then(|m| m.modified()).map(|t| age_days(t, now)) else { continue };
                out.oldest_days = out.oldest_days.max(days);
                here.oldest_days = here.oldest_days.max(days);
                here.newest_days = here.newest_days.min(days);
                if days > period_days {
                    out.at_risk += 1;
                    here.at_risk += 1;
                }
            }
        }
        if here.files > 0 {
            // The sentinel never survives into a report: a root whose every
            // metadata read failed would otherwise claim its newest conversation
            // is i64::MAX days old, i.e. maximally dormant — the exact opposite of
            // what "unknown" should imply about whether it is being swept.
            if here.newest_days == i64::MAX {
                here.newest_days = 0;
            }
            out.roots.push(here);
        }
    }
    out
}

/// Whole days between `t` and `now`; a file from the future reads as 0 rather
/// than a negative age.
fn age_days(t: std::time::SystemTime, now: std::time::SystemTime) -> i64 {
    now.duration_since(t).map(|d| (d.as_secs() / 86_400) as i64).unwrap_or(0)
}

/// Set `cleanupPeriodDays` in every account, or remove it (`None`) to go back to
/// Claude's default.
///
/// Kept here rather than in `policy` on purpose: `policy` propagates keys Houston
/// is willing to own, and retention is explicitly NOT one of them — Houston must
/// never change it as a side effect of tidying, because raising it keeps data the
/// user may want expired and lowering it deletes history. This function exists so
/// a person can say "keep it all", and it is only ever called from something they
/// pressed.
/// It writes the **same set of config dirs the report folds** — accounts plus
/// `~/.claude` when that holds transcripts of its own. Anything narrower would
/// make the action fail to do what the report just advised: on this machine half
/// the conversations live under the default dir, so setting three accounts and
/// calling it done would leave 30 days governing 22 of them.
pub fn set_cleanup_period(accs: &[Account], days: Option<i64>) -> Vec<(String, Result<String, String>)> {
    let default_dir = default_config_scope();
    set_cleanup_period_of(&accs.iter().chain(default_dir.iter()).collect::<Vec<_>>(), days)
}

/// The write itself, over an explicit list — same split as `retention_of`, and for
/// the same reason: the tests must not depend on this machine having a `~/.claude`.
fn set_cleanup_period_of(accs: &[&Account], days: Option<i64>) -> Vec<(String, Result<String, String>)> {
    let mut out = Vec::new();
    for a in accs {
        let label = scope_label(a);
        let Some(p) = settings_path(a) else {
            out.push((label, Err("no config dir".into())));
            continue;
        };
        let before = cleanup_period_days(a);
        if before == days {
            out.push((label, Ok("already".into())));
            continue;
        }
        let res = jsonedit::patch(&p, false, |obj| {
            match days {
                // Claude rejects 0 and requires at least 1; a caller asking for
                // less is asking for something the setting cannot mean.
                Some(d) if d >= 1 => obj.insert("cleanupPeriodDays".into(), Value::from(d)),
                Some(_) => return Err(std::io::Error::other("cleanupPeriodDays must be at least 1")),
                None => obj.remove("cleanupPeriodDays"),
            };
            Ok(())
        });
        match res {
            Ok(()) => out.push((
                label,
                Ok(match (before, days) {
                    (_, Some(d)) => format!("{d} days"),
                    (_, None) => format!("unset (Claude's {DEFAULT_CLEANUP_DAYS}-day default)"),
                }),
            )),
            Err(e) => out.push((label, Err(e.to_string()))),
        }
    }
    out
}

/// A one-line verdict for `doctor`, or `None` when retention needs no comment.
///
/// Reporting is the whole intervention: Houston never edits `cleanupPeriodDays`.
/// Raising it means keeping data the user may have chosen to expire, and
/// lowering it deletes history — neither belongs in an automatic fix.
pub fn retention_notice(r: &Retention, h: &History) -> Option<String> {
    // An unreadable settings.json is reported in EVERY branch below, including
    // the ones that would otherwise stay silent: a comfortable retention is no
    // reason to hide a file that also carries the status line and the hooks.
    let broken = (!r.paused.is_empty()).then(|| unreadable_line(r));
    if h.files == 0 {
        return broken;
    }
    // Nothing sweeps: the history is safe by accident, which is worth more alarm
    // than a period would be. Say what to fix, not how long files live.
    if r.sweepers() == 0 {
        return Some(format!(
            "retention: NO account can be read, so Claude pauses its cleanup sweep entirely and \
             deletes nothing — {} conversations, oldest {} days, kept by an error rather than a \
             decision. {}",
            h.files,
            h.oldest_days,
            unreadable_line(r)
        ));
    }
    // "config dirs", not "accounts": the fold spans Houston's accounts AND
    // `~/.claude` when that holds transcripts of its own, and the count above
    // includes both. Calling them accounts is how the report came to describe
    // three files while governing four.
    let noun = if r.includes_default { "config dirs (accounts + ~/.claude)" } else { "accounts" };
    // The advice has to name the same set the fold does, or following it would not
    // work: on a machine where half the history sits under `~/.claude`, "set it in
    // every account" leaves the default period governing that half.
    let noun_short = if r.includes_default { "config dir's" } else { "account's" };
    let src = match (&r.decided_by, r.explicit) {
        (Some(id), true) => format!("the strictest cleanupPeriodDays is {} days ({id})", r.days),
        _ => format!(
            "{} of {} {noun} leave cleanupPeriodDays unset, so Claude's default of {} days governs",
            r.unset, r.accounts, r.days
        ),
    };
    if h.at_risk == 0 {
        // Still worth one line when it is only the default holding: the span is
        // what tells you whether the default is a real ceiling or academic.
        let heads_up = (!r.explicit && h.oldest_days > r.days / 2).then(|| {
            format!(
                "retention {src}; oldest of {} conversations is {} days — nothing over the limit yet",
                h.files, h.oldest_days
            )
        });
        return match (heads_up, broken) {
            (Some(a), Some(b)) => Some(format!("{a}. {b}")),
            (Some(a), None) => Some(a),
            (None, b) => b,
        };
    }
    let mut msg = format!(
        "retention {src}, and {} of {} conversations are older than that (oldest {} days). \
         To keep them, set \"cleanupPeriodDays\" high (3650 ≈ 10 years) in EVERY {noun_short} \
         settings.json — one left at the default is enough to delete them for all.",
        h.at_risk, h.files, h.oldest_days
    );
    // The part this notice used to get backwards. It said the survivors proved the
    // sweep had not run, which is reassurance drawn from the wrong root: a store
    // nobody opens is never swept, because the sweep runs when a session in that
    // config dir starts. So the ages have to be read per root, and the alarming
    // case is a LIVE root with nothing left older than the period.
    msg.push_str(&sweep_evidence(r, h));
    // What is NOT at stake, said explicitly: the number above counts transcripts,
    // and the auto-memory directory beside them is excluded from the sweep. Left
    // unsaid, the figure reads as "your notes are expiring too", which would make
    // the decision look more frightening than it is.
    msg.push_str(&format!(
        " Auto memory is not in that count: Claude excludes projects/<project>/{MEMORY_DIR}/ from \
         the sweep (since {MEMORY_EXCLUDED_SINCE}) and removes the directory only after it has been \
         empty for the whole period."
    ));
    if let Some(b) = broken {
        msg.push(' ');
        msg.push_str(&b);
    }
    Some(msg)
}

/// The auto-memory directory Claude keeps beside the transcripts of a project.
const MEMORY_DIR: &str = "memory";
/// The release that stopped the sweep from reaching inside it. Named so the claim
/// above carries its own evidence, and so `compat` has something to check.
const MEMORY_EXCLUDED_SINCE: &str = "claude 2.1.228";

/// What the per-root ages actually say about whether the sweep is running.
///
/// The distinction that matters, and that a single aggregate age hides: a root
/// still being written to whose oldest conversation sits just under the period has
/// almost certainly been swept — the older ones are simply gone. A dormant root
/// keeps everything, however old, because nothing there ever starts a session for
/// the sweep to run in. Reported as observation plus reading, in that order, so the
/// numbers can be checked against the conclusion.
fn sweep_evidence(r: &Retention, h: &History) -> String {
    let mut out = String::new();
    for root in &h.roots {
        if root.looks_swept(r.days, LIVE_WITHIN_DAYS) {
            out.push_str(&format!(
                " {} is LIVE ({} conversations, newest {}d) and holds nothing older than {} days \
                 against a {}-day limit — that is what a sweep that already ran looks like, not a \
                 store that happens to be young.",
                root.path, root.files, root.newest_days, root.oldest_days, r.days
            ));
        } else if root.dormant(LIVE_WITHIN_DAYS) && root.at_risk > 0 {
            out.push_str(&format!(
                " {} is DORMANT (newest {}d) and still holds {} of its {} past the limit: nothing \
                 opens a session there, so its sweep never runs. Those survive by disuse, not by \
                 the setting.",
                root.path, root.newest_days, root.at_risk, root.files
            ));
        }
    }
    out
}

/// The line about accounts Claude cannot read. Shared so the three callers say
/// exactly the same thing.
fn unreadable_line(r: &Retention) -> String {
    format!(
        "{} of {} accounts have an unreadable settings.json, which pauses Claude's cleanup sweep \
         for their sessions AND silently disables the status line and hooks there — fix those \
         first: {}",
        r.paused.len(),
        r.accounts,
        r.paused.join("; ")
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::Path;

    fn acct(dir: &Path) -> Account {
        Account { id: "x".into(), config_dir: dir.to_string_lossy().into_owned(), ..Default::default() }
    }

    fn write(dir: &Path, body: &str) {
        std::fs::write(dir.join("settings.json"), body).unwrap();
    }

    /// An account with its own config dir under `base`, for the folds that need
    /// several accounts disagreeing with each other.
    fn acc_at(base: &Path, id: &str) -> Account {
        let d = base.join(id);
        std::fs::create_dir_all(&d).unwrap();
        Account { id: id.into(), config_dir: d.to_string_lossy().into_owned(), ..Default::default() }
    }

    /// Borrow a fixture list for the `_of` entry points. They take `&[&Account]`
    /// so the caller states the participants, which is what keeps a test off this
    /// machine's real `~/.claude` — a lesson learned the expensive way.
    fn refs(v: &[Account]) -> Vec<&Account> {
        v.iter().collect()
    }

    fn write_acc(a: &Account, body: &str) {
        write(Path::new(&a.config_dir), body);
    }

    #[test]
    fn statusline_states_are_distinguished() {
        let tmp = tempfile::tempdir().unwrap();
        let d = tmp.path();
        let a = acct(d);
        // No file at all.
        assert_eq!(statusline_state(&a), StatusLine::NoSettings);
        // Valid settings, no statusLine.
        write(d, r#"{"theme":"dark"}"#);
        assert_eq!(statusline_state(&a), StatusLine::Absent);
        // Ours, event-driven only — the case that goes stale while idle.
        write(d, r#"{"statusLine":{"type":"command","command":"houston statusline"}}"#);
        assert_eq!(statusline_state(&a), StatusLine::Ours { refresh: None });
        // Ours via an absolute path with .exe, and already timed.
        write(
            d,
            r#"{"statusLine":{"type":"command","command":"C:\\Users\\x\\.local\\bin\\Houston.EXE statusline","refreshInterval":30}}"#,
        );
        assert_eq!(statusline_state(&a), StatusLine::Ours { refresh: Some(30) });
        // Somebody else's line.
        write(d, r#"{"statusLine":{"type":"command","command":"starship prompt"}}"#);
        assert!(matches!(statusline_state(&a), StatusLine::Foreign(_)));
        // Unparseable: must not be mistaken for "absent" and patched blind.
        write(d, "{not json");
        assert!(matches!(statusline_state(&a), StatusLine::Unreadable(_)));
    }

    #[test]
    fn ensure_statusline_adds_the_interval_and_keeps_everything_else() {
        let tmp = tempfile::tempdir().unwrap();
        let d = tmp.path();
        let a = acct(d);
        write(d, r#"{"theme":"dark","statusLine":{"type":"command","command":"houston statusline"}}"#);
        let msg = ensure_statusline(&a).unwrap().expect("an event-only line needs fixing");
        assert!(msg.contains("refreshInterval"), "{msg}");
        let back = jsonedit::read_obj(&d.join("settings.json")).unwrap();
        assert_eq!(back["statusLine"]["refreshInterval"], 60);
        assert_eq!(back["statusLine"]["command"], "houston statusline", "the command is not rewritten");
        assert_eq!(back["theme"], "dark", "unrelated settings survive");
        // Idempotent: a second pass has nothing to do.
        assert!(ensure_statusline(&a).unwrap().is_none());
    }

    #[test]
    fn ensure_statusline_installs_the_whole_block_but_never_hijacks_one() {
        let tmp = tempfile::tempdir().unwrap();
        let d = tmp.path();
        let a = acct(d);
        write(d, r#"{"model":"opus"}"#);
        assert!(ensure_statusline(&a).unwrap().unwrap().contains("installed"));
        let back = jsonedit::read_obj(&d.join("settings.json")).unwrap();
        assert_eq!(back["statusLine"]["type"], "command");
        assert_eq!(back["statusLine"]["command"], STATUSLINE_COMMAND);
        assert_eq!(back["statusLine"]["refreshInterval"], 60);

        // A foreign line is reported, never replaced.
        write(d, r#"{"statusLine":{"type":"command","command":"starship prompt"}}"#);
        assert!(ensure_statusline(&a).unwrap().is_none());
        let back = jsonedit::read_obj(&d.join("settings.json")).unwrap();
        assert_eq!(back["statusLine"]["command"], "starship prompt");
        assert!(back["statusLine"].get("refreshInterval").is_none());

        // Neither is a file Houston cannot parse.
        write(d, "{oops");
        assert!(ensure_statusline(&a).unwrap().is_none());
        assert_eq!(std::fs::read_to_string(d.join("settings.json")).unwrap(), "{oops");
    }

    /// `projects` is shared, so the strictest account governs everyone's history.
    #[test]
    fn retention_is_the_minimum_across_accounts() {
        let tmp = tempfile::tempdir().unwrap();
        let mk = |id: &str, body: Option<&str>| {
            let d = tmp.path().join(id);
            std::fs::create_dir_all(&d).unwrap();
            if let Some(b) = body {
                write(&d, b);
            }
            Account { id: id.into(), config_dir: d.to_string_lossy().into_owned(), ..Default::default() }
        };
        let unset = mk("a", Some(r#"{"theme":"dark"}"#));
        let generous = mk("b", Some(r#"{"cleanupPeriodDays":3650}"#));
        let strict = mk("c", Some(r#"{"cleanupPeriodDays":7}"#));

        // Nothing set anywhere → the default, and flagged as not chosen.
        let r = retention_of(std::slice::from_ref(&unset), None);
        assert_eq!((r.days, r.explicit, r.unset), (DEFAULT_CLEANUP_DAYS, false, 1));
        // ONE generous account cannot protect the shared history: the unset one
        // still applies Claude's 30-day default to the same files, so 30 governs.
        let r = retention_of(&[unset.clone(), generous.clone()], None);
        assert_eq!((r.days, r.explicit), (DEFAULT_CLEANUP_DAYS, false));
        assert_eq!(r.decided_by.as_deref(), Some("account-a"), "the account whose default decides is named");
        // Only when every account is generous does the generous value govern.
        let r = retention_of(std::slice::from_ref(&generous), None);
        assert_eq!((r.days, r.explicit, r.unset), (3650, true, 0));
        // The strictest wins wherever it sits in the list.
        assert_eq!(retention_of(&[strict.clone(), generous.clone()], None).days, 7);
        assert_eq!(retention_of(&[generous, strict], None).days, 7);
        // No accounts at all: the default, said without naming anyone.
        let r = retention_of(&[], None);
        assert_eq!((r.days, r.accounts, r.decided_by), (DEFAULT_CLEANUP_DAYS, 0, None));
    }

    /// Houston never changes retention on its own, but a person must be able to
    /// say "keep it all" once.
    #[test]
    fn setting_the_retention_period_is_explicit_and_reversible() {
        let tmp = tempfile::tempdir().unwrap();
        let mk = |id: &str, body: &str| {
            let d = tmp.path().join(id);
            std::fs::create_dir_all(&d).unwrap();
            write(&d, body);
            Account { id: id.into(), config_dir: d.to_string_lossy().into_owned(), ..Default::default() }
        };
        let accs = vec![mk("a", r#"{"theme":"dark"}"#), mk("b", r#"{"cleanupPeriodDays":7}"#)];

        let res = set_cleanup_period_of(&refs(&accs), Some(3650));
        assert!(res.iter().all(|(_, r)| r.is_ok()), "{res:?}");
        let r = retention_of(&accs, None);
        assert_eq!((r.days, r.explicit, r.unset), (3650, true, 0));
        // Unrelated settings survive.
        assert_eq!(jsonedit::read_obj(&tmp.path().join("a/settings.json")).unwrap()["theme"], "dark");

        // Idempotent, and reversible back to Claude's default.
        assert!(set_cleanup_period_of(&refs(&accs), Some(3650)).iter().all(|(_, r)| r.as_deref() == Ok("already")));
        set_cleanup_period_of(&refs(&accs), None);
        let r = retention_of(&accs, None);
        assert_eq!((r.days, r.explicit, r.unset), (DEFAULT_CLEANUP_DAYS, false, 2));

        // A value the setting cannot mean is refused rather than written: Claude
        // rejects 0, and "delete everything immediately" is not something to let
        // through by accident.
        let bad = set_cleanup_period_of(&refs(&accs), Some(0));
        assert!(bad.iter().all(|(_, r)| r.is_err()), "{bad:?}");
        assert_eq!(cleanup_period_days(&accs[0]), None, "nothing was written");
    }

    /// The runtime half of the guard that keeps `default_config_scope` off the
    /// real `~/.claude` in OTHER crates' tests. Unset must stay permissive:
    /// this is a test-harness knob, not a feature, and a machine that never
    /// sets it has to keep folding its default config dir.
    #[test]
    fn the_default_scope_env_guard_reads_the_obvious_spellings() {
        assert!(scope_env_allows(None), "unset must not disable the fold");
        assert!(scope_env_allows(Some("1")));
        assert!(scope_env_allows(Some("")), "empty reads as unset, not as a refusal");
        for off in ["0", "off", "no", "false", " OFF ", "False"] {
            assert!(!scope_env_allows(Some(off)), "{off:?} should turn it off");
        }
    }

    #[test]
    fn retention_notice_speaks_only_when_there_is_something_to_say() {
        let none = History::default();
        assert!(retention_notice(&retention_of(&[], None), &none).is_none(), "no history, no lecture");
        // Explicit and comfortable: silence.
        let young = History { files: 10, oldest_days: 3, at_risk: 0, ..Default::default() };
        let explicit = Retention {
            days: 3650,
            explicit: true,
            decided_by: Some("b".into()),
            unset: 0,
            accounts: 1,
            paused: Vec::new(),
            includes_default: false,
        };
        assert!(retention_notice(&explicit, &young).is_none());
        // Default, and history approaching it: one informational line.
        let default = Retention {
            days: 30,
            explicit: false,
            decided_by: Some("a".into()),
            unset: 3,
            accounts: 3,
            paused: Vec::new(),
            includes_default: false,
        };
        let approaching = History { files: 400, oldest_days: 25, at_risk: 0, ..Default::default() };
        let n = retention_notice(&default, &approaching).expect("worth a heads-up");
        assert!(n.contains("nothing over the limit yet"), "{n}");
        // Files already past the limit: name the number and how to keep them.
        let over = History { files: 49, oldest_days: 126, at_risk: 22, ..Default::default() };
        let n = retention_notice(&default, &over).unwrap();
        assert!(n.contains("22 of 49 conversations") && n.contains("cleanupPeriodDays"), "{n}");
        assert!(n.contains("EVERY account"), "one lax account deletes for all — say so: {n}");
        // Say what is NOT at risk. The count is transcripts; auto memory sits in
        // the same tree and is excluded, and leaving that unsaid makes the
        // decision look worse than it is.
        assert!(n.contains("Auto memory is not in that count"), "{n}");
    }

    #[test]
    fn an_unreadable_settings_file_pauses_the_sweep_instead_of_voting_for_the_default() {
        let tmp = tempfile::tempdir().unwrap();
        // One account keeps a decade, the other's file is corrupt.
        let keep = acc_at(tmp.path(), "keep");
        write_acc(&keep, r#"{"cleanupPeriodDays":3650}"#);
        let broken = acc_at(tmp.path(), "broken");
        write_acc(&broken, "{ this is not json");

        assert_eq!(cleanup_period(&keep), Period::Set(3650));
        assert!(matches!(cleanup_period(&broken), Period::SweepPaused(_)));

        let r = retention_of(&[keep, broken], None);
        // The old fold read the corrupt file as "unset" and therefore as 30 days,
        // reporting a limit that account cannot enforce — Claude pauses its sweep
        // rather than guessing low.
        assert_eq!(r.days, 3650, "the only account that can sweep decides");
        assert_eq!(r.unset, 0, "a file that cannot be read is not a file that says nothing");
        assert_eq!(r.paused.len(), 1);
        assert_eq!(r.sweepers(), 1);
        let n = retention_notice(&r, &History { files: 10, oldest_days: 200, at_risk: 0, ..Default::default() }).expect("the broken file is worth saying");
        assert!(n.contains("unreadable settings.json"), "{n}");
    }

    #[test]
    fn no_readable_account_means_nothing_is_deleted_at_all() {
        let tmp = tempfile::tempdir().unwrap();
        let a = acc_at(tmp.path(), "a");
        write_acc(&a, "nope");
        let r = retention_of(&[a], None);
        assert_eq!(r.sweepers(), 0);
        let n = retention_notice(&r, &History { files: 49, oldest_days: 126, at_risk: 22, ..Default::default() }).unwrap();
        // The history is safe by accident. That deserves alarm, not a period.
        assert!(n.contains("deletes nothing"), "{n}");
        assert!(n.contains("kept by an error rather than a decision"), "{n}");
    }

    /// `history` counts transcripts under EVERY root `scan::project_roots`
    /// returns, and that includes `~/.claude/projects` — which on this machine is
    /// not a junction onto the shared store but 22 conversations of its own, from
    /// anything that ran `claude` without a CLAUDE_CONFIG_DIR. The fold has to
    /// span the same set, or the report describes three files while governing four.
    #[test]
    fn the_default_config_dir_joins_the_fold_when_it_holds_transcripts() {
        let tmp = tempfile::tempdir().unwrap();
        let acct = acc_at(tmp.path(), "a");
        write_acc(&acct, r#"{"cleanupPeriodDays":3650}"#);
        // The default dir says nothing, so Claude's 30 days apply to ITS
        // transcripts — and 30 is therefore the shortest period in force.
        let default_dir = acc_at(tmp.path(), DEFAULT_SCOPE_ID);

        let alone = retention_of(std::slice::from_ref(&acct), None);
        assert_eq!((alone.days, alone.accounts, alone.includes_default), (3650, 1, false));

        let both = retention_of(&[acct], Some(default_dir));
        assert_eq!(both.days, DEFAULT_CLEANUP_DAYS, "the unset default dir is the strictest");
        assert_eq!(both.accounts, 2);
        assert!(both.includes_default);
        // Named as itself. "account-~/.claude" would be a lie about what it is.
        assert_eq!(both.decided_by.as_deref(), Some(DEFAULT_SCOPE_ID));
        let n = retention_notice(&both, &History { files: 44, oldest_days: 151, at_risk: 22, ..Default::default() }).unwrap();
        assert!(n.contains("config dirs (accounts + ~/.claude)"), "the noun must not say accounts: {n}");
    }

    /// A real scenario an earlier notice read backwards.
    ///
    /// Two roots: a LIVE one whose oldest conversation sits just under the limit,
    /// and a DORMANT one keeping everything however old. The notice used to say
    /// "they are still here, so the sweep has not run" — reassurance taken from
    /// the wrong root: the survivors sit in the dormant store, where nothing ever
    /// opens a session so its sweep never runs, while the live store had already
    /// lost weeks of history with a clean edge at 28d against a 30-day limit.
    #[test]
    fn the_notice_reads_ages_per_root_not_backwards() {
        let r = Retention {
            days: 30,
            explicit: false,
            decided_by: Some("account-a".into()),
            unset: 4,
            accounts: 4,
            paused: Vec::new(),
            includes_default: true,
        };
        let h = History {
            files: 44,
            oldest_days: 151,
            at_risk: 22,
            roots: vec![
                RootHistory { path: "~/.claude-shared/projects".into(), files: 22, oldest_days: 28, newest_days: 0, at_risk: 0 },
                RootHistory { path: "~/.claude/projects".into(), files: 22, oldest_days: 151, newest_days: 71, at_risk: 22 },
            ],
        };
        assert!(h.roots[0].looks_swept(r.days, LIVE_WITHIN_DAYS), "live, and nothing past the limit");
        assert!(!h.roots[0].dormant(LIVE_WITHIN_DAYS));
        assert!(h.roots[1].dormant(LIVE_WITHIN_DAYS), "71 days without a write is not a store in use");
        assert!(!h.roots[1].looks_swept(r.days, LIVE_WITHIN_DAYS));

        let n = retention_notice(&r, &h).expect("22 past the limit deserve a notice");
        assert!(n.contains("is LIVE") && n.contains("what a sweep that already ran looks like"), "{n}");
        assert!(n.contains("is DORMANT") && n.contains("survive by disuse"), "{n}");
        // And what it must NOT say any more — the reassuring, false half.
        assert!(!n.contains("so it has not on this history"), "the notice regressed to the old reading: {n}");
    }

    #[test]
    fn a_genuinely_young_root_is_not_reported_as_swept() {
        // Same age profile as a swept store, different cause: a freshly created
        // one. That is why the report states the ages rather than only the
        // verdict — but the liveness threshold avoids the common false positive,
        // the root nobody touches.
        let fresh = RootHistory { path: "new".into(), files: 3, oldest_days: 2, newest_days: 0, at_risk: 0 };
        assert!(fresh.looks_swept(30, LIVE_WITHIN_DAYS), "indistinguishable by age, as the doc says");
        let empty = RootHistory::default();
        assert!(!empty.looks_swept(30, LIVE_WITHIN_DAYS), "no files, nothing to conclude");
        assert!(!empty.dormant(LIVE_WITHIN_DAYS));
    }

    #[test]
    fn a_missing_settings_file_is_unset_not_paused() {
        // Claude reads the merged settings and applies its default; only a file
        // that exists and does not parse stops the sweep.
        let tmp = tempfile::tempdir().unwrap();
        let a = acc_at(tmp.path(), "fresh");
        assert_eq!(cleanup_period(&a), Period::Unset);
        let r = retention_of(&[a], None);
        assert_eq!(r.days, DEFAULT_CLEANUP_DAYS);
        assert_eq!(r.unset, 1);
        assert!(r.paused.is_empty());
    }

    #[test]
    fn history_dedups_across_roots_and_counts_conversations_only() {
        let tmp = tempfile::tempdir().unwrap();
        let mut roots = Vec::new();
        // The same project+file under two roots is what junctions produce: one
        // physical transcript seen twice must count once, or every figure in the
        // report is multiplied by the number of accounts.
        for root in ["r1", "r2"] {
            let p = tmp.path().join(root).join("projects").join("proj");
            std::fs::create_dir_all(&p).unwrap();
            std::fs::write(p.join("a.jsonl"), "{}").unwrap();
            std::fs::write(p.join("notes.md"), "not a transcript").unwrap();
            roots.push(tmp.path().join(root).join("projects"));
        }
        // A second conversation in another project, and a stray file at the root.
        let other = tmp.path().join("r1").join("projects").join("otherproj");
        std::fs::create_dir_all(&other).unwrap();
        std::fs::write(other.join("b.jsonl"), "{}").unwrap();
        std::fs::write(tmp.path().join("r1").join("projects").join("loose.jsonl"), "{}").unwrap();
        // Subagent transcripts belong to the conversation above them: counting
        // them would report ~10x the real number (measured: 49 vs 426 here).
        let subs = tmp.path().join("r1").join("projects").join("proj").join("a").join("subagents");
        std::fs::create_dir_all(subs.join("workflows").join("wf_1")).unwrap();
        std::fs::write(subs.join("agent-1.jsonl"), "{}").unwrap();
        std::fs::write(subs.join("workflows").join("wf_1").join("agent-2.jsonl"), "{}").unwrap();

        let now = std::time::SystemTime::now();
        let h = history_in(&roots, 30, now);
        assert_eq!(h.files, 2, "deduped; .md, subagents and a loose root file all ignored");
        assert_eq!(h.at_risk, 0, "files written now are not near any limit");
        // Against a clock a year ahead, everything is past a 30-day period.
        let later = now + std::time::Duration::from_secs(86_400 * 365);
        let h = history_in(&roots, 30, later);
        assert_eq!((h.files, h.at_risk), (2, 2));
        assert!(h.oldest_days >= 364);
        // A missing root is skipped rather than fatal.
        assert_eq!(history_in(&[tmp.path().join("nope")], 30, now), History::default());
    }

    #[test]
    fn age_days_floors_and_never_goes_negative() {
        let now = std::time::SystemTime::now();
        assert_eq!(age_days(now, now), 0);
        assert_eq!(age_days(now - std::time::Duration::from_secs(86_400 * 3 + 5), now), 3);
        // Clock skew (a file dated in the future) must not read as a negative age.
        assert_eq!(age_days(now + std::time::Duration::from_secs(3_600), now), 0);
    }
}
