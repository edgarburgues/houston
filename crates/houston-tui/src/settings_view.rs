//! The Settings tab — Houston's own, reached with `0`.
//!
//! It is not a plugin and not a view of your chats, which is why it sits on the
//! right of the bar rather than among the numbered views.
//!
//! Two kinds of row, and the difference is deliberate:
//!
//! - **Settings** come from `settings_schema::ENTRIES` and are editable. The
//!   schema says how (flag, vocabulary, text, action) and **who writes them**, so
//!   this file never touches a file another module owns: account values go through
//!   `policy::set_everywhere`, the status line through `claude_settings`, hooks
//!   through `hooks`, and Houston's own config through a request `App` applies.
//! - **Facts** are read-only: data-link health, policy drift, how much history
//!   sits past Claude's retention. A row that looked editable and silently was not
//!   would be worse than a row that says it only reports.
//!
//! Account settings are written to EVERY account, because "the same everywhere" is
//! the whole reason Houston manages a fleet — a value that drifts between accounts
//! is the bug this screen exists to prevent.
//!
//! Undo (`u`) restores the previous value of the last account-scope write. It is
//! the safety net where Houston does not own the schema: Claude parses these files
//! at startup, and a value it dislikes should cost one keystroke to take back.

use crate::world::{ConfigRequest, World};
use crate::Widget;
use houston_core::settings_schema::{self as schema, Kind, Owner, Scope};
use houston_core::{accounts::Account, claude_settings as cs, hooks, policy, segments};
use ratatui::{
    layout::Rect,
    style::{Modifier, Style},
    text::{Line, Span},
    widgets::Paragraph,
    Frame,
};
use serde_json::Value;
use std::time::{Duration, Instant};

/// How long a gathered snapshot is reused. The reads are local, but this pane
/// redraws on every keystroke and there is no reason to re-read the fleet's
/// settings sixty times a second.
const REFRESH: Duration = Duration::from_secs(5);

/// Ten years. What "keep my history" means in a setting that only takes days.
const KEEP_FOREVER_DAYS: i64 = 3650;

enum Row {
    Section(String),
    /// An editable row from the schema.
    Setting {
        entry: &'static schema::Entry,
        /// Rendered value, already reflecting disagreement across accounts.
        shown: String,
        /// The accounts do not agree, so any edit here will align them.
        mixed: bool,
    },
    /// Read-only.
    Fact {
        label: String,
        value: String,
        note: String,
        warn: bool,
    },
}

#[derive(Default)]
pub struct SettingsWidget {
    rows: Vec<Row>,
    at: Option<Instant>,
    cursor: usize,
    offset: usize,
    /// Text buffer while typing a value.
    editing: Option<String>,
    /// (key, value before the write) for account-scope changes, newest last.
    undo: Vec<(&'static str, Option<Value>)>,
    /// What the last action did, shown in the pane rather than the footer so it
    /// survives the next keystroke.
    said: String,
    /// A config change staged during a keypress, handed to `World` on the way out
    /// of `on_key` (the only place a widget can reach it).
    pending_config: Option<ConfigRequest>,
}

impl SettingsWidget {
    fn accounts(&self, world: &World) -> Vec<Account> {
        if world.accounts.is_empty() {
            houston_core::accounts::load().unwrap_or_default()
        } else {
            world.accounts.clone()
        }
    }

    /// The fleet's value for a key: `None` when unset everywhere, plus whether the
    /// accounts disagree.
    fn fleet_value(accs: &[Account], key: &str) -> (Option<Value>, bool) {
        let first = accs.first().and_then(|a| policy::get(a, key));
        let mixed = !policy::agrees(accs, key);
        (first, mixed)
    }

    fn shown_value(entry: &schema::Entry, v: Option<&Value>, houston_theme: &str) -> String {
        match (&entry.kind, entry.scope) {
            (Kind::Action(label), _) => (*label).to_string(),
            (_, Scope::Houston) => houston_theme.to_string(),
            (Kind::Flag, _) => match v {
                Some(Value::Bool(true)) => "on".into(),
                Some(Value::Bool(false)) => "off".into(),
                Some(other) => other.to_string(),
                None => "—".into(),
            },
            (_, _) => match v {
                Some(Value::String(s)) if s.is_empty() => "—".into(),
                Some(Value::String(s)) => s.clone(),
                Some(other) => other.to_string(),
                None => "—".into(),
            },
        }
    }

    fn gather(&mut self, world: &World) {
        let accs = self.accounts(world);
        // Houston's palette is a preset, not a stored string: report which of the
        // two built-ins the config currently matches.
        let cfg = houston_core::config::Config::load();
        let preset = if cfg.theme.accent.bytes().all(|b| b.is_ascii_digit()) && !cfg.theme.accent.is_empty() {
            "blue"
        } else {
            "mono"
        };

        let mut rows = Vec::new();
        let mut scope_shown: Option<Scope> = None;
        for entry in schema::ENTRIES {
            if scope_shown != Some(entry.scope) {
                rows.push(Row::Section(
                    match entry.scope {
                        Scope::Houston => "HOUSTON",
                        Scope::Fleet => "EVERY ACCOUNT",
                    }
                    .into(),
                ));
                scope_shown = Some(entry.scope);
            }
            let (value, mixed) = match entry.scope {
                Scope::Fleet => Self::fleet_value(&accs, entry.key),
                Scope::Houston => (None, false),
            };
            // Actions describe their current state rather than a value.
            let shown = match (&entry.kind, entry.owner) {
                (Kind::Action(_), Owner::Hooks) => {
                    let n = accs
                        .iter()
                        .filter(|a| {
                            hooks::SPECS.iter().all(|s| hooks::state_of(a, s) == hooks::State::Ours)
                        })
                        .count();
                    format!("{n}/{} accounts", accs.len())
                }
                (Kind::Action(_), Owner::ClaudeSettings) if entry.key == "statusLine" => {
                    let bad = accs.iter().filter(|a| cs::statusline_state(a).needs_fix()).count();
                    if bad == 0 { "installed everywhere".into() } else { format!("{bad} need it") }
                }
                (Kind::Action(_), Owner::ClaudeSettings) => {
                    let r = cs::retention(&accs);
                    format!("{} days", r.days)
                }
                _ => Self::shown_value(entry, value.as_ref(), preset),
            };
            rows.push(Row::Setting { entry, shown, mixed });
        }

        rows.push(Row::Section("REPORTED ONLY".into()));
        let specs = cfg.segment_specs();
        rows.push(Row::Fact {
            label: "segments".into(),
            value: if specs.is_empty() {
                "none configured".into()
            } else {
                specs
                    .iter()
                    .map(|s| match segments::read(s) {
                        Some(t) => format!("{}={t:?}", s.name),
                        None => format!("{}=(silent)", s.name),
                    })
                    .collect::<Vec<_>>()
                    .join(" ")
            },
            note: "houston segment set <name> <text>".into(),
            warn: false,
        });
        let drift: Vec<&str> = policy::KEYS.iter().map(|k| k.name).filter(|k| !policy::agrees(&accs, k)).collect();
        rows.push(Row::Fact {
            label: "policy".into(),
            value: if drift.is_empty() {
                format!("{} accounts agree on all {} keys", accs.len(), policy::KEYS.len())
            } else {
                format!("drifted: {}", drift.join(", "))
            },
            note: if drift.is_empty() { String::new() } else { "houston policy sync <key> --from <id>".into() },
            warn: !drift.is_empty(),
        });
        let shared = houston_core::heal::shared_dir();
        let mut drifted = 0usize;
        for cd in accs.iter().filter_map(|a| a.resolve_config_dir()).filter(|cd| cd.is_dir()) {
            for d in houston_core::heal::SHARE_DIRS {
                if houston_core::heal::classify(&cd.join(d), &shared.join(d)) != houston_core::heal::LinkState::Ok {
                    drifted += 1;
                }
            }
        }
        rows.push(Row::Fact {
            label: "data links".into(),
            value: if drifted == 0 { "all linked".into() } else { format!("{drifted} drifted") },
            note: if drifted == 0 { String::new() } else { "houston doctor --fix".into() },
            warn: drifted > 0,
        });
        let ret = cs::retention(&accs);
        let hist = cs::history(ret.days);
        rows.push(Row::Fact {
            label: "history".into(),
            value: format!("{} conversations, oldest {}d", hist.files, hist.oldest_days),
            note: if hist.at_risk > 0 { format!("{} past the {}-day limit", hist.at_risk, ret.days) } else { String::new() },
            warn: hist.at_risk > 0,
        });

        self.rows = rows;
        self.at = Some(Instant::now());
        self.clamp_cursor();
    }

    fn clamp_cursor(&mut self) {
        let last = self.rows.len().saturating_sub(1);
        self.cursor = self.cursor.min(last);
        // Landing on a heading would make `enter` do nothing with no explanation.
        if matches!(self.rows.get(self.cursor), Some(Row::Section(_))) {
            self.move_cursor(1);
        }
    }

    /// Move, skipping headings so every stop is something you can act on.
    fn move_cursor(&mut self, delta: i32) {
        if self.rows.is_empty() {
            return;
        }
        let last = self.rows.len() as i32 - 1;
        let mut i = (self.cursor as i32 + delta).clamp(0, last);
        let step = if delta >= 0 { 1 } else { -1 };
        while matches!(self.rows.get(i as usize), Some(Row::Section(_))) {
            let next = i + step;
            if next < 0 || next > last {
                break;
            }
            i = next;
        }
        if !matches!(self.rows.get(i as usize), Some(Row::Section(_))) {
            self.cursor = i as usize;
        }
    }

    fn focused(&self) -> Option<(&'static schema::Entry, bool)> {
        match self.rows.get(self.cursor) {
            Some(Row::Setting { entry, mixed, .. }) => Some((entry, *mixed)),
            _ => None,
        }
    }

    /// Apply a value to every account, remembering the previous one for `u`.
    fn write_fleet(&mut self, world: &World, key: &'static str, value: Option<Value>) {
        let accs = self.accounts(world);
        let before = accs.first().and_then(|a| policy::get(a, key));
        let res = policy::set_everywhere(&accs, key, value.as_ref());
        let failed: Vec<String> = res
            .iter()
            .filter_map(|(id, r)| r.as_ref().err().map(|e| format!("account-{id}: {e}")))
            .collect();
        if failed.is_empty() {
            self.undo.push((key, before));
            let what = value.as_ref().map(trim_value).unwrap_or_else(|| "unset".into());
            self.said = format!("{key} → {what} in {} accounts · u undoes", accs.len());
        } else {
            self.said = failed.join(" · ");
        }
        self.at = None; // re-read, so the row shows what is now on disk
    }

    fn act(&mut self, world: &World, entry: &'static schema::Entry) {
        let accs = self.accounts(world);
        match (entry.owner, entry.key) {
            (Owner::Hooks, _) => {
                // Toggle: if every account has them, remove; otherwise install.
                let all = !accs.is_empty()
                    && accs.iter().all(|a| hooks::SPECS.iter().all(|s| hooks::state_of(a, s) == hooks::State::Ours));
                let rep = if all { hooks::uninstall(&accs) } else { hooks::install(&accs) };
                self.said = if rep.changed.is_empty() && rep.left_alone.is_empty() {
                    "hooks: nothing to change".into()
                } else {
                    format!(
                        "hooks: {} change(s){}",
                        rep.changed.len(),
                        if rep.left_alone.is_empty() { String::new() } else { format!(", {} left alone", rep.left_alone.len()) }
                    )
                };
            }
            (Owner::ClaudeSettings, "statusLine") => {
                let mut done = 0;
                let mut notes = Vec::new();
                for a in &accs {
                    match cs::ensure_statusline(a) {
                        Ok(Some(_)) => done += 1,
                        Ok(None) => {}
                        Err(e) => notes.push(format!("account-{}: {e}", a.id)),
                    }
                }
                self.said = if notes.is_empty() {
                    format!("status line: {done} account(s) changed")
                } else {
                    notes.join(" · ")
                };
            }
            (Owner::ClaudeSettings, "cleanupPeriodDays") => {
                // Explicit, and only ever from this keypress: Houston never
                // changes retention on its own (Phase 0.3). Pressing it again puts
                // it back to Claude's default, so the action is reversible.
                let r = cs::retention(&accs);
                let target = if r.explicit && r.days == KEEP_FOREVER_DAYS { None } else { Some(KEEP_FOREVER_DAYS) };
                let res = cs::set_cleanup_period(&accs, target);
                let bad: Vec<String> = res.iter().filter_map(|(id, r)| r.as_ref().err().map(|e| format!("{id}: {e}"))).collect();
                self.said = if bad.is_empty() {
                    match target {
                        Some(d) => format!("history kept for {d} days in {} accounts", accs.len()),
                        None => "retention back to Claude's default".into(),
                    }
                } else {
                    bad.join(" · ")
                };
            }
            _ => self.said = format!("{} has no action", entry.key),
        }
        self.at = None;
    }

    /// `enter`: cycle, toggle, run, or start typing.
    fn primary(&mut self, world: &World) {
        let Some((entry, _)) = self.focused() else { return };
        match (&entry.kind, entry.scope) {
            (Kind::Action(_), _) => self.act(world, entry),
            (Kind::Choice(vals), Scope::Houston) => {
                // Houston's palette: the presets, applied by App.
                let cfg = houston_core::config::Config::load();
                let now = if cfg.theme.accent.bytes().all(|b| b.is_ascii_digit()) && !cfg.theme.accent.is_empty() {
                    "blue"
                } else {
                    "mono"
                };
                let i = vals.iter().position(|v| *v == now).unwrap_or(0);
                let next = vals[(i + 1) % vals.len()];
                self.pending_config = Some(ConfigRequest::ThemePreset(next));
                self.said = format!("Houston colour → {next}");
                self.at = None;
            }
            (Kind::Choice(vals), Scope::Fleet) => {
                let accs = self.accounts(world);
                let cur = accs
                    .first()
                    .and_then(|a| policy::get(a, entry.key))
                    .and_then(|v| v.as_str().map(|s| s.to_string()))
                    .unwrap_or_default();
                let i = vals.iter().position(|v| **v == cur).unwrap_or(0);
                let next = vals[(i + 1) % vals.len()];
                let value = (!next.is_empty()).then(|| Value::from(next));
                self.write_fleet(world, entry.key, value);
            }
            (Kind::Flag, _) => {
                let accs = self.accounts(world);
                let on = accs.first().and_then(|a| policy::get(a, entry.key)).and_then(|v| v.as_bool()).unwrap_or(false);
                self.write_fleet(world, entry.key, Some(Value::from(!on)));
            }
            (Kind::Text, _) | (Kind::Number, _) => self.begin_edit(world),
        }
    }

    fn begin_edit(&mut self, world: &World) {
        let Some((entry, _)) = self.focused() else { return };
        if matches!(entry.kind, Kind::Action(_)) || entry.scope == Scope::Houston {
            self.said = "this row is not typed in".into();
            return;
        }
        let accs = self.accounts(world);
        let seed = accs
            .first()
            .and_then(|a| policy::get(a, entry.key))
            .map(|v| v.as_str().map(|s| s.to_string()).unwrap_or_else(|| trim_value(&v)))
            .unwrap_or_default();
        self.editing = Some(seed);
    }

    fn commit_edit(&mut self, world: &World, text: String) {
        let Some((entry, _)) = self.focused() else { return };
        let t = text.trim().to_string();
        let value = if t.is_empty() {
            None
        } else if matches!(entry.kind, Kind::Number) {
            match t.parse::<i64>() {
                Ok(n) => Some(Value::from(n)),
                Err(_) => {
                    self.said = format!("{t:?} is not a whole number");
                    return;
                }
            }
        } else {
            Some(Value::from(t))
        };
        self.write_fleet(world, entry.key, value);
    }

    fn undo_last(&mut self, world: &World) {
        let Some((key, before)) = self.undo.pop() else {
            self.said = "nothing to undo".into();
            return;
        };
        let accs = self.accounts(world);
        let _ = policy::set_everywhere(&accs, key, before.as_ref());
        self.said = match before {
            Some(v) => format!("{key} back to {}", trim_value(&v)),
            None => format!("{key} unset again"),
        };
        self.at = None;
    }
}

/// A JSON value on one line, short enough for a status message.
fn trim_value(v: &Value) -> String {
    let s = match v {
        Value::String(s) => s.clone(),
        other => other.to_string(),
    };
    if s.chars().count() > 24 {
        format!("{}…", s.chars().take(23).collect::<String>())
    } else {
        s
    }
}

impl Widget for SettingsWidget {
    fn id(&self) -> &str {
        "settings"
    }

    fn title(&self, _w: &World) -> String {
        let warnings = self.rows.iter().filter(|r| matches!(r, Row::Fact { warn: true, .. })).count();
        if warnings > 0 {
            format!("Settings · {warnings} to look at")
        } else {
            "Settings".into()
        }
    }

    fn render(&self, area: Rect, frame: &mut Frame, world: &World, focused: bool) {
        let p = &world.palette;
        let lw = self
            .rows
            .iter()
            .filter_map(|r| match r {
                Row::Setting { entry, .. } => Some(entry.label.chars().count()),
                Row::Fact { label, .. } => Some(label.chars().count()),
                _ => None,
            })
            .max()
            .unwrap_or(0);

        // The last two lines are reserved: what the last action did, and the keys.
        let reserved = if self.said.is_empty() { 1 } else { 2 };
        let body = (area.height as usize).saturating_sub(reserved);
        let mut lines: Vec<Line> = Vec::new();
        for (i, row) in self.rows.iter().enumerate().skip(self.offset).take(body) {
            let selected = i == self.cursor && focused;
            match row {
                Row::Section(t) => {
                    lines.push(Line::styled(t.clone(), Style::new().fg(p.accent).add_modifier(Modifier::BOLD)))
                }
                Row::Setting { entry, shown, mixed } => {
                    let editing = selected && self.editing.is_some();
                    let value = if editing {
                        format!("{}▌", self.editing.clone().unwrap_or_default())
                    } else if *mixed {
                        format!("{shown} (differs)")
                    } else {
                        shown.clone()
                    };
                    let pad = " ".repeat(lw.saturating_sub(entry.label.chars().count()));
                    let mut spans = vec![
                        Span::styled(if selected { " › " } else { "   " }.to_string(), Style::new().fg(p.accent)),
                        Span::styled(entry.label.to_string(), Style::new().fg(p.fg)),
                        Span::raw(format!("{pad}  ")),
                        Span::styled(
                            value,
                            if editing {
                                Style::new().fg(p.sel_fg).bg(p.sel_bg)
                            } else if *mixed {
                                Style::new().fg(p.accent).add_modifier(Modifier::BOLD)
                            } else if shown == "—" {
                                Style::new().fg(p.grey)
                            } else {
                                Style::new().fg(p.fg).add_modifier(Modifier::BOLD)
                            },
                        ),
                    ];
                    if selected && self.editing.is_none() {
                        spans.push(Span::styled(
                            format!("   {}", entry.why),
                            Style::new().fg(p.grey).add_modifier(Modifier::DIM),
                        ));
                    }
                    lines.push(Line::from(spans));
                }
                Row::Fact { label, value, note, warn } => {
                    let pad = " ".repeat(lw.saturating_sub(label.chars().count()));
                    let mut spans = vec![
                        Span::styled(if selected { " › " } else { "   " }.to_string(), Style::new().fg(p.accent)),
                        Span::styled(label.clone(), Style::new().fg(p.grey)),
                        Span::raw(format!("{pad}  ")),
                        Span::styled(
                            value.clone(),
                            if *warn {
                                Style::new().fg(p.accent).add_modifier(Modifier::BOLD)
                            } else {
                                Style::new().fg(p.fg)
                            },
                        ),
                    ];
                    if !note.is_empty() {
                        spans.push(Span::styled(
                            format!("   {note}"),
                            Style::new().fg(p.grey).add_modifier(Modifier::DIM),
                        ));
                    }
                    lines.push(Line::from(spans));
                }
            }
        }
        if self.rows.is_empty() {
            lines.push(Line::styled("reading…", Style::new().fg(p.grey)));
        }
        if !self.said.is_empty() {
            lines.push(Line::styled(self.said.clone(), Style::new().fg(p.accent)));
        }
        let keys = if self.editing.is_some() {
            "↵ save · esc discard"
        } else {
            "↵ change · e type · d unset · u undo · r re-read"
        };
        lines.push(Line::styled(keys, Style::new().fg(p.grey).add_modifier(Modifier::DIM)));
        frame.render_widget(Paragraph::new(lines), area);
    }

    fn on_key(&mut self, key: char, world: &mut World) -> bool {
        // While typing, every printable key is text — including j/k/r.
        if let Some(buf) = self.editing.as_mut() {
            buf.push(key);
            return true;
        }
        match key {
            'j' => self.move_cursor(1),
            'k' => self.move_cursor(-1),
            'g' => {
                self.cursor = 0;
                self.clamp_cursor();
            }
            'G' => {
                self.cursor = self.rows.len().saturating_sub(1);
                self.clamp_cursor();
            }
            'e' => self.begin_edit(world),
            'd' => {
                if let Some((entry, _)) = self.focused() {
                    if entry.scope == Scope::Fleet && !matches!(entry.kind, Kind::Action(_)) {
                        self.write_fleet(world, entry.key, None);
                    } else {
                        self.said = "this row has nothing to unset".into();
                    }
                }
            }
            'u' => self.undo_last(world),
            'r' => self.at = None,
            ' ' => self.primary(world),
            _ => return false,
        }
        // A request raised while handling the key travels out through World, which
        // the event loop drains — a widget cannot reach `App.cfg` itself.
        if let Some(req) = self.pending_config.take() {
            world.config_request = Some(req);
        }
        // Handled. (`route_key` currently discards this, so the trait's "handled"
        // contract is advisory — but returning the truth costs nothing and the day
        // something reads it, this pane will not be the one that lies.)
        true
    }

    /// Enter belongs to this pane: it is the key the footer advertises, and the
    /// pane is not a list of chats to resume.
    fn on_enter(&mut self, world: &mut World) -> bool {
        self.primary(world);
        if let Some(req) = self.pending_config.take() {
            world.config_request = Some(req);
        }
        true
    }

    fn editing(&self) -> bool {
        self.editing.is_some()
    }

    fn on_edit_commit(&mut self, world: &mut World) {
        if let Some(text) = self.editing.take() {
            self.commit_edit(world, text);
        }
        if let Some(req) = self.pending_config.take() {
            world.config_request = Some(req);
        }
    }

    fn on_edit_cancel(&mut self) {
        // Leave the edit, not the pane: `esc` twice is how you get out entirely.
        self.editing = None;
    }

    fn on_edit_backspace(&mut self) {
        if let Some(b) = self.editing.as_mut() {
            b.pop();
        }
    }

    fn on_click(&mut self, row: u16, _col: u16, _w: &mut World) {
        let i = self.offset + row as usize;
        if i < self.rows.len() && !matches!(self.rows[i], Row::Section(_)) {
            self.cursor = i;
        }
    }

    fn on_scroll(&mut self, up: bool, _w: &mut World) {
        self.move_cursor(if up { -1 } else { 1 });
    }

    fn post_render(&mut self, area: Rect, world: &World, _focused: bool) {
        if self.at.map(|t| t.elapsed() > REFRESH).unwrap_or(true) {
            self.gather(world);
        }
        let h = (area.height as usize).saturating_sub(1);
        if h > 0 {
            if self.cursor < self.offset {
                self.offset = self.cursor;
            } else if self.cursor >= self.offset + h {
                self.offset = self.cursor + 1 - h;
            }
        }
    }

    fn commands(&self) -> Vec<crate::command::Command> {
        use crate::command::Command;
        vec![
            Command::widget("e", "type a value", "settings"),
            Command::widget("d", "unset it", "settings"),
            Command::widget("u", "undo the last change", "settings"),
            Command::widget("r", "re-read everything", "settings"),
        ]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn gathered() -> (SettingsWidget, World) {
        let world = crate::world::tests::test_world();
        let mut w = SettingsWidget::default();
        w.gather(&world);
        (w, world)
    }

    #[test]
    fn every_schema_entry_gets_a_row_under_its_scope() {
        let (w, _) = gathered();
        for e in schema::ENTRIES {
            assert!(
                w.rows.iter().any(|r| matches!(r, Row::Setting { entry, .. } if entry.key == e.key)),
                "{} has no row",
                e.key
            );
        }
        let sections: Vec<&String> = w
            .rows
            .iter()
            .filter_map(|r| match r {
                Row::Section(t) => Some(t),
                _ => None,
            })
            .collect();
        assert!(sections.iter().any(|s| *s == "HOUSTON"));
        assert!(sections.iter().any(|s| *s == "EVERY ACCOUNT"));
        assert!(sections.iter().any(|s| *s == "REPORTED ONLY"));
    }

    /// The cursor must never rest on a heading: `enter` there would do nothing and
    /// say nothing, which reads as a broken key.
    #[test]
    fn the_cursor_skips_headings_and_stays_in_range() {
        let (mut w, _) = gathered();
        w.cursor = 0;
        w.clamp_cursor();
        assert!(!matches!(w.rows[w.cursor], Row::Section(_)));
        for _ in 0..w.rows.len() + 5 {
            w.move_cursor(1);
            assert!(!matches!(w.rows[w.cursor], Row::Section(_)), "landed on a heading");
            assert!(w.cursor < w.rows.len());
        }
        for _ in 0..w.rows.len() + 5 {
            w.move_cursor(-1);
            assert!(!matches!(w.rows[w.cursor], Row::Section(_)));
        }
    }

    #[test]
    fn typing_beats_navigation_while_editing() {
        let (mut w, mut world) = gathered();
        w.editing = Some(String::new());
        let before = w.cursor;
        for c in "jkr".chars() {
            w.on_key(c, &mut world);
        }
        assert_eq!(w.editing.as_deref(), Some("jkr"), "keys are text, not movement");
        assert_eq!(w.cursor, before);
    }

    /// Houston's own palette cannot be written by the widget: it must ask.
    #[test]
    fn the_houston_row_raises_a_request_instead_of_writing() {
        let (mut w, mut world) = gathered();
        let i = w
            .rows
            .iter()
            .position(|r| matches!(r, Row::Setting { entry, .. } if entry.scope == Scope::Houston))
            .expect("a Houston row exists");
        w.cursor = i;
        w.primary(&world);
        // primary() stages it; on_key is what hands it to the world.
        assert!(w.pending_config.is_some());
        w.on_key(' ', &mut world);
        match world.config_request {
            Some(ConfigRequest::ThemePreset(p)) => assert!(p == "blue" || p == "mono", "{p}"),
            other => panic!("expected a theme request, got {other:?}"),
        }
    }

    #[test]
    fn a_number_row_refuses_text_and_says_so() {
        let (mut w, world) = gathered();
        // Point at any Fleet text row and commit nonsense as a number by forcing
        // the kind through the schema lookup the commit path uses.
        if let Some(i) = w.rows.iter().position(|r| matches!(r, Row::Setting { entry, .. } if entry.kind == Kind::Number))
        {
            w.cursor = i;
            w.commit_edit(&world, "not a number".into());
            assert!(w.said.contains("not a whole number"), "{}", w.said);
        }
    }

    /// A row whose key has an OWNER must not offer a raw editor. Editing
    /// `statusLine` or `hooks` as text here would mean two subsystems writing one
    /// file, and they would eventually disagree about what is in it.
    #[test]
    fn owned_rows_refuse_a_raw_editor_and_say_why() {
        let (mut w, world) = gathered();
        // Collect the indices first: walking the rows while calling &mut methods
        // would hold a borrow of the very thing being edited.
        let action_rows: Vec<(usize, &'static str)> = w
            .rows
            .iter()
            .enumerate()
            .filter_map(|(i, r)| match r {
                Row::Setting { entry, .. } if matches!(entry.kind, Kind::Action(_)) => Some((i, entry.key)),
                _ => None,
            })
            .collect();
        let mut checked = 0;
        for (i, key) in action_rows {
            w.cursor = i;
            w.begin_edit(&world);
            assert!(w.editing.is_none(), "{key} opened a text editor");
            assert!(!w.said.is_empty(), "{key} refused silently");
            checked += 1;
        }
        assert!(checked >= 3, "expected the statusLine/hooks/retention rows, saw {checked}");

        // Houston's own palette is a preset, not text either.
        let i = w
            .rows
            .iter()
            .position(|r| matches!(r, Row::Setting { entry, .. } if entry.scope == Scope::Houston))
            .expect("a Houston row exists");
        w.cursor = i;
        w.begin_edit(&world);
        assert!(w.editing.is_none());

        // And `d` does not pretend to unset them either.
        w.said.clear();
        let mut world = world;
        w.on_key('d', &mut world);
        assert!(w.said.contains("nothing to unset"), "{}", w.said);
    }

    #[test]
    fn undo_with_nothing_to_undo_says_so_rather_than_doing_something() {
        let (mut w, world) = gathered();
        assert!(w.undo.is_empty());
        w.undo_last(&world);
        assert_eq!(w.said, "nothing to undo");
    }

    /// Not an assertion — prints the tab:
    /// `cargo test -p houston-tui shot_settings -- --ignored --nocapture`
    #[test]
    #[ignore = "prints the pane for eyeballing; asserts nothing"]
    fn shot_settings() {
        let (w, _) = gathered();
        for row in &w.rows {
            match row {
                Row::Section(t) => println!("\n{t}"),
                Row::Setting { entry, shown, mixed } => println!(
                    "  {:<20} {}{}",
                    entry.label,
                    shown,
                    if *mixed { "  (differs)" } else { "" }
                ),
                Row::Fact { label, value, note, warn } => println!(
                    "{} {:<20} {}{}",
                    if *warn { "!" } else { " " },
                    label,
                    value,
                    if note.is_empty() { String::new() } else { format!("   ({note})") }
                ),
            }
        }
    }
}
