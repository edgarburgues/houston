//! Claude Code hooks: how Houston learns what happened instead of inferring it.
//!
//! This is the only place Houston writes into a file Claude owns **at runtime**,
//! so the rules are stricter than anywhere else in the codebase:
//!
//! - **Merge, never overwrite.** Hooks belong to the user as much as to Houston.
//!   Anything we did not put there is left exactly as it is and reported.
//! - **Uninstall ships with install**, not after it. `uninstall` removes exactly
//!   our entries and nothing else, and it is tested against a settings file that
//!   also holds a foreign hook.
//! - **A hook must be cheap.** It runs inside somebody's session, so the receiver
//!   (`houston hook <verb>`) appends one journal line and exits.
//!
//! ## Which events, and which are refused
//!
//! Verified against Claude Code 2.1.220's own payloads rather than taken from
//! docs: `SessionStart` carries `source`/`model`/`session_title`, `SessionEnd`
//! carries `reason`, and `StopFailure` carries `error` — with the matcher matched
//! against that error, where `rate_limit` and `authentication_failed` are real
//! values in the binary.
//!
//! `PreToolUse`, `PostToolUse` and `PermissionRequest` are deliberately NOT
//! subscribed: they fire on every tool call, and a `command` hook there is a
//! Houston process per tool call. The restraint is the design.
//!
//! `Notification` IS subscribed, with the matcher `auth_success`, because its
//! payload confirms the matcher is the `notification_type` — without that
//! evidence it would have been a guess, and guessing is not something to do in
//! files that are not ours. `ConfigChange`/`user_settings` is still left out: it
//! only tells Houston its own config changed, which it re-reads anyway.
//!
//! ## What a hook cannot promise, measured
//!
//! Print mode (`claude -p …`) does not run these hooks. `SessionStart` and `Stop`
//! are skipped silently — the binary even says so for one of them (*"Prompt stop
//! hooks are not yet supported outside REPL"*) — and `SessionEnd` is *attempted*
//! and then reported as **cancelled**, three times out of three: Claude fires it
//! while the process is already tearing down, and on Windows a fresh process
//! cannot win that race.
//!
//! **`SessionEnd` was therefore retired.** Its only observable effect was an error
//! line — `SessionEnd hook [houston hook session-end] failed: Hook cancelled` — in
//! every headless run, and its information (is this chat still open?) is something
//! `claude agents --json` answers from live pids anyway. An event whose one visible
//! result is noise in someone's script is worse than not subscribing it.
//!
//! The rest is exactly Decision 1's rule, now with evidence: events may only ever
//! be an accelerator. The live marker comes from `agents --json`, quota from the
//! cache, and the scan is the truth about which conversations exist — so a missed
//! event costs nothing. What is NOT verified is whether these hooks fire in an
//! interactive session; a print-mode harness cannot tell us, and the honest check
//! is one `houston journal --tail` after opening a chat.

use crate::accounts::Account;
use crate::jsonedit::{self, Obj};
use serde_json::{json, Value};

/// One hook Houston installs.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Spec {
    /// Claude's event name, as it appears in `settings.json`.
    pub event: &'static str,
    /// Matcher, matched against the event's own subject (the error for
    /// `StopFailure`). `None` = every occurrence.
    pub matcher: Option<&'static str>,
    /// The `houston hook <verb>` argument, which is ALSO the journal event name.
    pub verb: &'static str,
    /// One line for `hooks status`, so the list explains itself.
    pub why: &'static str,
}

/// What Houston installs. Small on purpose.
pub const SPECS: &[Spec] = &[
    Spec {
        event: "SessionStart",
        matcher: None,
        verb: crate::journal::EVENT_SESSION_START,
        why: "a chat opened (start, resume or fork)",
    },
    // SessionEnd was installed and then RETIRED — see "What a hook cannot
    // promise" below. `uninstall` still removes it, because it sweeps every
    // Houston command out of the hooks object rather than only the ones currently
    // in this list.
    Spec {
        event: "StopFailure",
        matcher: Some("rate_limit"),
        verb: crate::journal::EVENT_RATE_LIMIT,
        why: "this account just ran out — instead of noticing up to 60s later",
    },
    Spec {
        event: "StopFailure",
        matcher: Some("authentication_failed"),
        verb: crate::journal::EVENT_AUTH_FAILED,
        why: "this account needs a re-login",
    },
    Spec {
        event: "Notification",
        matcher: Some("auth_success"),
        verb: crate::journal::EVENT_AUTH_OK,
        why: "a login finished — stop showing that account as off",
    },
];

/// Seconds a hook is allowed before Claude gives up on it. Generous for what it
/// does (append one line) and small enough that a wedged Houston cannot hold a
/// session.
const TIMEOUT_SECS: u64 = 5;

/// The command written into settings.
///
/// A BARE command, not an absolute path: `houston` is on PATH (that is how the
/// status line has always been configured), and baking today's exe path in would
/// break every account the moment the binary moves or self-updates.
fn command_for(spec: &Spec) -> String {
    format!("houston hook {}", spec.verb)
}

/// Whether a settings hook entry is one of ours.
fn is_ours(cmd: &str) -> bool {
    let c = cmd.to_ascii_lowercase();
    c.contains("houston") && c.contains(" hook ")
}

fn settings_path(a: &Account) -> Option<std::path::PathBuf> {
    Some(a.resolve_config_dir()?.join("settings.json"))
}

/// What one account's settings say about one hook.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum State {
    /// Installed and pointing at the right verb.
    Ours,
    /// Ours, but the command text differs (an older Houston wrote it).
    Stale(String),
    /// Not installed.
    Missing,
    /// A non-Houston hook is registered for this event/matcher. Never touched.
    Foreign(usize),
}

/// Read what is installed, without writing anything.
pub fn state_of(a: &Account, spec: &Spec) -> State {
    let Some(p) = settings_path(a) else { return State::Missing };
    let Ok(obj) = jsonedit::read_obj(&p) else { return State::Missing };
    let entries = obj.get("hooks").and_then(|h| h.get(spec.event)).and_then(Value::as_array).cloned().unwrap_or_default();
    let mut foreign = 0usize;
    for entry in &entries {
        if entry.get("matcher").and_then(Value::as_str).unwrap_or_default() != spec.matcher.unwrap_or_default() {
            continue;
        }
        for h in entry.get("hooks").and_then(Value::as_array).cloned().unwrap_or_default() {
            let cmd = h.get("command").and_then(Value::as_str).unwrap_or_default();
            if is_ours(cmd) {
                return if cmd == command_for(spec) { State::Ours } else { State::Stale(cmd.to_string()) };
            }
            foreign += 1;
        }
    }
    if foreign > 0 { State::Foreign(foreign) } else { State::Missing }
}

/// Whether hooks (and, note, the custom status line) are switched off wholesale.
///
/// Worth surfacing loudly: with this set, Houston's journal stops filling AND its
/// status line disappears, and nothing on screen explains why.
pub fn all_hooks_disabled(a: &Account) -> bool {
    settings_path(a)
        .and_then(|p| jsonedit::read_obj(&p).ok())
        .and_then(|o| o.get("disableAllHooks").and_then(Value::as_bool))
        .unwrap_or(false)
}

/// What a pass changed, per account.
#[derive(Debug, Default, Clone)]
pub struct Report {
    pub changed: Vec<String>,
    pub left_alone: Vec<String>,
}

/// Install every spec into every account, merging into whatever is already there.
pub fn install(accs: &[Account]) -> Report {
    patch_all(accs, true)
}

/// Remove exactly Houston's hooks, leaving everything else untouched.
pub fn uninstall(accs: &[Account]) -> Report {
    patch_all(accs, false)
}

fn patch_all(accs: &[Account], add: bool) -> Report {
    let mut rep = Report::default();
    for a in accs {
        let Some(p) = settings_path(a) else {
            rep.left_alone.push(format!("account-{}: no config dir", a.id));
            continue;
        };
        if !p.is_file() {
            rep.left_alone.push(format!("account-{}: no settings.json (doctor --fix seeds one)", a.id));
            continue;
        }
        let mut notes: Vec<String> = Vec::new();
        let mut problems: Vec<String> = Vec::new();
        // create: false — a settings.json that does not exist is heal/fix's
        // business; writing one here that holds nothing but hooks would turn that
        // seeding into a merge conflict.
        let res = jsonedit::patch(&p, false, |obj| {
            let mut hooks = jsonedit::sub_object(obj, "hooks")?;
            if add {
                for spec in SPECS {
                    match add_spec(&mut hooks, spec) {
                        Ok(Some(note)) => notes.push(note),
                        Ok(None) => {}
                        Err(problem) => problems.push(problem),
                    }
                }
            } else {
                // Sweep every Houston command out of every event, not just the
                // ones in SPECS today: an event we once installed and later
                // retired (SessionEnd) must still be removable, and so must one
                // whose verb we rename later.
                notes.extend(remove_all_ours(&mut hooks));
            }
            jsonedit::set_sub_object(obj, "hooks", hooks);
            Ok(())
        });
        match res {
            Ok(()) => {
                for n in notes {
                    rep.changed.push(format!("account-{}: {n}", a.id));
                }
                for p in problems {
                    rep.left_alone.push(format!("account-{}: {p}", a.id));
                }
            }
            Err(e) => rep.left_alone.push(format!("account-{}: settings.json not patched: {e}", a.id)),
        }
        // Foreign hooks are reported after the write, so the report describes the
        // file as it now stands.
        for spec in SPECS {
            if let State::Foreign(n) = state_of(a, spec) {
                rep.left_alone.push(format!(
                    "account-{}: {n} non-Houston hook(s) on {}{} — left alone",
                    a.id,
                    spec.event,
                    spec.matcher.map(|m| format!("/{m}")).unwrap_or_default()
                ));
            }
        }
    }
    rep
}

/// Add one spec to the `hooks` object.
///
/// `Ok(None)` = already exactly right. `Err` = something in the file is shaped in
/// a way Houston will not touch, which must be REPORTED rather than swallowed: the
/// first version returned a bare `Option` and used `?` on the shape checks, so a
/// hand-edited entry made the install a silent no-op that reported success.
fn add_spec(hooks: &mut Obj, spec: &Spec) -> Result<Option<String>, String> {
    let cmd = command_for(spec);
    let where_ = format!("{}{}", spec.event, spec.matcher.map(|m| format!("/{m}")).unwrap_or_default());
    // A key that exists but is not an array is the user's, whatever it is. Writing
    // our array over it would destroy it — the opposite of "merge, never
    // overwrite".
    let mut entries = match hooks.get(spec.event) {
        None | Some(Value::Null) => Vec::new(),
        Some(Value::Array(a)) => a.clone(),
        Some(_) => return Err(format!("{} in settings is not a list — left alone", spec.event)),
    };

    // Reuse the entry with OUR matcher if there is one, so a second install does
    // not stack duplicates — and so a user's hook sharing that matcher keeps its
    // place in the array.
    let want_matcher = spec.matcher.unwrap_or_default();
    let slot = entries.iter().position(|e| e.get("matcher").and_then(Value::as_str).unwrap_or_default() == want_matcher);
    let hook = json!({ "type": "command", "command": cmd, "timeout": TIMEOUT_SECS });

    match slot {
        Some(i) => {
            let Some(list) = entries[i].get_mut("hooks").and_then(|h| h.as_array_mut()) else {
                return Err(format!("the {where_} entry has no `hooks` list — left alone"));
            };
            if let Some(existing) =
                list.iter_mut().find(|h| is_ours(h.get("command").and_then(Value::as_str).unwrap_or_default()))
            {
                if existing.get("command").and_then(Value::as_str) == Some(cmd.as_str()) {
                    return Ok(None); // already exactly right
                }
                *existing = hook;
                hooks.insert(spec.event.to_string(), Value::Array(entries));
                return Ok(Some(format!("{} hook updated to `{cmd}`", spec.event)));
            }
            list.push(hook);
        }
        None => {
            let mut entry = serde_json::Map::new();
            if let Some(m) = spec.matcher {
                entry.insert("matcher".into(), Value::from(m));
            }
            entry.insert("hooks".into(), Value::Array(vec![hook]));
            entries.push(Value::Object(entry));
        }
    }
    hooks.insert(spec.event.to_string(), Value::Array(entries));
    Ok(Some(format!("{where_} → `{cmd}`")))
}

/// Remove every Houston hook from every event, pruning containers that become
/// empty so uninstalling leaves no archaeology behind.
fn remove_all_ours(hooks: &mut Obj) -> Vec<String> {
    let mut notes = Vec::new();
    let events: Vec<String> = hooks.keys().cloned().collect();
    for event in events {
        let Some(mut entries) = hooks.get(&event).and_then(Value::as_array).cloned() else { continue };
        let mut removed = 0usize;
        for entry in entries.iter_mut() {
            if let Some(list) = entry.get_mut("hooks").and_then(|h| h.as_array_mut()) {
                let before = list.len();
                list.retain(|h| !is_ours(h.get("command").and_then(Value::as_str).unwrap_or_default()));
                removed += before - list.len();
            }
        }
        if removed == 0 {
            continue;
        }
        // An entry whose hook list is now empty is a matcher pointing at nothing;
        // an event whose entry list is empty is a key pointing at nothing.
        entries.retain(|e| !e.get("hooks").and_then(Value::as_array).map(|l| l.is_empty()).unwrap_or(true));
        if entries.is_empty() {
            hooks.remove(&event);
        } else {
            hooks.insert(event.clone(), Value::Array(entries));
        }
        notes.push(format!("{event}: {removed} Houston hook(s) removed"));
    }
    notes
}

#[cfg(test)]
mod tests {
    use super::*;

    fn acct(dir: &std::path::Path) -> Account {
        Account { id: "x".into(), config_dir: dir.to_string_lossy().into_owned(), ..Default::default() }
    }

    fn write(dir: &std::path::Path, body: &str) {
        std::fs::write(dir.join("settings.json"), body).unwrap();
    }

    fn read(dir: &std::path::Path) -> Value {
        serde_json::from_str(&std::fs::read_to_string(dir.join("settings.json")).unwrap()).unwrap()
    }

    #[test]
    fn install_is_idempotent_and_keeps_everything_else() {
        let tmp = tempfile::tempdir().unwrap();
        let d = tmp.path();
        let a = acct(d);
        write(d, r#"{"theme":"dark","statusLine":{"type":"command","command":"houston statusline"}}"#);

        let rep = install(std::slice::from_ref(&a));
        assert_eq!(rep.changed.len(), SPECS.len(), "every spec installed: {:?}", rep.changed);
        let v = read(d);
        assert_eq!(v["theme"], "dark", "unrelated settings survive");
        assert_eq!(v["statusLine"]["command"], "houston statusline", "the status line is not touched");
        assert_eq!(v["hooks"]["SessionStart"][0]["hooks"][0]["command"], "houston hook session-start");
        assert_eq!(v["hooks"]["SessionStart"][0]["hooks"][0]["timeout"], TIMEOUT_SECS);
        assert!(v["hooks"]["SessionStart"][0].get("matcher").is_none(), "no matcher means no matcher key");
        // Both StopFailure matchers live in the same event array, each with its own.
        let sf = v["hooks"]["StopFailure"].as_array().unwrap();
        assert_eq!(sf.len(), 2);
        let matchers: Vec<&str> = sf.iter().map(|e| e["matcher"].as_str().unwrap()).collect();
        assert!(matchers.contains(&"rate_limit") && matchers.contains(&"authentication_failed"));

        // A second install changes nothing and says so.
        let again = install(std::slice::from_ref(&a));
        assert!(again.changed.is_empty(), "idempotent: {:?}", again.changed);
        assert_eq!(read(d)["hooks"]["StopFailure"].as_array().unwrap().len(), 2, "no duplicates stacked");

        for spec in SPECS {
            assert_eq!(state_of(&a, spec), State::Ours);
        }
    }

    /// The property that matters most: uninstall removes OUR hooks and nothing
    /// else, from a file that also holds someone else's.
    #[test]
    fn uninstall_removes_exactly_ours() {
        let tmp = tempfile::tempdir().unwrap();
        let d = tmp.path();
        let a = acct(d);
        // A user's own hooks: one on an event we use (same matcher!), one on an
        // event we never touch.
        write(
            d,
            r#"{
              "hooks": {
                "SessionStart": [ { "hooks": [ { "type": "command", "command": "my-own-thing.ps1" } ] } ],
                "PreToolUse": [ { "matcher": "Write", "hooks": [ { "type": "command", "command": "guard.ps1" } ] } ]
              }
            }"#,
        );
        install(std::slice::from_ref(&a));
        let v = read(d);
        assert_eq!(
            v["hooks"]["SessionStart"][0]["hooks"].as_array().unwrap().len(),
            2,
            "ours joins the user's entry rather than replacing it"
        );

        let rep = uninstall(std::slice::from_ref(&a));
        assert!(!rep.changed.is_empty());
        let v = read(d);
        assert_eq!(v["hooks"]["SessionStart"][0]["hooks"][0]["command"], "my-own-thing.ps1", "the user's hook stays");
        assert_eq!(v["hooks"]["SessionStart"][0]["hooks"].as_array().unwrap().len(), 1);
        assert_eq!(v["hooks"]["PreToolUse"][0]["hooks"][0]["command"], "guard.ps1", "an event we never use is intact");
        // Our StopFailure entries are gone, key and all — no empty scaffolding.
        assert!(v["hooks"].get("StopFailure").is_none(), "empty containers are pruned: {v}");
        assert_eq!(state_of(&a, &SPECS[0]), State::Foreign(1), "what remains is reported as the user's");

        // Uninstalling twice is not an error and changes nothing.
        let again = uninstall(std::slice::from_ref(&a));
        assert!(again.changed.is_empty());
    }

    #[test]
    fn a_stale_command_is_rewritten_not_duplicated() {
        let tmp = tempfile::tempdir().unwrap();
        let d = tmp.path();
        let a = acct(d);
        // What an older Houston might have written for an event we still use.
        write(
            d,
            r#"{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"houston hook sessionStart"}]}]}}"#,
        );
        let spec = SPECS.iter().find(|s| s.event == "SessionStart").unwrap();
        assert!(matches!(state_of(&a, spec), State::Stale(_)));
        install(std::slice::from_ref(&a));
        let v = read(d);
        assert_eq!(v["hooks"]["SessionStart"][0]["hooks"].as_array().unwrap().len(), 1, "rewritten in place");
        assert_eq!(v["hooks"]["SessionStart"][0]["hooks"][0]["command"], "houston hook session-start");
        assert_eq!(state_of(&a, spec), State::Ours);
    }

    /// A hook Houston installed and later stopped installing (SessionEnd, retired
    /// after measuring that it only ever produces an error line) must still be
    /// removable — otherwise `uninstall` would leave it behind forever.
    #[test]
    fn uninstall_also_clears_hooks_no_spec_mentions_any_more() {
        let tmp = tempfile::tempdir().unwrap();
        let d = tmp.path();
        let a = acct(d);
        assert!(!SPECS.iter().any(|s| s.event == "SessionEnd"), "SessionEnd is retired");
        write(
            d,
            r#"{"hooks":{
                 "SessionEnd":[{"hooks":[{"type":"command","command":"houston hook session-end"}]}],
                 "PreCompact":[{"hooks":[{"type":"command","command":"houston hook something-we-renamed"}]}]
               }}"#,
        );
        let rep = uninstall(std::slice::from_ref(&a));
        assert_eq!(rep.changed.len(), 2, "{rep:?}");
        let v = read(d);
        assert!(v["hooks"].as_object().map(|o| o.is_empty()).unwrap_or(true), "nothing of ours is left: {v}");
    }

    #[test]
    fn states_are_distinguished_and_the_kill_switch_is_visible() {
        let tmp = tempfile::tempdir().unwrap();
        let d = tmp.path();
        let a = acct(d);
        write(d, "{}");
        assert_eq!(state_of(&a, &SPECS[0]), State::Missing);
        assert!(!all_hooks_disabled(&a));

        write(d, r#"{"disableAllHooks":true}"#);
        assert!(all_hooks_disabled(&a), "the switch that silently kills hooks AND the status line");

        // An unparseable file is not mistaken for "nothing installed" in a way
        // that would make install clobber it — patch refuses, state reports.
        write(d, "{oops");
        assert_eq!(state_of(&a, &SPECS[0]), State::Missing);
        let rep = install(std::slice::from_ref(&a));
        assert!(rep.changed.is_empty() && !rep.left_alone.is_empty(), "{rep:?}");
        assert_eq!(std::fs::read_to_string(d.join("settings.json")).unwrap(), "{oops");
    }

    /// A settings file shaped in a way Houston will not touch must produce a
    /// REPORT, not a silent no-op that reads as success. Found by inspection: the
    /// first version used `?` on these shape checks, so an install could quietly
    /// do nothing and still say it worked.
    #[test]
    fn a_shape_houston_will_not_touch_is_reported_not_swallowed() {
        let tmp = tempfile::tempdir().unwrap();
        let d = tmp.path();
        let a = acct(d);
        // Two hand-editable shapes: an event holding an object instead of a list,
        // and an entry with our matcher but no `hooks` list.
        write(
            d,
            r#"{"hooks":{
                 "SessionStart": {"oops":"an object, not a list"},
                 "StopFailure": [ { "matcher": "rate_limit", "note": "no hooks key here" } ]
               }}"#,
        );
        let rep = install(std::slice::from_ref(&a));
        let joined = rep.left_alone.join(" | ");
        assert!(joined.contains("SessionStart in settings is not a list"), "{joined}");
        assert!(joined.contains("StopFailure/rate_limit entry has no `hooks` list"), "{joined}");
        // And the user's odd values survive untouched.
        let v = read(d);
        assert_eq!(v["hooks"]["SessionStart"]["oops"], "an object, not a list");
        assert_eq!(v["hooks"]["StopFailure"][0]["note"], "no hooks key here");
        // The specs it COULD install still went in, so one bad entry does not
        // block the rest.
        assert_eq!(v["hooks"]["Notification"][0]["hooks"][0]["command"], "houston hook auth-ok");
        assert!(rep.changed.iter().any(|c| c.contains("authentication_failed")), "{:?}", rep.changed);
    }

    #[test]
    fn a_bare_command_is_used_so_a_moved_binary_keeps_working() {
        for spec in SPECS {
            let c = command_for(spec);
            assert!(c.starts_with("houston hook "), "{c}");
            assert!(!c.contains(":\\") && !c.contains('/'), "no absolute path: {c}");
            assert!(is_ours(&c));
        }
        // Recognising ours must survive a full path and a different case, so an
        // uninstall still finds a hook someone rewrote by hand.
        assert!(is_ours("C:\\Users\\x\\.local\\bin\\Houston.EXE Hook session-start"));
        assert!(!is_ours("my-own-thing.ps1"));
        assert!(!is_ours("houston statusline"), "the status line is not a hook");
    }
}
