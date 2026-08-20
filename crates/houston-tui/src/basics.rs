//! houston-basics — official in-tree widgets that ship in the binary and
//! reproduce the v1 experience. They are NOT part of the minimal core, but
//! unlike third-party plugins they are compiled in, because they need
//! capabilities a WASM sandbox lacks: `basics:quota` probes the network (via
//! houston-core's usage cache) and `basics:git` shells out to git. Whether the
//! default layout mounts them is decided at first-run provisioning.

use crate::world::World;
use crate::command::Command;
use crate::Widget;
use houston_core::accounts;
use ratatui::{
    layout::Rect,
    style::{Modifier, Style},
    text::Line,
    widgets::Paragraph,
    Frame,
};
use std::time::Duration;

fn usage_bar(pct: f64, cells: usize) -> String {
    let fill = ((pct / 100.0) * cells as f64).round().clamp(0.0, cells as f64) as usize;
    format!("{}{}", "█".repeat(fill), "░".repeat(cells - fill))
}

// ------------------------------------------------------------------ quota --

/// Per-account quota bars, from the shared usage cache (60s TTL, so it never
/// probes per frame — the statusline keeps it warm). Refreshed on mount and on
/// `r`, off the render path.
#[derive(Default)]
pub struct QuotaWidget {
    rows: Vec<houston_core::usage::Usage>,
    loaded: bool,
}

impl QuotaWidget {
    fn refresh(&mut self) {
        let accs = accounts::load().unwrap_or_default();
        self.rows = houston_core::usage::cached_utilization(&accs, Duration::from_secs(60), Duration::from_secs(4));
        self.loaded = true;
    }
}

/// Unix seconds now — the clock the reset countdowns are measured against.
pub(crate) fn now_secs() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// The " ↻ 7h" suffix for an account with no room left, empty otherwise. When a
/// pane says an account is out, the next question is always when it returns.
///
/// The space after the glyph is deliberate: `↻3h` reads as one token and the digit
/// crowds the symbol, while `↻ 3h` reads as "returns in 3h".
pub(crate) fn renew_suffix(u: &houston_core::usage::Usage, weekly: bool, pct: f64) -> String {
    if !u.ok || pct < houston_core::usage::SATURATED {
        return String::new();
    }
    houston_core::usage::resets_in_label(u.resets_in(weekly, now_secs()))
        .map(|s| format!(" ↻ {s}"))
        .unwrap_or_default()
}

/// The window worth showing for an account, as (percentage, label).
///
/// The 5h window is the one you actually work against, so it is the default.
/// The weekly only takes over once it is SATURATED — at which point it, not the
/// 5h figure, is why the account cannot serve a prompt and why the launcher
/// skips it. Showing 5h then would read as "0%, completely free" for the one
/// account that is out, which is how correct balancing came to look like a bug.
///
/// The threshold is `usage::SATURATED`, the very number the launcher uses, so
/// "shown as 7d" and "skipped by the launcher" can never disagree.
pub(crate) fn binding_window(u5: f64, u7: f64) -> (f64, &'static str) {
    if u7 >= houston_core::usage::SATURATED {
        (u7, "7d")
    } else {
        (u5, "5h")
    }
}

/// One account's row in the quota pane. Pure, so what each state looks like is
/// pinned by a test rather than read off a screenshot.
///
/// The failed states are the ones that earn the function: `login` where a login
/// is the remedy — same classifier as the status line (`Usage::needs_login`), so
/// the two can never disagree about which accounts are asking for one, and with
/// the exact command because the pane has the width for it. A bare dash is how
/// two dead accounts sat unnoticed for weeks; it remains only for the failures a
/// login would not cure.
pub(crate) fn quota_row(u: &houston_core::usage::Usage, bar_width: usize) -> String {
    let short: String = u.id.chars().take(7).collect();
    if !u.ok {
        return if u.needs_login() {
            format!("{short:<7} login  (houston run -a {})", u.id)
        } else {
            format!("{short:<7} —")
        };
    }
    let (pct, win) = binding_window(u.u5, u.u7);
    let renew = renew_suffix(u, win == "7d", pct);
    format!("{short:<7} {win} {} {pct:>3.0}{renew}", usage_bar(pct, bar_width))
}

impl Widget for QuotaWidget {
    fn id(&self) -> &str {
        "basics:quota"
    }
    fn title(&self, _w: &World) -> String {
        "Quota".into()
    }
    fn render(&self, area: Rect, frame: &mut Frame, world: &World, _focused: bool) {
        let p = &world.palette;
        let mut lines = Vec::new();
        if !self.loaded {
            lines.push(Line::styled("probing…", Style::new().fg(p.grey)));
        } else if self.rows.is_empty() {
            lines.push(Line::styled("no accounts", Style::new().fg(p.grey)));
        } else {
            let bw = (area.width as usize).saturating_sub(17).clamp(3, 12);
            for u in &self.rows {
                let (pct, _) = binding_window(u.u5, u.u7);
                let line = quota_row(u, bw);
                // An account whose binding window is saturated cannot serve a
                // prompt right now, and the launcher will skip it — so it must
                // not look like the others.
                let style = if u.ok && pct >= houston_core::usage::SATURATED {
                    Style::new().fg(p.grey).add_modifier(Modifier::DIM)
                } else {
                    Style::new().fg(p.fg)
                };
                lines.push(Line::styled(line, style));
            }
        }
        frame.render_widget(Paragraph::new(lines), area);
    }
    fn on_key(&mut self, key: char, _w: &mut World) -> bool {
        if key == 'r' {
            self.refresh();
            true
        } else {
            false
        }
    }
    fn on_click(&mut self, _r: u16, _c: u16, _w: &mut World) {}
    fn on_scroll(&mut self, _up: bool, _w: &mut World) {}
    fn post_render(&mut self, _area: Rect, _w: &World, _focused: bool) {
        if !self.loaded {
            self.refresh();
        }
    }
    fn commands(&self) -> Vec<Command> {
        vec![Command::widget("r", "refresh quota", "basics:quota")]
    }
}

// --------------------------------------------------------------- accounts --

/// The Accounts view: every registered account with the usage bar of whichever
/// window constrains it, the cursor row highlighted. Enter (or double-click)
/// launches a NEW claude
/// session on the selected account — this is how you force an account for a
/// chat (v1's Accounts tab). Account list + cursor live in the World so the
/// app can act on the selection; quota is this widget's own view state.
#[derive(Default)]
pub struct AccountsWidget {
    util: std::collections::HashMap<String, houston_core::usage::Usage>,
    loaded: bool,
}

impl AccountsWidget {
    /// Probe each account's quota (cached) for display. Reads the account list
    /// from the World (loaded at startup); only mutates our own view state.
    fn probe(&mut self, accs: &[accounts::Account]) {
        let util = houston_core::usage::cached_utilization(accs, Duration::from_secs(60), Duration::from_secs(3));
        self.util = util.into_iter().map(|u| (u.id.clone(), u)).collect();
        self.loaded = true;
    }
}

impl Widget for AccountsWidget {
    fn id(&self) -> &str {
        "basics:accounts"
    }
    fn title(&self, _w: &World) -> String {
        "Accounts".into()
    }
    fn render(&self, area: Rect, frame: &mut Frame, world: &World, focused: bool) {
        let p = &world.palette;
        if !self.loaded {
            frame.render_widget(Paragraph::new(Line::styled("loading…", Style::new().fg(p.grey))), area);
            return;
        }
        if world.accounts.is_empty() {
            frame.render_widget(Paragraph::new(Line::styled("no accounts registered", Style::new().fg(p.grey))), area);
            return;
        }
        let bw = (area.width as usize).saturating_sub(26).clamp(3, 10);
        let mut lines = Vec::new();
        let default_usage = houston_core::usage::Usage::default();
        for (i, a) in world.accounts.iter().enumerate() {
            let u = self.util.get(&a.id).unwrap_or(&default_usage);
            let (pct, win) = binding_window(u.u5, u.u7);
            let logged = a.logged_in();
            let quota = if !logged {
                "  (logged out)".to_string()
            } else if u.ok && pct >= houston_core::usage::SATURATED {
                // Say plainly that this one is out, and WHEN it returns: the
                // launcher skips it, so a bare percentage leaves the user
                // guessing both why and for how long.
                let back = houston_core::usage::resets_in_label(u.resets_in(win == "7d", now_secs()))
                    .map(|s| format!("back in {s}"))
                    .unwrap_or_else(|| "no room".into());
                format!("  {win} {} {pct:>3.0}% · {back}", usage_bar(pct, bw))
            } else if u.ok {
                format!("  {win} {} {pct:>3.0}%", usage_bar(pct, bw))
            } else {
                "  —".to_string()
            };
            let mark = if i == world.acc_cursor { "▸" } else { " " };
            let text = format!("{mark} {:<14}{quota}", a.id);
            let style = if i == world.acc_cursor && focused {
                Style::new().fg(p.sel_fg).bg(p.sel_bg).add_modifier(Modifier::BOLD)
            } else if i == world.acc_cursor {
                Style::new().fg(p.accent).add_modifier(Modifier::BOLD)
            } else {
                Style::new().fg(p.fg)
            };
            lines.push(Line::styled(format!("{text:<w$}", w = area.width as usize), style));
        }
        lines.push(Line::raw(""));
        lines.push(Line::styled("enter · launch a new chat on this account", Style::new().fg(p.grey)));
        frame.render_widget(Paragraph::new(lines), area);
    }
    fn on_key(&mut self, key: char, world: &mut World) -> bool {
        match key {
            'j' => {
                world.move_acc_cursor(1);
                true
            }
            'k' => {
                world.move_acc_cursor(-1);
                true
            }
            'r' => {
                world.accounts = accounts::load().unwrap_or_default();
                self.probe(&world.accounts);
                true
            }
            _ => false,
        }
    }
    fn on_click(&mut self, row: u16, _c: u16, world: &mut World) {
        if (row as usize) < world.accounts.len() {
            world.acc_cursor = row as usize;
        }
    }
    fn on_scroll(&mut self, _up: bool, _w: &mut World) {}
    fn post_render(&mut self, _area: Rect, world: &World, _focused: bool) {
        if !self.loaded {
            self.probe(&world.accounts);
        }
    }
    fn commands(&self) -> Vec<Command> {
        vec![
            Command::core(&["k"], "↑/k", "previous account", "Navigate", false),
            Command::core(&["j"], "↓/j", "next account", "Navigate", false),
            Command::widget("r", "refresh quota", "basics:accounts"),
        ]
    }
}

// -------------------------------------------------------------------- git --

/// The live git state of the selected mission's cwd: branch + dirty count.
/// Refreshed when the selection's cwd changes (git is fast; no per-frame exec).
#[derive(Default)]
pub struct GitWidget {
    cwd: String,
    text: String,
}

impl GitWidget {
    fn compute(cwd: &str) -> String {
        if cwd.is_empty() || !std::path::Path::new(cwd).is_dir() {
            return String::new();
        }
        let out = std::process::Command::new("git")
            .args(["--no-optional-locks", "-C", cwd, "status", "--porcelain=v1", "-b"])
            .output();
        let Ok(out) = out else { return String::new() };
        if !out.status.success() {
            return String::new();
        }
        let text = String::from_utf8_lossy(&out.stdout);
        let mut lines = text.lines();
        let head = lines.next().unwrap_or("");
        let branch = head.trim_start_matches("## ").split("...").next().unwrap_or("").to_string();
        let dirty = text.lines().count().saturating_sub(1);
        let state = if dirty > 0 { format!("✗ {dirty} changed") } else { "clean".into() };
        format!("git: {branch} · {state}")
    }
}

impl Widget for GitWidget {
    fn id(&self) -> &str {
        "basics:git"
    }
    fn title(&self, _w: &World) -> String {
        "Git".into()
    }
    fn render(&self, area: Rect, frame: &mut Frame, world: &World, _focused: bool) {
        let p = &world.palette;
        let line = if self.text.is_empty() {
            Line::styled("(not a git repo)", Style::new().fg(p.grey))
        } else {
            Line::styled(self.text.clone(), Style::new().fg(p.fg).add_modifier(Modifier::DIM))
        };
        frame.render_widget(Paragraph::new(line), area);
    }
    fn on_key(&mut self, _k: char, _w: &mut World) -> bool {
        false
    }
    fn on_click(&mut self, _r: u16, _c: u16, _w: &mut World) {}
    fn on_scroll(&mut self, _up: bool, _w: &mut World) {}
    fn post_render(&mut self, _area: Rect, world: &World, _focused: bool) {
        // Recompute only when the selected mission's cwd changes.
        let cwd = world.selected().map(|m| m.cwd.clone()).unwrap_or_default();
        if cwd != self.cwd {
            self.cwd = cwd.clone();
            self.text = Self::compute(&cwd);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bar_fills_proportionally() {
        assert_eq!(usage_bar(0.0, 10), "░░░░░░░░░░");
        assert_eq!(usage_bar(50.0, 10), "█████░░░░░");
        assert_eq!(usage_bar(100.0, 10), "██████████");
        assert_eq!(usage_bar(200.0, 10), "██████████"); // clamps
    }

    #[test]
    fn the_displayed_window_is_the_one_that_blocks() {
        // The situation that made the UI lie: idle on 5h, exhausted for the
        // week. Showing 5h would render it as "0% — free" while it is the one
        // account the launcher must skip.
        assert_eq!(binding_window(0.0, 100.0), (100.0, "7d"));
        // But a weekly that is merely HIGHER is not interesting: with room left
        // for the week, the 5h window is what you actually work against.
        assert_eq!(binding_window(4.0, 12.0), (4.0, "5h"));
        assert_eq!(binding_window(0.0, 60.0), (0.0, "5h"));
        assert_eq!(binding_window(10.0, 89.0), (10.0, "5h"), "just under the limit is still 5h");
        // The switch happens exactly at the launcher's own threshold.
        assert_eq!(binding_window(10.0, houston_core::usage::SATURATED), (90.0, "7d"));
        // A 5h window that is itself the blocker stays on 5h.
        assert_eq!(binding_window(95.0, 20.0), (95.0, "5h"));
        assert_eq!(binding_window(0.0, 0.0), (0.0, "5h"));
    }

    /// The scenario that motivated this: accounts with a dead refresh token
    /// rendered as a bare dash, indistinguishable from a network failure, for
    /// weeks. The row must state the remedy when it knows it.
    #[test]
    fn the_quota_row_says_login_when_that_is_the_remedy() {
        use houston_core::usage::Usage;
        let dead = Usage {
            id: "work-acct".into(),
            err: "refresh rejected (re-login: houston run -a work-acct): refresh HTTP 400".into(),
            ..Default::default()
        };
        let row = quota_row(&dead, 8);
        assert!(row.contains("login"), "{row}");
        assert!(row.contains("houston run -a work-acct"), "the exact command, not just the word: {row}");

        // A failure a login does not cure keeps the dash: sending the user to
        // log in there is sending them to a remedy that fixes nothing.
        let down = Usage { id: "x".into(), err: "usage request failed: timeout".into(), ..Default::default() };
        assert_eq!(quota_row(&down, 8), "x       —");

        // And a healthy account renders its binding window, bar included.
        let live = Usage { id: "live".into(), u5: 20.0, u7: 40.0, ok: true, ..Default::default() };
        let row = quota_row(&live, 8);
        assert!(row.starts_with("live    5h "), "{row}");
        assert!(row.contains(" 20"), "{row}");
    }

    #[test]
    fn git_widget_ids() {
        assert_eq!(QuotaWidget::default().id(), "basics:quota");
        assert_eq!(GitWidget::default().id(), "basics:git");
    }
}
