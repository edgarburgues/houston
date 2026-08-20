//! First-run provisioning of houston-basics.
//!
//! Policy:
//!   - Config already exists (`config-v2.json`) → already provisioned; do
//!     nothing. This is the UPDATE path — never re-prompt, never overwrite the
//!     user's config.
//!   - No config yet, but the store shows a prior Houston (v1) install →
//!     enable houston-basics by DEFAULT, silently. Existing users keep the
//!     experience they already have; v2 must not quietly downgrade them.
//!   - No config and a pristine store → a fresh install → ASK once whether to
//!     enable houston-basics. The answer is written to config, so it's asked
//!     exactly once. Non-interactive (piped) fresh installs default to basics
//!     (the recommended experience).

use houston_core::{config::Config, paths};
use std::io::{IsTerminal, Write};

/// Run first-run provisioning. Called before the TUI opens. Returns quietly on
/// every path but the fresh-install prompt.
pub fn ensure_provisioned() {
    if Config::exists() {
        return; // already decided (fresh v2, or an update) — respect it.
    }
    if had_prior_install() {
        // Existing user migrating to v2: batteries on, no questions.
        let _ = Config::basics().save();
        eprintln!("houston: welcome to 2.0 — kept your setup (houston-basics enabled).");
        return;
    }
    // Pristine machine: ask once.
    let cfg = if prompt_basics() { Config::basics() } else { Config::default() };
    let _ = cfg.save();
}

/// Whether the store shows a prior Houston (v1) install — any of its files.
fn had_prior_install() -> bool {
    let store = paths::store_dir();
    ["accounts.json", "store.json", "config.json", "modules.json", "scan-cache.json"]
        .iter()
        .any(|f| store.join(f).exists())
        || store.join("programs").is_dir()
        || store.join("modules").is_dir()
}

/// Ask whether to enable houston-basics. Defaults to YES (Enter, or a
/// non-interactive stdin). Only "n"/"no" declines.
fn prompt_basics() -> bool {
    if !std::io::stdin().is_terminal() {
        return true; // can't ask → the recommended default
    }
    eprintln!("\nWelcome to Houston 2.0.\n");
    eprintln!("Install houston-basics? It sets up the familiar interface — the three");
    eprintln!("columns plus a per-account quota panel and a live git strip. You can");
    eprintln!("always change the layout later in config-v2.json.\n");
    eprint!("Enable houston-basics? [Y/n] ");
    let _ = std::io::stderr().flush();
    let mut answer = String::new();
    if std::io::stdin().read_line(&mut answer).is_err() {
        return true;
    }
    !matches!(answer.trim().to_ascii_lowercase().as_str(), "n" | "no")
}

/// `houston provision [--basics|--minimal]` — (re)write the config to a preset,
/// so a user can opt in/out after the fact. Overwrites config-v2.json.
pub fn cmd_provision(args: &[String]) -> anyhow::Result<()> {
    let minimal = args.iter().any(|a| a == "--minimal");
    let cfg = if minimal { Config::default() } else { Config::basics() };
    cfg.save()?;
    println!(
        "houston: wrote {} preset to {}",
        if minimal { "minimal" } else { "houston-basics" },
        houston_core::config::config_path().display()
    );
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Crate-wide, not module-local: a lock only this module respects would not
    /// stop cli.rs's tests removing HOUSTON_HOME halfway through one of these.
    use crate::TEST_ENV_LOCK as ENV_LOCK;

    #[test]
    fn existing_v1_user_gets_basics_silently() {
        let _g = ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        // Simulate a v1 install: an accounts.json in the store.
        std::fs::write(tmp.path().join("accounts.json"), "[]").unwrap();
        unsafe { std::env::set_var("HOUSTON_HOME", tmp.path()) };

        assert!(had_prior_install());
        ensure_provisioned();
        // config-v2.json now exists with the basics layout.
        assert!(Config::exists());
        let cfg = Config::load();
        let json = serde_json::to_string(&cfg).unwrap();
        assert!(json.contains("basics:quota"), "existing users get batteries");

        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }

    #[test]
    fn already_provisioned_is_left_untouched() {
        let _g = ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("HOUSTON_HOME", tmp.path()) };
        // A minimal config already on disk → provisioning must not overwrite it.
        Config::default().save().unwrap();
        ensure_provisioned();
        let json = serde_json::to_string(&Config::load()).unwrap();
        assert!(!json.contains("basics:quota"), "update path must not re-provision");
        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }

    #[test]
    fn pristine_store_is_detected_as_fresh() {
        let _g = ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("HOUSTON_HOME", tmp.path()) };
        assert!(!had_prior_install(), "empty store = fresh install");
        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }
}
