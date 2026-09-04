//! The built-in widgets: Filters, Missions, Preview, plus MissingWidget (a
//! placeholder for an unknown/unloaded widget id). They implement the same
//! `Widget` trait plugin widgets will implement — the core is its own first
//! API consumer. Widgets hold only view state (scroll offsets); shared data
//! and the color Palette live in the World.

use crate::world::World;
use crate::command::Command;
use crate::Widget;
use ratatui::{
    layout::Rect,
    style::{Modifier, Style},
    text::{Line, Span},
    widgets::Paragraph,
    Frame,
};

use houston_core::text::clip;

use crate::world::Filter;

// ---------------------------------------------------------------- Filters --

#[derive(Default)]
pub struct FiltersWidget;

impl Widget for FiltersWidget {
    fn id(&self) -> &str {
        "filters"
    }
    fn title(&self, _world: &World) -> String {
        "Filters".into()
    }

    fn render(&self, area: Rect, frame: &mut Frame, world: &World, focused: bool) {
        let p = &world.palette;
        let mut lines = Vec::new();
        for f in Filter::ORDER {
            let sel = world.filter == f;
            let style = match (sel, focused) {
                (true, true) => Style::new().fg(p.sel_fg).bg(p.sel_bg).add_modifier(Modifier::BOLD),
                (true, false) => Style::new().fg(p.accent).add_modifier(Modifier::BOLD),
                _ => Style::new().fg(p.fg),
            };
            lines.push(Line::styled(clip(f.label(), area.width as usize), style));
        }
        frame.render_widget(Paragraph::new(lines), area);
    }

    fn on_key(&mut self, key: char, world: &mut World) -> bool {
        let pos = Filter::ORDER.iter().position(|f| *f == world.filter).unwrap_or(0);
        match key {
            'j' => {
                let next = (pos + 1).min(Filter::ORDER.len() - 1);
                world.set_filter(Filter::ORDER[next]);
                true
            }
            'k' => {
                world.set_filter(Filter::ORDER[pos.saturating_sub(1)]);
                true
            }
            _ => false,
        }
    }

    fn on_click(&mut self, row: u16, _col: u16, world: &mut World) {
        if let Some(f) = Filter::ORDER.get(row as usize) {
            world.set_filter(*f);
        }
    }

    fn on_scroll(&mut self, _up: bool, _world: &mut World) {}

    fn commands(&self) -> Vec<Command> {
        vec![
            Command::core(&["k"], "k", "previous filter", "Navigate", false),
            Command::core(&["j"], "j", "next filter", "Navigate", false),
        ]
    }
}

// --------------------------------------------------------------- Missions --

#[derive(Default)]
pub struct MissionsWidget {
    offset: usize,
}

impl MissionsWidget {
    /// Keep the cursor inside the viewport window.
    fn ensure_visible(&mut self, cursor: usize, height: usize) {
        if height == 0 {
            return;
        }
        if cursor < self.offset {
            self.offset = cursor;
        } else if cursor >= self.offset + height {
            self.offset = cursor + 1 - height;
        }
    }
}

impl Widget for MissionsWidget {
    fn id(&self) -> &str {
        "missions"
    }
    fn title(&self, world: &World) -> String {
        if world.scanning {
            return "Missions · scanning…".into();
        }
        let live = world.live.len();
        if live > 0 {
            format!("Missions · {} · {live} live", world.visible.len())
        } else {
            format!("Missions · {}", world.visible.len())
        }
    }

    fn render(&self, area: Rect, frame: &mut Frame, world: &World, focused: bool) {
        let p = &world.palette;
        let mut lines = Vec::new();
        let h = area.height as usize;
        let start = self.offset.min(world.visible.len());
        for (row, &mi) in world.visible.iter().enumerate().skip(start).take(h) {
            let m = &world.missions[mi];
            let meta = world.meta_of(m);
            let pin = if meta.pinned { "★" } else { " " };
            let date = m
                .last_time
                .map(|t| t.with_timezone(&chrono::Local).format("%m-%d").to_string())
                .unwrap_or_else(|| "     ".into());
            // A running session gets one leading cell: filled+accent while it
            // works, hollow while it waits, blank when nothing is running. It is
            // the first column because "is this open right now" changes what
            // Enter should do.
            let (mark, mark_fg) = match world.live_of(&m.id) {
                Some(l) if l.busy() => ("●", p.accent),
                Some(_) => ("○", p.grey),
                None => (" ", p.fg),
            };
            let body_w = (area.width as usize).saturating_sub(1);
            let text = clip(&format!("{pin} {date}  {}", m.title), body_w);
            let style = if row == world.cursor {
                if focused {
                    Style::new().fg(p.sel_fg).bg(p.sel_bg).add_modifier(Modifier::BOLD)
                } else {
                    Style::new().fg(p.accent).add_modifier(Modifier::BOLD)
                }
            } else {
                Style::new().fg(p.fg)
            };
            let body = Span::styled(format!("{:w$}", text, w = body_w), style);
            // On the selected row the marker joins the highlight rather than
            // fighting it — a differently coloured cell inside a solid bar reads
            // as a rendering bug.
            let mark = if row == world.cursor { Span::styled(mark, style) } else { Span::styled(mark, Style::new().fg(mark_fg)) };
            lines.push(Line::from(vec![mark, body]));
        }
        if world.visible.is_empty() {
            let hint = if world.scanning { "scanning…" } else { "(no missions)" };
            lines.push(Line::styled(hint, Style::new().fg(p.grey)));
        }
        frame.render_widget(Paragraph::new(lines), area);
    }

    fn on_key(&mut self, key: char, world: &mut World) -> bool {
        match key {
            'j' => {
                world.move_cursor(1);
                true
            }
            'k' => {
                world.move_cursor(-1);
                true
            }
            'g' => {
                world.cursor = 0;
                true
            }
            'G' => {
                world.cursor = world.visible.len().saturating_sub(1);
                true
            }
            // v1's mission keys: pin and archive act on the selection and the
            // visible set is rebuilt (pinned float, archived leave the All view).
            '*' => {
                if let Some(key) = world.selected().map(|m| m.key()) {
                    let _ = world.store.toggle_pin(&key);
                    world.rebuild();
                }
                true
            }
            'a' => {
                if let Some(key) = world.selected().map(|m| m.key()) {
                    let _ = world.store.toggle_archive(&key);
                    world.rebuild();
                }
                true
            }
            // Export the transcript to Markdown next to the store, and report
            // where it landed (transcripts hold secrets — owner-only perms).
            'e' => {
                if let Some(m) = world.selected().cloned() {
                    let dir = houston_core::paths::store_dir().join("exports");
                    let out = dir.join(houston_core::export::default_name(&m));
                    world.status = match houston_core::export::mission(&m, &out) {
                        // The path alone reads as "done"; transcripts carry
                        // secrets, so say that where the user is looking.
                        Ok(p) => format!("exported → {} · PLAINTEXT, may contain secrets", p.display()),
                        Err(e) => format!("export failed: {e}"),
                    };
                }
                true
            }
            _ => false,
        }
    }

    fn on_click(&mut self, row: u16, _col: u16, world: &mut World) {
        let idx = self.offset + row as usize;
        if idx < world.visible.len() {
            world.cursor = idx;
        }
    }

    fn on_scroll(&mut self, up: bool, world: &mut World) {
        world.move_cursor(if up { -3 } else { 3 });
    }

    fn post_render(&mut self, area: Rect, world: &World, _focused: bool) {
        self.ensure_visible(world.cursor, area.height as usize);
    }

    fn commands(&self) -> Vec<Command> {
        vec![
            Command::core(&["k"], "↑/k", "move up", "Navigate", true),
            Command::core(&["j"], "↓/j", "move down", "Navigate", false),
            Command::core(&["g"], "g", "top", "Navigate", false),
            Command::core(&["G"], "G", "bottom", "Navigate", false),
            Command::core(&["*"], "*", "pin / unpin", "Chat", false),
            Command::core(&["a"], "a", "archive / unarchive", "Chat", false),
            Command::core(&["e"], "e", "export to Markdown", "Chat", false),
        ]
    }
}

// ---------------------------------------------------------------- Preview --

#[derive(Default)]
pub struct PreviewWidget {
    scroll: u16,
}

impl Widget for PreviewWidget {
    fn id(&self) -> &str {
        "preview"
    }
    fn title(&self, _world: &World) -> String {
        "Preview".into()
    }

    fn render(&self, area: Rect, frame: &mut Frame, world: &World, _focused: bool) {
        let p = &world.palette;
        let Some(m) = world.selected() else {
            frame.render_widget(
                Paragraph::new(Line::styled("No mission selected.", Style::new().fg(p.grey))),
                area,
            );
            return;
        };
        let w = area.width as usize;
        let sec = Style::new().fg(p.accent).add_modifier(Modifier::BOLD);
        // TEXT uses the readable grey; `dim` (the muddier one) is for DECORATION
        // only — a field label the eye has to read is not decoration.
        let label = Style::new().fg(p.grey);
        let val = Style::new().fg(p.fg);
        let rule = Style::new().fg(p.dim);
        let quiet = Style::new().fg(p.grey);

        // The v1.2.1 block design, palette-driven: title, rule, ▍ sections.
        let mut lines: Vec<Line> = Vec::new();
        lines.push(Line::styled(clip(&m.title, w), Style::new().fg(p.accent).add_modifier(Modifier::BOLD)));
        lines.push(Line::styled("─".repeat(w), rule));

        let kv = |lines: &mut Vec<Line>, k: &str, v: String| {
            if v.is_empty() {
                return;
            }
            lines.push(Line::from(vec![
                Span::styled(format!("  {k:<9}"), label),
                Span::styled(clip(&v, w.saturating_sub(12)), val),
            ]));
        };

        lines.push(Line::styled("▍ Session", sec));
        kv(&mut lines, "branch", m.git_branch.clone());
        kv(&mut lines, "version", m.version.clone());
        if m.message_count() > 0 {
            kv(
                &mut lines,
                "activity",
                format!("{} msgs · 👤{} 🤖{} · 🔧{}", m.message_count(), m.user_msgs, m.assistant_msgs, m.tool_calls()),
            );
        }
        let mut size = format!("{:.1} MB", m.size_bytes as f64 / (1024.0 * 1024.0));
        if let (Some(f), Some(l)) = (m.first_time, m.last_time) {
            size += &format!(
                " · {}→{}",
                f.with_timezone(&chrono::Local).format("%y-%m-%d"),
                l.with_timezone(&chrono::Local).format("%y-%m-%d")
            );
        }
        kv(&mut lines, "size", size);

        lines.push(Line::default());
        lines.push(Line::styled("▍ Location", sec));
        lines.push(Line::styled(format!("  {}", clip(&human_cwd(&m.cwd), w.saturating_sub(2))), val));
        let meta = world.meta_of(m);
        if !meta.tags.is_empty() {
            lines.push(Line::styled(format!("  #{}", meta.tags.join("  #")), val));
        }
        if !meta.note.is_empty() {
            lines.push(Line::styled(format!("  “{}”", clip(&meta.note, w.saturating_sub(4))), quiet));
        }

        for (head, body) in [("First message", &m.first_prompt), ("Last message", &m.last_prompt)] {
            if body.is_empty() {
                continue;
            }
            lines.push(Line::default());
            lines.push(Line::styled(format!("▍ {head}"), sec));
            for l in body.lines().take(12) {
                lines.push(Line::styled(clip(l, w), val));
            }
        }

        lines.push(Line::default());
        lines.push(Line::styled("─".repeat(w), rule));
        // The copy-paste resume hint is something you READ, so it gets the
        // readable grey rather than the decoration one.
        lines.push(Line::styled(clip(&format!("↵ cd \"{}\"; claude --resume {}", m.cwd, m.id), w), quiet));

        frame.render_widget(Paragraph::new(lines).scroll((self.scroll, 0)), area);
    }

    fn on_key(&mut self, key: char, _world: &mut World) -> bool {
        match key {
            'j' => {
                self.scroll = self.scroll.saturating_add(1);
                true
            }
            'k' => {
                self.scroll = self.scroll.saturating_sub(1);
                true
            }
            _ => false,
        }
    }

    fn on_click(&mut self, _row: u16, _col: u16, _world: &mut World) {}

    fn on_scroll(&mut self, up: bool, _world: &mut World) {
        self.scroll = if up { self.scroll.saturating_sub(3) } else { self.scroll.saturating_add(3) };
    }

    fn commands(&self) -> Vec<Command> {
        vec![
            Command::core(&["k"], "k", "scroll up", "Navigate", false),
            Command::core(&["j"], "j", "scroll down", "Navigate", false),
        ]
    }
}

// ---------------------------------------------------------------- Missing --

/// A pane for a plugin that WILL NOT RUN, and why.
///
/// One widget covers every such reason (unapproved exec code, a manifest that
/// changed since approval, an api version this build doesn't implement) because
/// they all need the same thing: stay inert, say what happened in the pane the
/// user is looking at, and name the command that fixes it. Failing silently or
/// with a stack trace in a log is what makes plugins feel haunted.
pub struct BlockedWidget {
    pub id: String,
    pub headline: String,
    /// Explanation lines, then the command to run (styled as the action).
    pub detail: Vec<String>,
    pub action: Option<String>,
    /// Held for the same reason as `MissingWidget`'s: the pane is inert because
    /// the plugin needs fixing, and fixing it should not also mean re-typing its
    /// configuration.
    pub settings: Option<serde_json::Value>,
}

impl Widget for BlockedWidget {
    fn id(&self) -> &str {
        &self.id
    }
    fn configure(&mut self, settings: &serde_json::Value) {
        self.settings = Some(settings.clone());
    }
    fn settings(&self) -> Option<serde_json::Value> {
        self.settings.clone()
    }
    fn title(&self, _world: &World) -> String {
        format!("⚠ {}", self.id)
    }
    fn render(&self, area: Rect, frame: &mut Frame, world: &World, _focused: bool) {
        let p = &world.palette;
        let mut lines =
            vec![Line::styled(self.headline.clone(), Style::new().fg(p.accent).add_modifier(Modifier::BOLD))];
        for d in &self.detail {
            lines.push(Line::styled(d.clone(), Style::new().fg(p.grey)));
        }
        if let Some(a) = &self.action {
            lines.push(Line::raw(""));
            lines.push(Line::styled(a.clone(), Style::new().fg(p.fg)));
        }
        frame.render_widget(Paragraph::new(lines), area);
    }
    fn on_key(&mut self, _key: char, _world: &mut World) -> bool {
        false
    }
    fn on_click(&mut self, _row: u16, _col: u16, _world: &mut World) {}
    fn on_scroll(&mut self, _up: bool, _world: &mut World) {}
}

/// Placeholder for a layout that references an unknown widget id (a plugin not
/// loaded, or a typo). Visible and inert — never a hard error.
#[derive(Default)]
pub struct MissingWidget {
    pub id: String,
    /// The pane's settings, held only to hand them back on save.
    ///
    /// A placeholder is exactly the case where losing them hurts most: the
    /// widget is absent because the plugin is not installed *yet*, and the whole
    /// point of a visible placeholder is that the layout survives until it is.
    /// Dropping the configuration on the next border drag would quietly punish
    /// the user for installing things in the wrong order.
    pub settings: Option<serde_json::Value>,
}

impl Widget for MissingWidget {
    fn id(&self) -> &str {
        &self.id
    }
    fn configure(&mut self, settings: &serde_json::Value) {
        self.settings = Some(settings.clone());
    }
    fn settings(&self) -> Option<serde_json::Value> {
        self.settings.clone()
    }
    fn title(&self, _world: &World) -> String {
        format!("? {}", self.id)
    }
    fn render(&self, area: Rect, frame: &mut Frame, world: &World, _focused: bool) {
        let lines = vec![
            Line::styled(
                format!("unknown widget: {}", self.id),
                Style::new().fg(world.palette.accent).add_modifier(Modifier::BOLD),
            ),
            Line::styled("no built-in or plugin provides it", Style::new().fg(world.palette.grey)),
        ];
        frame.render_widget(Paragraph::new(lines), area);
    }
    fn on_key(&mut self, _key: char, _world: &mut World) -> bool {
        false
    }
    fn on_click(&mut self, _row: u16, _col: u16, _world: &mut World) {}
    fn on_scroll(&mut self, _up: bool, _world: &mut World) {}
}

/// Shorten a path to its last two segments (…/parent/leaf) — same rule as
/// v1.2.1's preview.
fn human_cwd(p: &str) -> String {
    if p.is_empty() {
        return "(no cwd)".into();
    }
    let norm = p.replace('\\', "/");
    let segs: Vec<&str> = norm.trim_end_matches('/').split('/').filter(|s| !s.is_empty()).collect();
    if segs.len() <= 2 {
        return p.to_string();
    }
    format!("…/{}/{}", segs[segs.len() - 2], segs[segs.len() - 1])
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn human_cwd_shortens_long_paths() {
        assert_eq!(human_cwd(r"C:\Users\me\Documents\Github\proj"), "…/Github/proj");
        assert_eq!(human_cwd(r"C:\p"), r"C:\p");
        assert_eq!(human_cwd(""), "(no cwd)");
    }

    #[test]
    fn missions_viewport_follows_cursor() {
        let mut w = MissionsWidget::default();
        w.ensure_visible(0, 10);
        assert_eq!(w.offset, 0);
        w.ensure_visible(15, 10);
        assert_eq!(w.offset, 6);
        w.ensure_visible(2, 10);
        assert_eq!(w.offset, 2);
    }

    #[test]
    fn clip_is_char_safe() {
        assert_eq!(clip("hola", 10), "hola");
        assert_eq!(clip("hola mundo", 5), "hola…");
        assert_eq!(clip("ñandú ñandú", 6), "ñandú…");
    }
}
