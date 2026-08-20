//! Settings Houston is willing to own across every account.
//!
//! `fleet` already propagates MCP servers and plugin enablement. The key that
//! matters more is **`permissions`**: a rule you add in one account silently does
//! not exist in the other two, so the same command is approved here and asks
//! there — and you find out at the worst moment.
//!
//! Three rules make this safe enough to be useful:
//!
//! - **An allowlist, not "any key".** Houston propagates only what it is willing
//!   to be responsible for. `settings.json` also holds per-account identity and
//!   things like the status line; copying it wholesale would be a footgun with a
//!   nice name.
//! - **Copy from an explicit source, never merge.** Unioning permission lists
//!   sounds friendlier until it resurrects a `deny` rule someone deliberately
//!   removed. One named source account, last-writer-wins, predictable.
//! - **Dry run by default.** `sync` prints what would change and writes nothing
//!   until asked, because the values here decide what Claude does without
//!   stopping to ask.
//!
//! Claude merges permissions across scopes (managed, user, project, local).
//! Houston writes the **user** scope of each account and nothing else, which is
//! the only scope that means the same thing in every account.

use crate::accounts::Account;
use crate::jsonedit;
use serde_json::Value;

/// One propagatable key.
pub struct Key {
    pub name: &'static str,
    pub why: &'static str,
}

/// What Houston will propagate. Deliberately short; everything here is something
/// that should plainly be the same in every account.
pub const KEYS: &[Key] = &[
    Key { name: "permissions", why: "allow/ask/deny rules — the one that bites when it drifts" },
    Key { name: "env", why: "environment variables Claude sets for tools" },
    Key { name: "effortLevel", why: "default reasoning effort" },
    Key { name: "outputStyle", why: "output style" },
    Key { name: "theme", why: "colour theme" },
    Key { name: "editorMode", why: "vim or emacs keys" },
    Key { name: "availableModels", why: "which models are offered" },
    Key { name: "fallbackModel", why: "model used when the first is unavailable" },
    Key { name: "sandbox", why: "sandbox policy for Claude's own Bash tool" },
    Key { name: "includeCoAuthoredBy", why: "co-author trailer in commits" },
    Key { name: "alwaysThinkingEnabled", why: "extended thinking by default" },
    Key { name: "autoCompactEnabled", why: "automatic compaction" },
    Key { name: "fileCheckpointingEnabled", why: "file checkpointing" },
    // Auto memory writes into `projects/<project>/memory/`, the one directory
    // under the SHARED transcripts tree that Claude's retention sweep leaves
    // alone. Fleet-wide because the directory itself is shared: one account with
    // it off is one account that stops contributing notes every other account
    // reads.
    Key { name: "autoMemoryEnabled", why: "whether Claude keeps its own notes per project" },
    Key { name: "dialogExpiry", why: "how long a forwarded dialog waits before it answers itself" },
    // macOS/Linux only — Claude does not offer cross-session messaging on native
    // Windows — but this is the setting that decides whether another session can
    // put words in this one's mouth, so it belongs in the audited set wherever it
    // applies.
    Key { name: "crossSessionInbound", why: "whether your other sessions may message this one" },
    // NOT here on purpose: statusLine (claude_settings owns it),
    // cleanupPeriodDays (retention is the user's decision, per Phase 0.3), hooks
    // (the hooks module owns those), enabledPlugins and mcpServers (fleet owns
    // them). One owner per key, or two subsystems fight over a file.
    //
    // Also not here, for a different reason: `spellcheck`, whose value is an
    // object ({enabled, language}) rather than a scalar. Everything above is a
    // value this module can compare for equality and copy wholesale; an object
    // needs a per-field editor, and offering one field as if it were the whole key
    // would silently drop the other on every write.
    //
    // And the marketplace keys (`extraKnownMarketplaces`, `strictKnownMarketplaces`)
    // stay out even though they look like a fit: Claude accepts the aliases
    // `additionalMarketplaces`/`allowedMarketplaces` and MAY REWRITE one spelling
    // to the other when it updates the file. A sync that compares spellings would
    // report drift Claude created and then flap against it.
];

pub fn is_known(name: &str) -> bool {
    KEYS.iter().any(|k| k.name == name)
}

fn settings_path(a: &Account) -> Option<std::path::PathBuf> {
    Some(a.resolve_config_dir()?.join("settings.json"))
}

/// One account's value for a key: `None` when unset (or unreadable).
pub fn get(a: &Account, key: &str) -> Option<Value> {
    let p = settings_path(a)?;
    jsonedit::read_obj(&p).ok()?.get(key).cloned().filter(|v| !v.is_null())
}

/// A compact one-line rendering, so a table row stays a row.
///
/// Values here are objects and arrays; printing them raw would wrap and make the
/// table useless, while printing only "set" would hide the drift the table exists
/// to show. So: shape and size, plus the scalar itself when it IS a scalar.
pub fn summarize(v: Option<&Value>) -> String {
    match v {
        None => "—".into(),
        Some(Value::Object(o)) => {
            // For permissions, the counts ARE the interesting part.
            let mut parts: Vec<String> = Vec::new();
            for (k, val) in o {
                let n = match val {
                    Value::Array(a) => a.len(),
                    Value::Object(m) => m.len(),
                    _ => 0,
                };
                parts.push(if n > 0 { format!("{k}:{n}") } else { k.clone() });
            }
            parts.sort();
            clip(&parts.join(" "))
        }
        Some(Value::Array(a)) => format!("[{}]", a.len()),
        Some(Value::String(s)) => clip(s),
        Some(other) => other.to_string(),
    }
}

/// One column's worth. Wide enough for `allow:12 ask:3 deny:4`, narrow enough
/// that three accounts still fit an 80-column terminal.
const CELL: usize = 22;

fn clip(s: &str) -> String {
    if s.chars().count() <= CELL {
        return s.to_string();
    }
    format!("{}…", s.chars().take(CELL - 1).collect::<String>())
}

/// Whether every account agrees about a key.
pub fn agrees(accs: &[Account], key: &str) -> bool {
    let mut it = accs.iter().map(|a| get(a, key));
    let Some(first) = it.next() else { return true };
    it.all(|v| v == first)
}

/// What a sync would do to one account.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Change {
    /// Already identical.
    Same,
    /// Absent here; would be added.
    Add,
    /// Different here; would be replaced (the old value is shown so a mistake is
    /// recoverable by hand).
    Replace(String),
    /// The source has no value, so there is nothing to copy — and Houston does
    /// NOT delete: "unset it everywhere" is a different intention from "make
    /// these match", and conflating them would silently drop a rule.
    SourceUnset,
}

/// Compute, without writing, what `sync` would do for one key.
pub fn plan(accs: &[Account], source: &Account, key: &str) -> Vec<(String, Change)> {
    let want = get(source, key);
    accs.iter()
        .filter(|a| a.id != source.id)
        .map(|a| {
            let have = get(a, key);
            let change = match (&want, &have) {
                (None, _) => Change::SourceUnset,
                (Some(w), Some(h)) if w == h => Change::Same,
                (Some(_), None) => Change::Add,
                (Some(_), Some(h)) => Change::Replace(summarize(Some(h))),
            };
            (a.id.clone(), change)
        })
        .collect()
}

/// Copy `key` from `source` into every other account. Returns what changed.
///
/// Only ever called after a dry run has been shown: this rewrites values that
/// decide what Claude does without asking.
pub fn sync(accs: &[Account], source: &Account, key: &str) -> Vec<(String, Result<Change, String>)> {
    let Some(want) = get(source, key) else {
        return accs
            .iter()
            .filter(|a| a.id != source.id)
            .map(|a| (a.id.clone(), Ok(Change::SourceUnset)))
            .collect();
    };
    let mut out = Vec::new();
    for a in accs.iter().filter(|a| a.id != source.id) {
        let before = get(a, key);
        if before.as_ref() == Some(&want) {
            out.push((a.id.clone(), Ok(Change::Same)));
            continue;
        }
        let Some(p) = settings_path(a) else {
            out.push((a.id.clone(), Err("no config dir".into())));
            continue;
        };
        let want = want.clone();
        let res = jsonedit::patch(&p, false, |obj| {
            obj.insert(key.to_string(), want);
            Ok(())
        });
        match res {
            Ok(()) => out.push((
                a.id.clone(),
                Ok(match before {
                    None => Change::Add,
                    Some(b) => Change::Replace(summarize(Some(&b))),
                }),
            )),
            Err(e) => out.push((a.id.clone(), Err(e.to_string()))),
        }
    }
    out
}

/// Set one key to the same value in EVERY account.
///
/// The other direction from `sync`: there the truth lives in one account and is
/// copied; here the caller states the value outright, which is what a settings
/// screen does. Still routed through this module rather than written by the
/// caller, so these keys keep exactly one owner.
///
/// `None` removes the key, which is a real intention ("stop overriding this") and
/// distinct from setting it to a default.
pub fn set_everywhere(accs: &[Account], key: &str, value: Option<&Value>) -> Vec<(String, Result<Change, String>)> {
    let mut out = Vec::new();
    for a in accs {
        let before = get(a, key);
        if before.as_ref() == value {
            out.push((a.id.clone(), Ok(Change::Same)));
            continue;
        }
        let Some(p) = settings_path(a) else {
            out.push((a.id.clone(), Err("no config dir".into())));
            continue;
        };
        let want = value.cloned();
        let key_owned = key.to_string();
        let res = jsonedit::patch(&p, false, |obj| {
            match &want {
                Some(v) => obj.insert(key_owned.clone(), v.clone()),
                None => obj.remove(&key_owned),
            };
            Ok(())
        });
        match res {
            Ok(()) => out.push((
                a.id.clone(),
                Ok(match before {
                    None => Change::Add,
                    Some(b) => Change::Replace(summarize(Some(&b))),
                }),
            )),
            Err(e) => out.push((a.id.clone(), Err(e.to_string()))),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fleet(tmp: &std::path::Path, bodies: &[(&str, &str)]) -> Vec<Account> {
        bodies
            .iter()
            .map(|(id, body)| {
                let d = tmp.join(id);
                std::fs::create_dir_all(&d).unwrap();
                std::fs::write(d.join("settings.json"), body).unwrap();
                Account { id: (*id).into(), config_dir: d.to_string_lossy().into_owned(), ..Default::default() }
            })
            .collect()
    }

    const PERMS_A: &str = r#"{"permissions":{"allow":["Bash(git status)","Read(**)"],"deny":["Bash(rm -rf *)"]}}"#;

    #[test]
    fn drift_is_detected_and_a_plan_says_what_would_change() {
        let tmp = tempfile::tempdir().unwrap();
        let accs = fleet(tmp.path(), &[("a", PERMS_A), ("b", r#"{"permissions":{"allow":["Read(**)"]}}"#), ("c", "{}")]);
        assert!(!agrees(&accs, "permissions"));
        assert!(agrees(&accs, "theme"), "unset everywhere counts as agreement");

        let p = plan(&accs, &accs[0], "permissions");
        assert_eq!(p.len(), 2, "the source is not in its own plan");
        assert!(matches!(p[0], (ref id, Change::Replace(_)) if id == "b"));
        assert_eq!(p[1], ("c".to_string(), Change::Add));

        // Syncing FROM an account that has nothing must not delete elsewhere:
        // "unset it everywhere" is a different intention from "make these match".
        let from_empty = plan(&accs, &accs[2], "permissions");
        assert!(from_empty.iter().all(|(_, c)| *c == Change::SourceUnset));
        let done = sync(&accs, &accs[2], "permissions");
        assert!(done.iter().all(|(_, r)| matches!(r, Ok(Change::SourceUnset))));
        assert_eq!(get(&accs[0], "permissions"), serde_json::from_str::<Value>(PERMS_A).unwrap().get("permissions").cloned());
    }

    #[test]
    fn sync_copies_exactly_and_leaves_everything_else_alone() {
        let tmp = tempfile::tempdir().unwrap();
        let accs = fleet(
            tmp.path(),
            &[
                ("a", PERMS_A),
                // b has its own identity and status line, which must survive.
                (
                    "b",
                    r#"{"permissions":{"allow":["Read(**)"]},"statusLine":{"type":"command","command":"houston statusline"},"theme":"dark"}"#,
                ),
            ],
        );
        let res = sync(&accs, &accs[0], "permissions");
        assert!(matches!(res[0].1, Ok(Change::Replace(_))));
        assert_eq!(get(&accs[1], "permissions"), get(&accs[0], "permissions"), "b now matches a exactly");

        let b = jsonedit::read_obj(&std::path::Path::new(&accs[1].config_dir).join("settings.json")).unwrap();
        assert_eq!(b["statusLine"]["command"], "houston statusline", "unrelated keys survive");
        assert_eq!(b["theme"], "dark");

        // Idempotent: a second sync reports nothing to do.
        let again = sync(&accs, &accs[0], "permissions");
        assert!(matches!(again[0].1, Ok(Change::Same)));
        assert!(agrees(&accs, "permissions"));
    }

    /// What a settings screen needs: state the value, have every account take it.
    #[test]
    fn set_everywhere_states_the_value_and_can_remove_it() {
        let tmp = tempfile::tempdir().unwrap();
        let accs = fleet(tmp.path(), &[("a", r#"{"theme":"dark","model":"opus"}"#), ("b", "{}")]);

        let res = set_everywhere(&accs, "theme", Some(&Value::from("light")));
        assert!(matches!(res[0].1, Ok(Change::Replace(_))), "{res:?}");
        assert_eq!(res[1].1.as_ref().unwrap(), &Change::Add);
        assert!(agrees(&accs, "theme"));
        assert_eq!(get(&accs[0], "theme"), Some(Value::from("light")));
        // Unrelated keys are untouched.
        assert_eq!(get(&accs[0], "model"), Some(Value::from("opus")));

        // Idempotent.
        let again = set_everywhere(&accs, "theme", Some(&Value::from("light")));
        assert!(again.iter().all(|(_, r)| matches!(r, Ok(Change::Same))), "{again:?}");

        // None REMOVES: "stop overriding this" is a different intention from
        // setting it to whatever the default happens to be.
        let removed = set_everywhere(&accs, "theme", None);
        assert!(matches!(removed[0].1, Ok(Change::Replace(_))));
        assert_eq!(get(&accs[0], "theme"), None);
        assert_eq!(get(&accs[0], "model"), Some(Value::from("opus")), "removal is surgical");
    }

    #[test]
    fn the_allowlist_owns_only_what_nobody_else_owns() {
        assert!(is_known("permissions") && is_known("env"));
        // Keys with another owner must not be here, or two subsystems fight over
        // the same file.
        for stolen in ["statusLine", "hooks", "enabledPlugins", "mcpServers", "cleanupPeriodDays"] {
            assert!(!is_known(stolen), "{stolen} belongs to another module");
        }
    }

    #[test]
    fn a_summary_stays_one_row_wide() {
        let v: Value = serde_json::from_str(PERMS_A).unwrap();
        let s = summarize(v.get("permissions"));
        assert_eq!(s, "allow:2 deny:1", "counts are the interesting part of permissions");
        assert_eq!(summarize(None), "—");
        assert_eq!(summarize(Some(&Value::from("dark"))), "dark");
        assert_eq!(summarize(Some(&serde_json::json!(["a", "b", "c"]))), "[3]");
        assert_eq!(summarize(Some(&Value::from(true))), "true");
        // Long values are cut, never wrapped: a wrapped row destroys the table.
        // ONE budget for every shape — the first version capped objects at 28 and
        // strings at 12 while the table reserved 12, so a real value made every
        // row after it ragged.
        for v in [
            Value::from("a-very-long-output-style-name"),
            serde_json::json!({"aaaaaaaa":[1],"bbbbbbbb":[1],"cccccccc":[1],"dddddddd":[1]}),
        ] {
            let s = summarize(Some(&v));
            assert!(s.chars().count() <= CELL, "{s} ({} chars)", s.chars().count());
        }
    }
}
