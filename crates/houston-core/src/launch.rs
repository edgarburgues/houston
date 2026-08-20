//! Launching the real `claude`: locate the binary, pick an account, and build
//! a resume command. Shared by the `run` CLI verb and the TUI's `Enter` resume.
//!
//! Accounts share one projects/sessions store, so ANY logged-in account can
//! reopen ANY session, which makes the account a scheduling decision rather than
//! a property of the conversation. Resume takes that decision **from the usage
//! cache**: `Enter` is advertised as a one-keystroke resume, so it may not stall
//! on the network, and the cache is kept warm by the statusline anyway. Only when
//! the cache has no reading at all does resume probe, because at that point the
//! alternative is choosing blind.

use crate::accounts::{self, Account};
use crate::model::Launch;
use crate::usage::Pick;
use crate::{journal, paths, usage};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::Duration;

/// Route the child's link-opens through Houston: Claude Code honors `$BROWSER`
/// as the opener for every URL — the OAuth login page included — invoking it
/// with the URL as its only argument. With Houston as `$BROWSER`, login pages
/// open in a PRIVATE browser window (no inherited claude.ai session, which
/// matters when juggling several accounts) and every other link opens normally
/// (see `browse`). A `BROWSER` the user already set wins.
fn set_browser_self(cmd: &mut Command) {
    if std::env::var_os("BROWSER").is_some() {
        return;
    }
    if let Ok(exe) = std::env::current_exe() {
        cmd.env("BROWSER", exe);
    }
}

/// Locate the real `claude` executable: PATH first (honoring PATHEXT on
/// Windows), then ~/.local/bin, else the bare name.
pub fn claude_bin() -> PathBuf {
    if let Some(p) = which("claude") {
        return p;
    }
    let fb = paths::home().join(".local").join("bin").join("claude.exe");
    if fb.is_file() {
        return fb;
    }
    PathBuf::from("claude")
}

fn which(cmd: &str) -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    #[cfg(windows)]
    let exts: Vec<String> =
        std::env::var("PATHEXT").unwrap_or_else(|_| ".EXE;.CMD;.BAT".into()).split(';').map(|e| e.to_string()).collect();
    #[cfg(not(windows))]
    let exts: Vec<String> = vec![String::new()];
    for dir in std::env::split_paths(&path) {
        for ext in &exts {
            let cand = dir.join(format!("{cmd}{ext}"));
            if cand.is_file() {
                return Some(cand);
            }
        }
    }
    None
}

/// The Claude config dir that physically holds a transcript: the parent of the
/// ".../projects/..." segment. None if it can't tell (→ claude's default).
fn config_dir_of(transcript_path: &str) -> Option<PathBuf> {
    let p = transcript_path.replace('\\', "/");
    let i = p.rfind("/projects/")?;
    Some(PathBuf::from(&transcript_path[..i]))
}

/// A `claude --resume <session>` command in the mission's working dir (v1
/// behavior): the account is auto-balanced — the shared projects/ store lets any
/// logged-in account reopen any session, so we pick the lowest-pressure one, from
/// the cache when it can answer and by probing only when it cannot. With NO
/// account logged in, we fall back to the config dir that physically owns the
/// transcript.
///
/// Probing here used to be unconditional with a 4 s timeout, and a failed round
/// degraded to plain least-recently-used. That is how three consecutive resumes
/// could land on three different accounts — two of them at 100 % of the weekly
/// window — while the statusline was displaying that exact saturation: the
/// decision threw away the reading the screen was showing.
pub fn resume_command(session_id: &str, cwd: &str, transcript_path: &str, prefs: &Launch) -> Command {
    let accs = accounts::load().unwrap_or_default();
    let logged: Vec<Account> = accs.into_iter().filter(|a| a.logged_in()).collect();
    let (config_dir, account, how) = if !logged.is_empty() {
        let d = usage::best_cached(&logged).or_else(|| usage::best(&logged, Duration::from_secs(4)));
        match d {
            Some(d) => {
                let _ = accounts::touch_last_use(&d.account.id, &now_stamp());
                (d.account.resolve_config_dir(), d.account.id.clone(), d.how)
            }
            None => (config_dir_of(transcript_path), String::new(), Pick::TranscriptOwner),
        }
    } else {
        (config_dir_of(transcript_path), String::new(), Pick::TranscriptOwner)
    };

    // Record the decision, not just its effect. The account used to be
    // recoverable only by reading `lastUse` stamps afterwards, which is evidence
    // by accident rather than a record.
    journal::append(&journal::Entry {
        session: session_id.to_string(),
        cwd: cwd.to_string(),
        account: account.clone(),
        reason: how.label().to_string(),
        ..journal::Entry::now(journal::EVENT_RESUME)
    });

    let mut cmd = Command::new(claude_bin());
    cmd.arg("--resume").arg(session_id);
    cmd.args(args_for(prefs));
    if !cwd.is_empty() && Path::new(cwd).is_dir() {
        cmd.current_dir(cwd);
    }
    cmd.env_remove("CLAUDE_CONFIG_DIR");
    if let Some(dir) = config_dir {
        cmd.env("CLAUDE_CONFIG_DIR", dir);
    }
    set_browser_self(&mut cmd);
    cmd
}

/// Render per-mission launch preferences as claude arguments.
///
/// Order is fixed so a command line is reproducible and diffable, and `extra`
/// goes last: a flag Houston does not model yet must be able to override one it
/// does, which only works if it comes after.
pub fn args_for(p: &Launch) -> Vec<String> {
    let mut out: Vec<String> = Vec::new();
    let mut flag = |name: &str, value: &str| {
        if !value.is_empty() {
            out.push(name.to_string());
            out.push(value.to_string());
        }
    };
    flag("--model", &p.model);
    flag("--effort", &p.effort);
    flag("--permission-mode", &p.permission_mode);
    flag("--agent", &p.agent);
    for d in &p.add_dirs {
        if !d.is_empty() {
            out.push("--add-dir".into());
            out.push(d.clone());
        }
    }
    // The three worktree states are distinguishable here and nowhere else: no
    // flag, a bare flag, or a flag with a name.
    match &p.worktree {
        None => {}
        Some(name) if name.is_empty() => out.push("--worktree".into()),
        Some(name) => {
            out.push("--worktree".into());
            out.push(name.clone());
        }
    }
    if p.fork {
        out.push("--fork-session".into());
    }
    if p.safe_mode {
        out.push("--safe-mode".into());
    }
    out.extend(p.extra.iter().filter(|a| !a.is_empty()).cloned());
    out
}

/// Record that a NEW session was opened on an account.
///
/// Called by the real launch paths (`run`, the Accounts view) rather than from
/// inside `launch_command`, because `fleet` uses that same builder to run claude
/// for MCP and plugin work — which is not a session and has no business in a
/// journal of sessions. The session id is absent by necessity: it does not exist
/// until claude creates it.
pub fn record_launch(account_id: &str, how: Pick, cwd: &str) {
    journal::append(&journal::Entry {
        account: account_id.to_string(),
        cwd: cwd.to_string(),
        reason: how.label().to_string(),
        ..journal::Entry::now(journal::EVENT_LAUNCH)
    });
}

/// A `claude` command for a NEW session forced onto a specific account
/// (config_dir). Used by the Accounts view. Touches the account's last-use.
pub fn launch_command(account_id: &str, config_dir: Option<&Path>) -> Command {
    let mut cmd = Command::new(claude_bin());
    cmd.env_remove("CLAUDE_CONFIG_DIR");
    if let Some(dir) = config_dir {
        cmd.env("CLAUDE_CONFIG_DIR", dir);
    }
    cmd.env_remove(PROJECT_DIR_NAME_ENV);
    set_browser_self(&mut cmd);
    let _ = accounts::touch_last_use(account_id, &now_stamp());
    cmd
}

/// Claude's override for the per-project transcript directory name.
///
/// Houston strips it for the same reason it strips `CLAUDE_CONFIG_DIR`: a value
/// inherited from some outer wrapper silently relocates the transcript of every
/// session Houston launches, and Houston finds sessions by *decoding* that
/// directory name back into a cwd (see `pathenc`). A session written under a name
/// that is not an encoded path is a session Houston can list but cannot resume —
/// `--resume` re-derives the key from the cwd, and there is no cwd that produces
/// an arbitrary name.
///
/// Houston does not set it either. Doing so would make the store unreadable by
/// anything else, including Claude's own `/resume`.
pub const PROJECT_DIR_NAME_ENV: &str = "CLAUDE_CODE_PROJECT_DIR_NAME";

/// Where a `CLAUDE_CODE_PROJECT_DIR_NAME` is in force despite the strip above:
/// an account's `settings.json` `env` block, which Claude applies itself and no
/// child environment can undo. Reported, never edited — it is the user's file and
/// they may have a reason.
pub fn project_dir_name_overrides(accs: &[accounts::Account]) -> Vec<(String, String)> {
    accs.iter()
        .filter_map(|a| {
            let p = a.resolve_config_dir()?.join("settings.json");
            let v = crate::jsonedit::read_obj(&p)
                .ok()?
                .get("env")?
                .get(PROJECT_DIR_NAME_ENV)?
                .as_str()?
                .to_string();
            Some((a.id.clone(), v))
        })
        .collect()
}

fn now_stamp() -> String {
    use std::time::{SystemTime, UNIX_EPOCH};
    let secs = SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0);
    format!("@{secs}")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn no_preferences_render_no_arguments() {
        assert!(args_for(&Launch::default()).is_empty(), "an unset mission must launch exactly as before");
    }

    /// An inherited project-dir override would write transcripts under a name
    /// `pathenc` cannot decode, so Houston would launch sessions it can never
    /// resume. Stripped for the same reason `CLAUDE_CONFIG_DIR` is.
    #[test]
    fn an_inherited_project_dir_override_is_not_passed_on() {
        let cmd = launch_command("", None);
        let removed = cmd
            .get_envs()
            .any(|(k, v)| k == std::ffi::OsStr::new(PROJECT_DIR_NAME_ENV) && v.is_none());
        assert!(removed, "{PROJECT_DIR_NAME_ENV} must be cleared for the child");
    }

    #[test]
    fn worktree_has_three_distinguishable_states() {
        let off = Launch::default();
        let bare = Launch { worktree: Some(String::new()), ..Default::default() };
        let named = Launch { worktree: Some("spike".into()), ..Default::default() };
        assert!(!args_for(&off).contains(&"--worktree".to_string()));
        assert_eq!(args_for(&bare), vec!["--worktree"]);
        assert_eq!(args_for(&named), vec!["--worktree", "spike"]);
    }

    /// The two halves of this change meeting: a resume carries the mission's
    /// preferences onto the command line AND leaves a record of the account
    /// decision. Building a `Command` never spawns anything, so this is safe to
    /// assert directly.
    #[test]
    fn a_resume_applies_prefs_and_records_the_decision() {
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("HOUSTON_HOME", tmp.path()) };

        let prefs = Launch { model: "sonnet".into(), worktree: Some("spike".into()), ..Default::default() };
        // No accounts registered under this HOUSTON_HOME, so the decision falls
        // to the config dir that owns the transcript — and must say so.
        let cmd = resume_command("sess-1", "", "C:\\x\\.claude\\projects\\p\\sess-1.jsonl", &prefs);

        let args: Vec<String> = cmd.get_args().map(|a| a.to_string_lossy().into_owned()).collect();
        assert_eq!(args, vec!["--resume", "sess-1", "--model", "sonnet", "--worktree", "spike"]);

        let rec = journal::read_all();
        assert_eq!(rec.len(), 1, "exactly one event per resume");
        assert_eq!(rec[0].event, journal::EVENT_RESUME);
        assert_eq!(rec[0].session, "sess-1");
        assert_eq!(rec[0].reason, Pick::TranscriptOwner.label());
        assert!(rec[0].account.is_empty(), "no account was chosen, so none is claimed");

        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }

    #[test]
    fn value_flags_and_extras_render_in_a_fixed_order() {
        let p = Launch {
            model: "sonnet".into(),
            effort: "high".into(),
            permission_mode: "plan".into(),
            agent: "reviewer".into(),
            worktree: Some("wt".into()),
            fork: true,
            safe_mode: true,
            add_dirs: vec!["C:\\a".into(), String::new()],
            extra: vec!["--verbose".into(), String::new()],
        };
        assert_eq!(
            args_for(&p),
            vec![
                "--model",
                "sonnet",
                "--effort",
                "high",
                "--permission-mode",
                "plan",
                "--agent",
                "reviewer",
                "--add-dir",
                "C:\\a",
                "--worktree",
                "wt",
                "--fork-session",
                "--safe-mode",
                "--verbose",
            ],
            "order is fixed, empty values are dropped, and extras come last so they can override"
        );
    }
}
