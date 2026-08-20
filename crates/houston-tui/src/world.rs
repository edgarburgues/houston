//! World — the shared application state every widget reads and mutates.
//! Widgets keep only VIEW state (cursor, scroll); the data lives here, so two
//! widgets can never disagree about which mission is selected. The resolved
//! color Palette lives here too, so every widget styles from one source.

use houston_core::config::Theme;
use houston_core::model::{Meta, Mission};
use houston_core::store::Store;
use ratatui::style::Color;

/// The left-pane filters (programs join in a later phase).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Filter {
    All,
    Pinned,
    Archived,
}

impl Filter {
    pub const ORDER: [Filter; 3] = [Filter::All, Filter::Pinned, Filter::Archived];

    pub fn label(self) -> &'static str {
        match self {
            Filter::All => "◷ All",
            Filter::Pinned => "★ Pinned",
            Filter::Archived => "⌁ Archived",
        }
    }
}

/// Resolve a config color string to a ratatui Color: a name, an ANSI-256
/// index ("240"), or Reset on anything unrecognized (never fails).
pub fn parse_color(s: &str) -> Color {
    match s.trim().to_ascii_lowercase().as_str() {
        "black" => Color::Black,
        "red" => Color::Red,
        "green" => Color::Green,
        "yellow" => Color::Yellow,
        "blue" => Color::Blue,
        "magenta" => Color::Magenta,
        "cyan" => Color::Cyan,
        "gray" | "grey" => Color::Gray,
        "darkgray" | "darkgrey" => Color::DarkGray,
        "lightred" => Color::LightRed,
        "lightgreen" => Color::LightGreen,
        "lightyellow" => Color::LightYellow,
        "lightblue" => Color::LightBlue,
        "lightmagenta" => Color::LightMagenta,
        "lightcyan" => Color::LightCyan,
        "white" => Color::White,
        "reset" | "" => Color::Reset,
        other => other.parse::<u8>().map(Color::Indexed).unwrap_or(Color::Reset),
    }
}

/// The resolved palette: config strings turned into ratatui colors once.
#[derive(Debug, Clone, Copy)]
pub struct Palette {
    pub fg: Color,
    pub dim: Color,
    pub grey: Color,
    pub accent: Color,
    pub sel_fg: Color,
    pub sel_bg: Color,
    pub border: Color,
    pub border_focus: Color,
    pub header_fg: Color,
    pub header_bg: Color,
}

impl Palette {
    pub fn from_theme(t: &Theme) -> Self {
        Palette {
            fg: parse_color(&t.fg),
            dim: parse_color(&t.dim),
            grey: parse_color(&t.grey),
            accent: parse_color(&t.accent),
            sel_fg: parse_color(&t.sel_fg),
            sel_bg: parse_color(&t.sel_bg),
            border: parse_color(&t.border),
            border_focus: parse_color(&t.border_focus),
            header_fg: parse_color(&t.header_fg),
            header_bg: parse_color(&t.header_bg),
        }
    }
}

/// A change to Houston's own config, asked for by a widget and applied by `App`.
///
/// Deliberately a small enum of NAMED intentions rather than a generic
/// "set this JSON path": a typed variant cannot produce a config that fails to
/// parse, so there is nothing to validate. If a free-text row is ever added here,
/// it must apply the edit to a clone, round-trip it through
/// `serde_json::from_value::<Config>()`, and refuse on error — the same parse the
/// loader will do. Until then, claiming a validation step would be theatre.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ConfigRequest {
    /// Swap Houston's whole palette between the built-in presets.
    ThemePreset(&'static str),
}

pub struct World {
    /// Full deduped scan, most recent first. Empty until the async scan lands.
    pub missions: Vec<Mission>,
    /// Mutable per-mission metadata + programs.
    pub store: Store,
    pub filter: Filter,
    /// Indices into `missions` for the active filter, pinned first.
    pub visible: Vec<usize>,
    /// Cursor position within `visible`.
    pub cursor: usize,
    /// True until the first scan completes (widgets show a loading hint).
    pub scanning: bool,
    /// A transient one-line message for the footer (an export path, an error).
    /// Cleared by the next action that sets it.
    pub status: String,
    /// Sessions running right now, as of `live_at`. A SNAPSHOT, not live truth:
    /// the query costs ~1.2 s, so it is taken on the scan's rhythm rather than on
    /// any UI cadence, and everything that reads it must tolerate being a little
    /// behind.
    pub live: Vec<houston_core::agents::Live>,
    /// When `live` was taken. `None` = never asked yet (which is different from
    /// "asked and nothing is running", and the UI must not conflate them).
    pub live_at: Option<std::time::Instant>,
    /// A live query is in flight; a second key press must not start another.
    pub live_loading: bool,
    /// A key handler asked for a refresh. Key handlers cannot spawn work (they
    /// have no channel), so they raise this and the event loop acts on it.
    pub live_wanted: bool,
    /// A widget asked for Houston's OWN configuration to change.
    ///
    /// Widgets get `&mut World`, and `World` deliberately has no `cfg` — that
    /// lives in `App`, which is the only thing allowed to write `config-v2.json`
    /// and to rebuild what a changed config affects. So the Settings pane raises a
    /// request here and the event loop applies it, the same shape as
    /// `live_wanted`. Account-scope settings need none of this: their owners
    /// (`policy`, `claude_settings`, `hooks`) are plain core functions the widget
    /// can call itself.
    pub config_request: Option<ConfigRequest>,
    /// Registered accounts (populated by the accounts widget when mounted).
    pub accounts: Vec<houston_core::accounts::Account>,
    /// Cursor within `accounts` (the account the Accounts view acts on).
    pub acc_cursor: usize,
    /// Resolved theme colors.
    pub palette: Palette,
}

impl World {
    pub fn new(store: Store, palette: Palette) -> Self {
        World {
            missions: Vec::new(),
            store,
            filter: Filter::All,
            visible: Vec::new(),
            cursor: 0,
            scanning: true,
            status: String::new(),
            live: Vec::new(),
            live_at: None,
            live_loading: false,
            live_wanted: false,
            config_request: None,
            accounts: Vec::new(),
            acc_cursor: 0,
            palette,
        }
    }

    /// The running session for a mission id, if the last snapshot saw one.
    pub fn live_of(&self, mission_id: &str) -> Option<&houston_core::agents::Live> {
        self.live.iter().find(|l| l.session_id == mission_id)
    }

    /// How stale the live snapshot is, in seconds. `None` = never taken.
    pub fn live_age_secs(&self) -> Option<u64> {
        self.live_at.map(|t| t.elapsed().as_secs())
    }

    /// The account under the accounts cursor, if any.
    pub fn selected_account(&self) -> Option<&houston_core::accounts::Account> {
        self.accounts.get(self.acc_cursor)
    }

    pub fn move_acc_cursor(&mut self, delta: i32) {
        if self.accounts.is_empty() {
            return;
        }
        let last = self.accounts.len() as i32 - 1;
        self.acc_cursor = (self.acc_cursor as i32 + delta).clamp(0, last) as usize;
    }

    pub fn meta_of(&self, m: &Mission) -> Meta {
        self.store.meta_of(&m.key())
    }

    /// Recompute `visible` for the current filter. Pinned-first is inviolable
    /// in All (explicit user intent); within groups, scan order (recency).
    pub fn rebuild(&mut self) {
        let mut pinned = Vec::new();
        let mut rest = Vec::new();
        for (i, m) in self.missions.iter().enumerate() {
            let meta = self.store.meta_of(&m.key());
            let wanted = match self.filter {
                Filter::All => !meta.archived,
                Filter::Pinned => meta.pinned,
                Filter::Archived => meta.archived,
            };
            if !wanted {
                continue;
            }
            if meta.pinned {
                pinned.push(i);
            } else {
                rest.push(i);
            }
        }
        pinned.extend(rest);
        self.visible = pinned;
        if self.cursor >= self.visible.len() {
            self.cursor = self.visible.len().saturating_sub(1);
        }
    }

    /// The mission under the cursor, if any.
    pub fn selected(&self) -> Option<&Mission> {
        self.visible.get(self.cursor).map(|&i| &self.missions[i])
    }

    pub fn set_filter(&mut self, f: Filter) {
        if self.filter != f {
            self.filter = f;
            self.cursor = 0;
            self.rebuild();
        }
    }

    pub fn move_cursor(&mut self, delta: i32) {
        if self.visible.is_empty() {
            return;
        }
        let last = self.visible.len() as i32 - 1;
        let next = (self.cursor as i32 + delta).clamp(0, last);
        self.cursor = next as usize;
    }
}

#[cfg(test)]
// Visible to the crate's other test modules: `test_palette` and `test_world` are
// shared fixtures, not this module's private business.
pub(crate) mod tests {
    use super::*;

    pub fn test_palette() -> Palette {
        Palette::from_theme(&Theme::default())
    }

    /// An empty World on a throwaway store, for widget tests elsewhere in the
    /// crate that only need *a* world to render or tick against.
    pub fn test_world() -> World {
        let tmp = tempfile::tempdir().unwrap();
        let store = Store::load_from(tmp.path().to_path_buf()).unwrap();
        std::mem::forget(tmp); // outlive the borrow; it is a temp dir
        World::new(store, test_palette())
    }

    fn world_with(titles: &[(&str, bool, bool)]) -> World {
        let tmp = tempfile::tempdir().unwrap();
        let mut w = World::new(
            Store::load_from(tmp.path().to_path_buf()).unwrap(),
            test_palette(),
        );
        for (i, (t, pinned, archived)) in titles.iter().enumerate() {
            let m = Mission {
                id: format!("m{i}"),
                project: "p".into(),
                title: t.to_string(),
                ..Default::default()
            };
            if *pinned {
                w.store.toggle_pin(&m.key()).unwrap();
            }
            if *archived {
                w.store.toggle_archive(&m.key()).unwrap();
            }
            w.missions.push(m);
        }
        std::mem::forget(tmp);
        w.rebuild();
        w
    }

    #[test]
    fn parse_color_names_indices_and_fallback() {
        assert_eq!(parse_color("white"), Color::White);
        assert_eq!(parse_color("DarkGray"), Color::DarkGray);
        assert_eq!(parse_color("240"), Color::Indexed(240));
        assert_eq!(parse_color("banana"), Color::Reset);
    }

    #[test]
    fn all_hides_archived_and_pins_first() {
        let w = world_with(&[("a", false, false), ("b", true, false), ("c", false, true)]);
        let titles: Vec<_> = w.visible.iter().map(|&i| w.missions[i].title.as_str()).collect();
        assert_eq!(titles, vec!["b", "a"]);
    }

    #[test]
    fn filters_switch_and_clamp_cursor() {
        let mut w = world_with(&[("a", false, false), ("b", true, false), ("c", false, true)]);
        w.cursor = 1;
        w.set_filter(Filter::Archived);
        let titles: Vec<_> = w.visible.iter().map(|&i| w.missions[i].title.as_str()).collect();
        assert_eq!(titles, vec!["c"]);
        assert_eq!(w.cursor, 0);
        assert_eq!(w.selected().unwrap().title, "c");
    }

    #[test]
    fn cursor_moves_clamped() {
        let mut w = world_with(&[("a", false, false), ("b", false, false)]);
        w.move_cursor(5);
        assert_eq!(w.cursor, 1);
        w.move_cursor(-9);
        assert_eq!(w.cursor, 0);
    }
}
