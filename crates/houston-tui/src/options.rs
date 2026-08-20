//! The `o` launch-options overlay: how a chat reopens.
//!
//! The preferences themselves already existed and already applied on every
//! resume (`Meta.launch`, rendered by `launch::args_for`) — the only way to enter
//! one was editing `store.json` by hand. This is the setter.
//!
//! Three decisions worth stating, because they are what make it usable rather
//! than merely present:
//!
//! - **It shows the command.** The bottom line is the actual argument list
//!   `args_for` will produce, so the overlay is never a claim about what will
//!   happen — it is the thing that will happen. A launch options screen you have
//!   to trust is a launch options screen you avoid.
//! - **Every change saves immediately**, like pinning and tagging already do.
//!   There is no save/cancel, so `esc` means exactly one thing.
//! - **Enumerated fields cycle, but text is always reachable** with `e`. Claude
//!   owns these vocabularies and adds to them; a UI that only offered today's
//!   values would be wrong by the next release. `bypassPermissions` is
//!   deliberately NOT in the cycle — landing on "no permission prompts for this
//!   chat" by tapping a key is not a thing this should make easy, and `e` is
//!   there for someone who means it.

use crate::shell::draw_modal;
use crate::world::World;
use houston_core::model::Launch;
use ratatui::{
    layout::Rect,
    style::{Modifier, Style},
    text::{Line, Span},
    widgets::Paragraph,
    Frame,
};

/// One editable preference.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Field {
    Model,
    Effort,
    PermissionMode,
    Agent,
    Worktree,
    Fork,
    SafeMode,
    AddDirs,
    Extra,
}

pub const FIELDS: [Field; 9] = [
    Field::Model,
    Field::Effort,
    Field::PermissionMode,
    Field::Agent,
    Field::Worktree,
    Field::Fork,
    Field::SafeMode,
    Field::AddDirs,
    Field::Extra,
];

impl Field {
    fn label(self) -> &'static str {
        match self {
            Field::Model => "model",
            Field::Effort => "effort",
            Field::PermissionMode => "permissions",
            Field::Agent => "agent",
            Field::Worktree => "worktree",
            Field::Fork => "open a copy",
            Field::SafeMode => "safe mode",
            Field::AddDirs => "extra dirs",
            Field::Extra => "raw args",
        }
    }

    /// What the flag does, in the fewest words that are still true.
    fn hint(self) -> &'static str {
        match self {
            Field::Model => "--model",
            Field::Effort => "--effort",
            Field::PermissionMode => "--permission-mode",
            Field::Agent => "--agent",
            Field::Worktree => "--worktree [name] · isolated git worktree",
            Field::Fork => "--fork-session · new session id, original untouched",
            // What --safe-mode actually turns off, per claude --help.
            Field::SafeMode => "--safe-mode · no CLAUDE.md, skills, plugins, hooks, MCP",
            Field::AddDirs => "--add-dir, comma separated",
            Field::Extra => "passed verbatim, last",
        }
    }

    /// The values `enter` cycles through, empty when the field is text or a flag.
    ///
    /// Taken from `claude --help` rather than from memory: `--effort` really does
    /// offer `max`, and `--permission-mode`'s choices are `acceptEdits, auto,
    /// bypassPermissions, manual, dontAsk, plan` — note there is no `default`,
    /// which an earlier version of this list invented.
    /// The cycle, for callers outside this module (tests that must not hard-code a
    /// model name the CLI can change under them).
    pub fn cycle_values(self) -> &'static [&'static str] {
        self.cycle()
    }

    fn cycle(self) -> &'static [&'static str] {
        match self {
            // Aliases, not full names: `--model` takes either, and an alias keeps
            // pointing at the latest of that family.
            Field::Model => &["", "fable", "opus", "sonnet", "haiku"],
            Field::Effort => &["", "low", "medium", "high", "xhigh", "max"],
            // bypassPermissions and dontAsk are deliberately NOT here — see the
            // module docs. `e` types any of them for someone who means it.
            Field::PermissionMode => &["", "plan", "acceptEdits", "manual", "auto"],
            _ => &[],
        }
    }

    fn is_flag(self) -> bool {
        matches!(self, Field::Fork | Field::SafeMode)
    }
}

/// Open overlay state: which mission, its preferences, the cursor, and the text
/// buffer while a field is being typed.
pub struct Options {
    /// Store key (project/id) — what the preferences are saved against.
    pub key: String,
    /// Mission id and title, for the preview line and the modal title.
    pub id: String,
    pub title: String,
    pub prefs: Launch,
    pub row: usize,
    /// `Some` while typing; the buffer replaces the field on `enter`.
    pub editing: Option<String>,
}

impl Options {
    pub fn new(key: String, id: String, title: String, prefs: Launch) -> Self {
        Options { key, id, title, prefs, row: 0, editing: None }
    }

    pub fn field(&self) -> Field {
        FIELDS[self.row.min(FIELDS.len() - 1)]
    }

    pub fn move_row(&mut self, delta: i32) {
        let last = FIELDS.len() as i32 - 1;
        self.row = (self.row as i32 + delta).clamp(0, last) as usize;
    }

    /// The current value as text, for display and as the seed when editing.
    pub fn value(&self, f: Field) -> String {
        let p = &self.prefs;
        match f {
            Field::Model => p.model.clone(),
            Field::Effort => p.effort.clone(),
            Field::PermissionMode => p.permission_mode.clone(),
            Field::Agent => p.agent.clone(),
            Field::Worktree => match &p.worktree {
                None => String::new(),
                // An on-but-unnamed worktree has to LOOK different from off, or
                // the three states collapse into two on screen.
                Some(n) if n.is_empty() => "(unnamed)".into(),
                Some(n) => n.clone(),
            },
            Field::Fork => bool_text(p.fork),
            Field::SafeMode => bool_text(p.safe_mode),
            Field::AddDirs => p.add_dirs.join(", "),
            Field::Extra => p.extra.join(" "),
        }
    }

    /// Replace a field from typed text. Empty clears it.
    pub fn set_text(&mut self, f: Field, text: &str) {
        let t = text.trim().to_string();
        match f {
            Field::Model => self.prefs.model = t,
            Field::Effort => self.prefs.effort = t,
            Field::PermissionMode => self.prefs.permission_mode = t,
            Field::Agent => self.prefs.agent = t,
            Field::Worktree => self.prefs.worktree = if t.is_empty() { None } else { Some(t) },
            Field::Fork => self.prefs.fork = truthy(&t),
            Field::SafeMode => self.prefs.safe_mode = truthy(&t),
            Field::AddDirs => self.prefs.add_dirs = split_list(&t, ','),
            Field::Extra => self.prefs.extra = split_list(&t, ' '),
        }
    }

    /// `enter`/`space` on a row: cycle a vocabulary, toggle a flag, or start
    /// typing when the value is free text.
    pub fn primary(&mut self) {
        let f = self.field();
        if f.is_flag() {
            let on = !truthy(&self.value(f));
            self.set_text(f, if on { "on" } else { "" });
            return;
        }
        let cycle = f.cycle();
        if !cycle.is_empty() {
            let cur = self.value(f);
            let i = cycle.iter().position(|v| *v == cur).unwrap_or(0);
            let next = cycle[(i + 1) % cycle.len()];
            self.set_text(f, next);
            return;
        }
        if f == Field::Worktree {
            // off → on (unnamed) → off. A NAME is deliberate, so it goes through
            // `e` rather than appearing by tapping enter.
            self.prefs.worktree = match &self.prefs.worktree {
                None => Some(String::new()),
                Some(_) => None,
            };
            return;
        }
        self.begin_edit();
    }

    pub fn begin_edit(&mut self) {
        let f = self.field();
        // Seed with the real value, except the "(unnamed)" placeholder, which is
        // display text and would otherwise become a worktree called that.
        let seed = if f == Field::Worktree { self.prefs.worktree.clone().unwrap_or_default() } else { self.value(f) };
        self.editing = Some(seed);
    }

    pub fn clear_field(&mut self) {
        let f = self.field();
        self.set_text(f, "");
    }

    /// The claude arguments these preferences produce — the same call the resume
    /// path makes, so the preview cannot drift from the behaviour.
    pub fn preview(&self) -> String {
        let args = houston_core::launch::args_for(&self.prefs);
        let head = format!("claude --resume {}", short_id(&self.id));
        if args.is_empty() {
            head
        } else {
            format!("{head} {}", args.join(" "))
        }
    }
}

fn bool_text(b: bool) -> String {
    if b { "on".into() } else { String::new() }
}

fn truthy(s: &str) -> bool {
    !s.is_empty() && !s.eq_ignore_ascii_case("off") && !s.eq_ignore_ascii_case("false") && s != "0"
}

fn split_list(s: &str, sep: char) -> Vec<String> {
    s.split(sep).map(|p| p.trim().to_string()).filter(|p| !p.is_empty()).collect()
}

/// Session ids are 36 characters of noise; the head is enough to recognise one.
fn short_id(id: &str) -> String {
    id.chars().take(8).collect()
}

pub fn render(frame: &mut Frame, body: Rect, world: &World, o: &Options) {
    let p = &world.palette;
    // Width first: the title and the preview are clipped to the box that will
    // actually be drawn, not to a constant that guesses at it.
    let w = 76u16.min(body.width.saturating_sub(6)).max(24);
    let text_w = w.saturating_sub(4) as usize;
    let mut lines: Vec<Line> = Vec::new();
    lines.push(Line::styled(clip(&o.title, text_w), Style::new().fg(p.grey)));
    lines.push(Line::raw(""));

    let lw = FIELDS.iter().map(|f| f.label().len()).max().unwrap_or(0);
    for (i, f) in FIELDS.iter().enumerate() {
        let selected = i == o.row;
        let editing = selected && o.editing.is_some();
        let shown = if editing {
            format!("{}▌", o.editing.clone().unwrap_or_default())
        } else {
            let v = o.value(*f);
            if v.is_empty() { "—".into() } else { v }
        };
        let pad = " ".repeat(lw - f.label().len());
        let label_style = if selected {
            Style::new().fg(p.accent).add_modifier(Modifier::BOLD)
        } else {
            Style::new().fg(p.fg)
        };
        let value_style = if editing {
            Style::new().fg(p.sel_fg).bg(p.sel_bg)
        } else if o.value(*f).is_empty() {
            Style::new().fg(p.grey)
        } else {
            Style::new().fg(p.fg).add_modifier(Modifier::BOLD)
        };
        let mut spans = vec![
            Span::styled(if selected { "› " } else { "  " }, label_style),
            Span::styled(f.label().to_string(), label_style),
            Span::raw(format!("{pad}  ")),
            Span::styled(shown, value_style),
        ];
        // The hint only for the row you are on: nine hints at once is noise.
        if selected && o.editing.is_none() {
            spans.push(Span::styled(format!("   {}", f.hint()), Style::new().fg(p.grey).add_modifier(Modifier::DIM)));
        }
        lines.push(Line::from(spans));
    }

    lines.push(Line::raw(""));
    const LEAD: &str = "opens as  ";
    lines.push(Line::from(vec![
        Span::styled(LEAD, Style::new().fg(p.grey)),
        Span::styled(clip(&o.preview(), text_w.saturating_sub(LEAD.len())), Style::new().fg(p.accent)),
    ]));
    lines.push(Line::raw(""));
    let keys = if o.editing.is_some() {
        "↵ save · esc discard"
    } else {
        "↵ change · e type · d clear · X clear all · esc close (saved as you go)"
    };
    lines.push(Line::styled(keys, Style::new().fg(p.grey).add_modifier(Modifier::DIM)));

    let h = (lines.len() as u16 + 2).min(body.height.saturating_sub(2));
    let inner = draw_modal(frame, body, w, h, false, p.accent, " How this chat opens ");
    frame.render_widget(Paragraph::new(lines), inner);
}

fn clip(s: &str, w: usize) -> String {
    if s.chars().count() <= w {
        return s.to_string();
    }
    let mut out: String = s.chars().take(w.saturating_sub(1)).collect();
    out.push('…');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn opts() -> Options {
        Options::new("p/1".into(), "abcdef1234567890".into(), "a mission".into(), Launch::default())
    }

    #[test]
    fn enter_cycles_vocabularies_and_wraps() {
        let mut o = opts();
        assert_eq!(o.field(), Field::Model);
        assert_eq!(o.value(Field::Model), "");
        for expected in ["fable", "opus", "sonnet", "haiku", ""] {
            o.primary();
            assert_eq!(o.value(Field::Model), expected);
        }
        // A value Houston does not know about is not lost by cycling from it: it
        // simply starts the cycle again rather than being treated as position -1.
        o.set_text(Field::Model, "claude-opus-5");
        o.primary();
        assert_eq!(o.value(Field::Model), "fable");
    }

    /// Every value the cycles offer must be one the real CLI accepts — these were
    /// checked against `claude --help`, where an earlier version of this list had
    /// invented a `default` permission mode and forgotten `--effort max`.
    #[test]
    fn the_cycles_only_offer_values_claude_documents() {
        const MODELS: &[&str] = &["fable", "opus", "sonnet", "haiku"];
        const EFFORTS: &[&str] = &["low", "medium", "high", "xhigh", "max"];
        const MODES: &[&str] = &["acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan"];
        for v in Field::Model.cycle().iter().filter(|v| !v.is_empty()) {
            assert!(MODELS.contains(v), "unknown model alias: {v}");
        }
        for v in Field::Effort.cycle().iter().filter(|v| !v.is_empty()) {
            assert!(EFFORTS.contains(v), "unknown effort: {v}");
        }
        for v in Field::PermissionMode.cycle().iter().filter(|v| !v.is_empty()) {
            assert!(MODES.contains(v), "unknown permission mode: {v}");
        }
        assert!(Field::Effort.cycle().contains(&"max"), "the CLI offers max; the cycle should too");
    }

    #[test]
    fn permission_cycle_never_reaches_the_dangerous_modes() {
        let mut o = opts();
        o.row = 2;
        let mut seen = Vec::new();
        for _ in 0..10 {
            o.primary();
            seen.push(o.value(Field::PermissionMode));
        }
        assert!(seen.contains(&"plan".to_string()));
        for dangerous in ["bypassPermissions", "dontAsk"] {
            assert!(
                !seen.iter().any(|v| v == dangerous),
                "cycling must not be able to stop Claude asking ({dangerous}): {seen:?}"
            );
        }
        // But it IS reachable deliberately, by typing.
        o.set_text(Field::PermissionMode, "bypassPermissions");
        assert_eq!(o.value(Field::PermissionMode), "bypassPermissions");
    }

    #[test]
    fn worktree_keeps_its_three_states_apart() {
        let mut o = opts();
        o.row = 4;
        assert_eq!(o.value(Field::Worktree), "", "off shows as empty");
        o.primary();
        assert_eq!(o.prefs.worktree, Some(String::new()));
        assert_eq!(o.value(Field::Worktree), "(unnamed)", "on-unnamed must not look like off");
        o.primary();
        assert_eq!(o.prefs.worktree, None);
        // A name comes from typing, and the placeholder must never become one.
        o.begin_edit();
        assert_eq!(o.editing.as_deref(), Some(""));
        o.set_text(Field::Worktree, "spike");
        assert_eq!(o.prefs.worktree.as_deref(), Some("spike"));
        o.prefs.worktree = Some(String::new());
        o.begin_edit();
        assert_eq!(o.editing.as_deref(), Some(""), "the display placeholder is not seeded as text");
    }

    #[test]
    fn flags_toggle_and_lists_split() {
        let mut o = opts();
        o.row = 5; // fork
        o.primary();
        assert!(o.prefs.fork);
        o.primary();
        assert!(!o.prefs.fork);

        o.set_text(Field::AddDirs, "C:\\a , C:\\b ,, ");
        assert_eq!(o.prefs.add_dirs, vec!["C:\\a", "C:\\b"], "empty entries are dropped");
        o.set_text(Field::Extra, "--verbose  --debug");
        assert_eq!(o.prefs.extra, vec!["--verbose", "--debug"]);
        o.set_text(Field::AddDirs, "");
        assert!(o.prefs.add_dirs.is_empty());
    }

    /// The preview is the whole point: it must be the real argument list.
    #[test]
    fn the_preview_is_the_command_that_will_run() {
        let mut o = opts();
        assert_eq!(o.preview(), "claude --resume abcdef12", "no preferences, no arguments");
        o.set_text(Field::Model, "sonnet");
        o.prefs.worktree = Some("spike".into());
        o.prefs.fork = true;
        let expected = format!(
            "claude --resume abcdef12 {}",
            houston_core::launch::args_for(&o.prefs).join(" ")
        );
        assert_eq!(o.preview(), expected);
        assert!(o.preview().contains("--model sonnet") && o.preview().contains("--fork-session"));
    }

    #[test]
    fn the_cursor_cannot_leave_the_field_list() {
        let mut o = opts();
        o.move_row(-5);
        assert_eq!(o.row, 0);
        o.move_row(99);
        assert_eq!(o.row, FIELDS.len() - 1);
        assert_eq!(o.field(), Field::Extra);
    }
}
