//! houston-core — the kernel of Houston 2.0: pure data logic, no UI.
//!
//! Faithful Rust port of the Go v1 data layer:
//! - `model`   — Mission / Meta / Program types.
//! - `pathenc` — Claude's lossy project-dir encoding and its reconstruction.
//! - `plugin`  — plugin manifest discovery (widgets/runtime/capabilities).
//! - `config`  — the layout tree + theme (declarative, config-v2.json).
//! - `oauth`   — Claude.ai OAuth refresh-token flow.
//! - `usage`   — quota probing + time-weighted pressure balancing (+cache).
//! - `accounts`— the multi-account registry + per-account credentials.
//! - `flock`   — cross-process advisory lock (an OS lock on an open handle).
//! - `heal`    — self-repair of the shared data links between accounts.
//! - `browse`  — $BROWSER routing, isolating OAuth logins in a private window.
//! - `jsonedit`/`fleet` — surgical config edits, applied across all accounts.
//! - `update`  — release check + signature-verified self-update.
//! - `export`  — a transcript rendered to Markdown.
//! - `store`   — mutable state: per-mission Meta + Programs (store.json / .prog).
//! - `scan`    — streaming .jsonl transcript parser, multi-root discovery,
//!   logical-key dedup and the incremental scan cache.
//!
//! Everything here is synchronous and side-effect-light; the TUI schedules it
//! off the render thread.

pub mod accounts;
pub mod oauth;
pub mod usage;
pub mod config;
pub mod agents;
pub mod atomic;
pub mod browse;
pub mod claude_settings;
pub mod compat;
pub mod export;
pub mod fleet;
pub mod flock;
pub mod journal;
pub mod jsonedit;
pub mod heal;
pub mod hooks;
pub mod launch;
pub mod model;
pub mod paths;
pub mod plugin;
pub mod policy;
pub mod pathenc;
pub mod scan;
pub mod segments;
pub mod settings_schema;
pub mod store;
pub mod text;
pub mod update;

/// One process-wide lock for tests that set environment variables. It must be
/// shared across MODULES: `HOUSTON_HOME` is global to the process, so a
/// per-module mutex serializes a module against itself while still racing every
/// other module's tests.
#[cfg(test)]
pub(crate) static TEST_ENV_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
