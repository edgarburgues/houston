//! The native discovery shell: the `?` which-key help overlay and the `:`
//! command palette, both derived from the command registry (globals + the
//! focused widget's commands). These are CORE — always present, independent
//! of houston-basics — because a one-line footer collapses once plugins pile
//! on, and these are how you actually discover and run everything.

use crate::command::{self, Command};
use crate::App;
use ratatui::{
    layout::Rect,
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, BorderType, Clear, Padding, Paragraph},
    Frame,
};

/// The raised surface behind a modal — a touch lighter than the terminal
/// ground so the panel reads as floating.
const MODAL_BG: Color = Color::Indexed(235);

/// Draw a floating modal box of (w, h) centered in `body`, with a solid thick
/// border and a blanked margin around it that masks the text underneath —
/// giving a sense of relief. Returns the inner content area.
pub(crate) fn draw_modal(
    frame: &mut Frame,
    body: Rect,
    w: u16,
    h: u16,
    top: bool,
    accent: Color,
    title: &str,
) -> Rect {
    let area = centered(body, w, h, top);
    // Relief margin: clear a ring around the box (2 cols, 1 row), so nearby
    // letters vanish and the panel looks lifted off the page.
    let (mx, my) = (2u16, 1u16);
    let ox = area.x.saturating_sub(mx).max(body.x);
    let oy = area.y.saturating_sub(my).max(body.y);
    // Saturating: `area` already sits inside `body`, so the ring can only be
    // clipped, never inverted — but the intermediate `x + width + margin` is one
    // of the few sums here that could top u16 on an absurdly wide surface, and
    // that would panic in a debug build rather than clamp.
    let right = area.x.saturating_add(area.width).saturating_add(mx).min(body.x.saturating_add(body.width));
    let bottom = area.y.saturating_add(area.height).saturating_add(my).min(body.y.saturating_add(body.height));
    frame.render_widget(Clear, Rect::new(ox, oy, right - ox, bottom - oy));
    let block = Block::bordered()
        .border_type(BorderType::Thick)
        .title(title)
        .border_style(Style::new().fg(accent))
        .style(Style::new().bg(MODAL_BG))
        .padding(Padding::horizontal(1));
    let inner = block.inner(area);
    frame.render_widget(block, area);
    inner
}

fn centered(area: Rect, w: u16, h: u16, top_bias: bool) -> Rect {
    let w = w.min(area.width);
    let h = h.min(area.height);
    let x = area.x + (area.width - w) / 2;
    let y = if top_bias {
        area.y + (area.height.saturating_sub(h)) / 4
    } else {
        area.y + (area.height - h) / 2
    };
    Rect::new(x, y, w, h)
}

/// The candidate list for the palette, filtered+ranked by the current query.
pub fn palette_matches(app: &App) -> Vec<Command> {
    let reg = app.registry();
    let q = app.pal_input.trim();
    if q.is_empty() {
        return reg;
    }
    let mut scored: Vec<(usize, Command)> = reg
        .into_iter()
        .filter_map(|c| {
            let hay = format!("{} {}", c.title, c.source);
            command::fuzzy_score(q, &hay).map(|s| (s, c))
        })
        .collect();
    scored.sort_by_key(|(s, _)| *s);
    scored.into_iter().map(|(_, c)| c).collect()
}

/// Lines of the help overlay body (for the renderer and the scroll clamp).
fn help_lines(app: &App) -> Vec<Line<'static>> {
    let p = &app.world.palette;
    let reg = app.registry();
    let mut lines: Vec<Line> = Vec::new();
    for (title, is_widget, rows) in command::sections(&reg) {
        let hstyle = if is_widget {
            Style::new().fg(p.accent).add_modifier(Modifier::BOLD).add_modifier(Modifier::DIM)
        } else {
            Style::new().fg(p.accent).add_modifier(Modifier::BOLD)
        };
        lines.push(Line::styled(title, hstyle));
        let lw = rows.iter().map(|c| c.label.chars().count()).max().unwrap_or(0);
        for c in rows {
            let pad = " ".repeat(lw - c.label.chars().count());
            lines.push(Line::from(vec![
                Span::raw(" "),
                Span::styled(c.label, Style::new().fg(p.accent)),
                Span::raw(format!("{pad}  ")),
                Span::styled(c.title, Style::new().fg(p.fg)),
            ]));
        }
        lines.push(Line::raw(""));
    }
    lines
}

pub fn help_line_count(app: &App) -> usize {
    help_lines(app).len()
}

pub fn render_help(frame: &mut Frame, body: Rect, app: &App) {
    let p = &app.world.palette;
    let lines = help_lines(app);
    let w = 64u16.min(body.width.saturating_sub(6)).max(20);
    let h = ((lines.len() as u16) + 2).min(body.height.saturating_sub(2));
    let inner = draw_modal(frame, body, w, h, false, p.accent, " Help — press a key to run it, esc closes ");
    frame.render_widget(Paragraph::new(lines).scroll((app.help_scroll, 0)), inner);
}

/// The "already open elsewhere" prompt. Says what it knows AND how old that
/// knowledge is: the live snapshot is refreshed on the scan's rhythm, not
/// continuously, so a stale reading is possible and pretending otherwise would
/// make the wrong choice look authoritative.
pub fn render_conflict(frame: &mut Frame, body: Rect, app: &App) {
    let Some(c) = app.conflict_view() else { return };
    let p = &app.world.palette;
    let (name, pid, status, age) = c;
    let mut lines: Vec<Line> = Vec::new();
    // A session with no pid is not on this machine — Claude's registry also lists
    // cloud sessions and disconnected Remote Control ones. Printing "pid 0" would
    // be a fact Houston does not have, and the consequence of attaching is a
    // different one, so both the label and the caption below change.
    let local = pid > 0;
    lines.push(Line::from(vec![
        Span::styled("This chat is already open", Style::new().fg(p.fg).add_modifier(Modifier::BOLD)),
        Span::styled(
            if local { format!("  (pid {pid}, {status})") } else { format!("  (on another host, {status})") },
            Style::new().fg(p.grey),
        ),
    ]));
    if !name.is_empty() {
        lines.push(Line::styled(format!("  {name}"), Style::new().fg(p.grey)));
    }
    lines.push(Line::raw(""));
    lines.push(Line::from(vec![
        Span::styled("f", Style::new().fg(p.accent).add_modifier(Modifier::BOLD)),
        Span::styled("  open a copy instead", Style::new().fg(p.fg)),
        Span::styled("   (--fork-session, leaves the original alone)", Style::new().fg(p.grey)),
    ]));
    lines.push(Line::from(vec![
        Span::styled("↵", Style::new().fg(p.accent).add_modifier(Modifier::BOLD)),
        Span::styled("  attach anyway", Style::new().fg(p.fg)),
        Span::styled(
            if local { "   (two claudes on one transcript)" } else { "   (a local claude on a transcript another host is writing)" },
            Style::new().fg(p.grey),
        ),
    ]));
    lines.push(Line::from(vec![
        Span::styled("esc", Style::new().fg(p.accent).add_modifier(Modifier::BOLD)),
        Span::styled("  cancel", Style::new().fg(p.fg)),
    ]));
    lines.push(Line::raw(""));
    lines.push(Line::styled(
        format!("seen running {age}s ago · L rechecks"),
        Style::new().fg(p.grey).add_modifier(Modifier::DIM),
    ));

    let w = 68u16.min(body.width.saturating_sub(6)).max(24);
    let h = (lines.len() as u16 + 2).min(body.height.saturating_sub(2));
    let inner = draw_modal(frame, body, w, h, false, p.accent, " Already open ");
    frame.render_widget(Paragraph::new(lines), inner);
}

pub fn render_palette(frame: &mut Frame, body: Rect, app: &App) {
    let p = &app.world.palette;
    let matches = palette_matches(app);
    let sel = app.pal_sel.min(matches.len().saturating_sub(1));
    let w = 66u16.min(body.width.saturating_sub(6)).max(24);
    let content_w = w.saturating_sub(4) as usize;

    let max_rows = 12usize.min(body.height.saturating_sub(7) as usize).max(1);
    let start = if sel >= max_rows { sel + 1 - max_rows } else { 0 };

    let mut lines: Vec<Line> = Vec::new();
    lines.push(Line::from(vec![
        Span::styled("› ", Style::new().fg(p.accent).add_modifier(Modifier::BOLD)),
        Span::styled(format!("{}▌", app.pal_input), Style::new().fg(p.fg)),
    ]));
    lines.push(Line::raw(""));
    if matches.is_empty() {
        lines.push(Line::styled("  (no matching command)", Style::new().fg(p.grey)));
    }
    for (i, c) in matches.iter().enumerate().skip(start).take(max_rows) {
        let mut title = c.title.clone();
        if !c.source.is_empty() {
            title.push_str(&format!("  · {}", c.source));
        }
        let label = &c.label;
        let tclip: String = title.chars().take(content_w.saturating_sub(label.chars().count() + 2)).collect();
        let gap = content_w.saturating_sub(tclip.chars().count() + label.chars().count()).max(1);
        if i == sel {
            let row = format!("{tclip}{}{label}", " ".repeat(gap));
            lines.push(Line::styled(row, Style::new().fg(p.sel_fg).bg(p.sel_bg).add_modifier(Modifier::BOLD)));
        } else {
            lines.push(Line::from(vec![
                Span::styled(tclip, Style::new().fg(p.fg)),
                Span::raw(" ".repeat(gap)),
                Span::styled(label.clone(), Style::new().fg(p.accent)),
            ]));
        }
    }

    let h = (lines.len() as u16 + 2).min(body.height.saturating_sub(2));
    let inner = draw_modal(frame, body, w, h, true, p.accent, " Palette — enter runs · esc closes ");
    frame.render_widget(Paragraph::new(lines), inner);
}

