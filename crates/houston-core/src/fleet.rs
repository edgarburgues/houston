//! Applies configuration across EVERY Houston account at once. The shared
//! store already propagates FILES (skills, plugin installs, agents…) via
//! junctions; what never propagated is per-account CONFIG: user-scope MCP
//! servers live in each account's `.claude.json` and plugin enablement in each
//! account's `settings.json`.
//!
//! The pattern for add-operations is **passthrough-then-propagate**: the real
//! `claude` CLI runs ONCE against a source account (full flag parity, real
//! validation and downloads), and the resulting config diff is copied
//! surgically into every other account.

use crate::accounts::Account;
use crate::jsonedit::{self};
pub use crate::jsonedit::Obj;
use crate::launch;
use serde_json::Value;
use std::path::PathBuf;

fn claude_json_path(a: &Account) -> Option<PathBuf> {
    Some(a.resolve_config_dir()?.join(".claude.json"))
}

fn settings_path(a: &Account) -> Option<PathBuf> {
    Some(a.resolve_config_dir()?.join("settings.json"))
}

/// Run the real `claude` CLI once against the account's config dir with
/// inherited stdio, so its own prompts and errors reach the user directly.
pub fn run_claude(a: &Account, args: &[String]) -> std::io::Result<std::process::ExitStatus> {
    let mut cmd = launch::launch_command(&a.id, a.resolve_config_dir().as_deref());
    cmd.args(args);
    cmd.status()
}

// ------------------------------------ user-scope MCP servers (.claude.json) --

/// An account's user-scope MCP servers (empty when there's no file yet — an
/// account that was never logged in still counts as "none", not an error).
pub fn mcp_servers(a: &Account) -> Obj {
    let Some(p) = claude_json_path(a) else { return Obj::new() };
    jsonedit::read_obj(&p).ok().and_then(|o| jsonedit::sub_object(&o, "mcpServers").ok()).unwrap_or_default()
}

/// Upsert `set` and delete `remove` in an account's user scope. The
/// `.claude.json` is created if missing, so a not-yet-logged-in account still
/// receives the config.
pub fn patch_mcp(a: &Account, set: &Obj, remove: &[String]) -> std::io::Result<()> {
    if set.is_empty() && remove.is_empty() {
        return Ok(());
    }
    let p = claude_json_path(a).ok_or_else(|| std::io::Error::other("account has no config dir"))?;
    jsonedit::patch(&p, true, |obj| {
        let mut sub = jsonedit::sub_object(obj, "mcpServers")?;
        for (k, v) in set {
            sub.insert(k.clone(), v.clone());
        }
        for k in remove {
            sub.remove(k);
        }
        jsonedit::set_sub_object(obj, "mcpServers", sub);
        Ok(())
    })
}

// ------------------------------- plugin enablement (settings.json) ----------

/// An account's plugin-enablement map (empty when there's no file yet).
pub fn enabled_plugins(a: &Account) -> Obj {
    let Some(p) = settings_path(a) else { return Obj::new() };
    jsonedit::read_obj(&p).ok().and_then(|o| jsonedit::sub_object(&o, "enabledPlugins").ok()).unwrap_or_default()
}

/// Upsert/remove entries in an account's `enabledPlugins`.
pub fn patch_plugins(a: &Account, set: &Obj, remove: &[String]) -> std::io::Result<()> {
    if set.is_empty() && remove.is_empty() {
        return Ok(());
    }
    let p = settings_path(a).ok_or_else(|| std::io::Error::other("account has no config dir"))?;
    jsonedit::patch(&p, true, |obj| {
        let mut sub = jsonedit::sub_object(obj, "enabledPlugins")?;
        for (k, v) in set {
            sub.insert(k.clone(), v.clone());
        }
        for k in remove {
            sub.remove(k);
        }
        jsonedit::set_sub_object(obj, "enabledPlugins", sub);
        Ok(())
    })
}

/// The keys matching a user-supplied plugin spec: an exact `name@marketplace`
/// first, else every key whose name part equals the spec.
pub fn match_plugin_keys(spec: &str, keys: &[String]) -> Vec<String> {
    let mut out: Vec<String> = keys
        .iter()
        .filter(|k| k.as_str() == spec || k.split('@').next() == Some(spec))
        .cloned()
        .collect();
    out.sort();
    out
}

// ------------------------------------------------------------------ diffing --

/// Compare two config maps: `changed` holds keys whose value is new or
/// different in `after`; `removed` holds keys that disappeared.
pub fn diff(before: &Obj, after: &Obj) -> (Obj, Vec<String>) {
    let mut changed = Obj::new();
    for (k, v) in after {
        if before.get(k) != Some(v) {
            changed.insert(k.clone(), v.clone());
        }
    }
    let mut removed: Vec<String> = before.keys().filter(|k| !after.contains_key(*k)).cloned().collect();
    removed.sort();
    (changed, removed)
}

/// The sorted union of the maps' keys.
pub fn keys(maps: &[Obj]) -> Vec<String> {
    let mut set: std::collections::BTreeSet<String> = std::collections::BTreeSet::new();
    for m in maps {
        set.extend(m.keys().cloned());
    }
    set.into_iter().collect()
}

/// Whether a plugin-enablement value counts as ON. Claude Code stores `true`
/// or a small object; anything falsey reads as off.
pub fn is_enabled(v: &Value) -> bool {
    match v {
        Value::Bool(b) => *b,
        Value::Null => false,
        _ => true,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn obj(pairs: &[(&str, Value)]) -> Obj {
        pairs.iter().map(|(k, v)| (k.to_string(), v.clone())).collect()
    }

    #[test]
    fn diff_finds_added_changed_and_removed() {
        let before = obj(&[
            ("keep", serde_json::json!({"command": "a"})),
            ("edit", serde_json::json!({"command": "old"})),
            ("gone", serde_json::json!({"command": "x"})),
        ]);
        let after = obj(&[
            ("keep", serde_json::json!({"command": "a"})),
            ("edit", serde_json::json!({"command": "new"})),
            ("fresh", serde_json::json!({"command": "y"})),
        ]);
        let (changed, removed) = diff(&before, &after);
        // Untouched entries are NOT propagated (that would rewrite every account).
        assert!(!changed.contains_key("keep"));
        assert_eq!(changed["edit"]["command"], "new");
        assert_eq!(changed["fresh"]["command"], "y");
        assert_eq!(changed.len(), 2);
        assert_eq!(removed, vec!["gone"]);
    }

    #[test]
    fn diff_is_empty_when_nothing_moved() {
        let m = obj(&[("a", serde_json::json!(1))]);
        let (changed, removed) = diff(&m, &m);
        assert!(changed.is_empty() && removed.is_empty());
    }

    #[test]
    fn keys_are_the_sorted_union() {
        let a = obj(&[("b", serde_json::json!(1)), ("a", serde_json::json!(1))]);
        let b = obj(&[("c", serde_json::json!(1)), ("a", serde_json::json!(1))]);
        assert_eq!(keys(&[a, b]), vec!["a", "b", "c"]);
        assert!(keys(&[]).is_empty());
    }

    #[test]
    fn plugin_specs_match_bare_name_or_exact_key() {
        let ks: Vec<String> =
            ["fmt@market", "fmt@other", "lint@market"].iter().map(|s| s.to_string()).collect();
        // A bare name hits every marketplace it's installed from.
        assert_eq!(match_plugin_keys("fmt", &ks), vec!["fmt@market", "fmt@other"]);
        // A fully-qualified spec hits exactly one.
        assert_eq!(match_plugin_keys("fmt@other", &ks), vec!["fmt@other"]);
        assert!(match_plugin_keys("missing", &ks).is_empty());
    }

    #[test]
    fn enablement_reads_bools_and_objects() {
        assert!(is_enabled(&serde_json::json!(true)));
        assert!(!is_enabled(&serde_json::json!(false)));
        assert!(!is_enabled(&serde_json::json!(null)));
        assert!(is_enabled(&serde_json::json!({"version": "1"})));
    }

    #[test]
    fn patch_mcp_writes_into_an_account_dir_and_reads_back() {
        let dir = tempfile::tempdir().unwrap();
        let cfg = dir.path().join("account-x");
        std::fs::create_dir_all(&cfg).unwrap();
        let a = Account { id: "x".into(), config_dir: cfg.to_string_lossy().into_owned(), ..Default::default() };

        // No file yet → no servers, and patch creates it.
        assert!(mcp_servers(&a).is_empty());
        let set = obj(&[("lifeos-db", serde_json::json!({"command": "ssh", "args": ["homelab"]}))]);
        patch_mcp(&a, &set, &[]).unwrap();
        let got = mcp_servers(&a);
        assert_eq!(got["lifeos-db"]["command"], "ssh");

        // A second patch keeps the first entry (surgical, not a rewrite).
        patch_mcp(&a, &obj(&[("forgejo", serde_json::json!({"command": "docker"}))]), &[]).unwrap();
        let got = mcp_servers(&a);
        assert_eq!(got.len(), 2);
        assert_eq!(got["lifeos-db"]["command"], "ssh");

        // Removal takes the named one only.
        patch_mcp(&a, &Obj::new(), &["lifeos-db".to_string()]).unwrap();
        let got = mcp_servers(&a);
        assert_eq!(keys(&[got]), vec!["forgejo"]);

        // A no-op patch is not an error and writes nothing.
        patch_mcp(&a, &Obj::new(), &[]).unwrap();
    }

    #[test]
    fn plugin_enablement_round_trips_in_settings() {
        let dir = tempfile::tempdir().unwrap();
        let cfg = dir.path().join("account-y");
        std::fs::create_dir_all(&cfg).unwrap();
        // A settings.json with unrelated user preferences that must survive.
        std::fs::write(cfg.join("settings.json"), r#"{"model":"opus","statusLine":{"type":"command"}}"#).unwrap();
        let a = Account { id: "y".into(), config_dir: cfg.to_string_lossy().into_owned(), ..Default::default() };

        patch_plugins(&a, &obj(&[("fmt@market", serde_json::json!(true))]), &[]).unwrap();
        assert!(is_enabled(&enabled_plugins(&a)["fmt@market"]));

        let back = jsonedit::read_obj(&cfg.join("settings.json")).unwrap();
        assert_eq!(back["model"], "opus", "unrelated settings survive");
        assert_eq!(back["statusLine"]["type"], "command");
    }
}
