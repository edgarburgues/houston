//! houston-tui — the layout engine for Houston 2.0.
//!
//! The screen is a TREE of containers. A `Node` is a `Split` (a direction +
//! sized children) or a `Pane` (a single `Widget`). The engine owns the chrome
//! — borders, titles, focus — and hands each widget its inner area. The tree
//! is built from `houston_core::config` (data), rendered in the theme palette,
//! and persisted back when the user resizes: everything is config.
//!
//! Input: keyboard (tab focus, per-widget keys) AND mouse — click focuses and
//! forwards to the widget, the wheel scrolls the HOVERED pane, and DRAGGING a
//! border between two panes resizes them (persisted to config on release).

#![allow(clippy::doc_lazy_continuation)]
pub mod basics;
pub mod build;
pub mod command;
pub mod options;
pub mod plugin_widget;
pub mod probe;
pub mod settings_view;
pub mod shell;
pub mod widgets;
pub mod world;

pub use plugin_widget::PluginWidget;

use houston_core::config::Config;
use ratatui::{
    crossterm::event::{
        self, DisableMouseCapture, EnableMouseCapture, Event, KeyCode, KeyEvent, KeyEventKind,
        KeyModifiers, MouseButton, MouseEventKind,
    },
    crossterm::execute,
    layout::{Constraint, Direction, Layout, Rect},
    style::{Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Padding, Paragraph},
    Frame,
};
use std::io::stdout;
use std::sync::mpsc;
use std::time::Duration;
use world::{Palette, World};

/// Minimum cells a pane keeps when resized (border + a little content).
const MIN_PANE: u16 = 4;

/// A widget fills the inner area of a pane. Built-ins and plugin widgets alike
/// implement this. View state (scroll) lives in the widget; shared data and
/// the palette live in the World.
pub trait Widget {
    /// Stable id for config serialization (matches the layout's `widget`).
    fn id(&self) -> &str;
    fn title(&self, world: &World) -> String;
    fn render(&self, area: Rect, frame: &mut Frame, world: &World, focused: bool);
    fn on_key(&mut self, key: char, world: &mut World) -> bool;
    fn on_click(&mut self, row: u16, col: u16, world: &mut World);
    fn on_scroll(&mut self, up: bool, world: &mut World);
    fn post_render(&mut self, _area: Rect, _world: &World, _focused: bool) {}
    /// The commands this widget advertises to the `?` overlay and the `:`
    /// palette while it is focused. Default: none.
    fn commands(&self) -> Vec<crate::command::Command> {
        Vec::new()
    }

    /// Receive this pane's `settings` block from the layout. Most widgets need
    /// nothing and ignore it.
    fn configure(&mut self, _settings: &serde_json::Value) {}

    /// `Enter` while this pane is focused. Return `true` to claim it.
    ///
    /// `on_key` only carries characters, so without this a pane could advertise
    /// `↵` and never receive it — the app's global `Enter` would resume a chat
    /// instead, which is exactly what the Settings tab did until this existed.
    /// Panes that have nothing to do with Enter say nothing and the resume keeps
    /// its one keystroke.
    fn on_enter(&mut self, _world: &mut World) -> bool {
        false
    }

    /// True while this widget is taking typed input.
    ///
    /// `on_key` only ever receives printable characters, so a pane that edits text
    /// needs the app to route the three keys that are not characters. Saying so
    /// here is also what stops `Enter` resuming a chat while somebody is typing a
    /// value into a pane.
    fn editing(&self) -> bool {
        false
    }
    /// `Enter` while `editing()`.
    fn on_edit_commit(&mut self, _world: &mut World) {}
    /// `Esc` while `editing()` — leave the edit, not the pane.
    fn on_edit_cancel(&mut self) {}
    /// `Backspace` while `editing()`.
    fn on_edit_backspace(&mut self) {}

    /// Hand those settings back for persistence. **A configurable widget MUST
    /// implement this**: `build::to_layout` rebuilds the layout from the live
    /// tree on every save — including the save a border drag triggers — so a
    /// widget that stays silent here gets its configuration erased the first
    /// time the user resizes anything.
    fn settings(&self) -> Option<serde_json::Value> {
        None
    }
}

/// A split child's size. Own type (mirrors config::SizeSpec) so the live tree
/// can be mutated by resize and serialized back.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Size {
    Fixed(u16),
    Percent(u16),
    Fill(u16),
}

impl Size {
    fn to_constraint(self) -> Constraint {
        match self {
            Size::Fixed(n) => Constraint::Length(n),
            Size::Percent(n) => Constraint::Percentage(n),
            Size::Fill(n) => Constraint::Fill(n),
        }
    }
}

/// A container tree node.
pub enum Node {
    Split { dir: Direction, children: Vec<(Size, Node)> },
    Pane(Box<dyn Widget>),
}

fn collect_rects(node: &Node, area: Rect, out: &mut Vec<Rect>) {
    match node {
        Node::Pane(_) => out.push(area),
        Node::Split { dir, children } => {
            let cons: Vec<Constraint> = children.iter().map(|(s, _)| s.to_constraint()).collect();
            for ((_, child), a) in children.iter().zip(Layout::new(*dir, cons).split(area).iter()) {
                collect_rects(child, *a, out);
            }
        }
    }
}

fn widgets_mut<'a>(node: &'a mut Node, out: &mut Vec<&'a mut Box<dyn Widget>>) {
    match node {
        Node::Pane(w) => out.push(w),
        Node::Split { children, .. } => {
            for (_, child) in children {
                widgets_mut(child, out);
            }
        }
    }
}

fn pane_count(node: &Node) -> usize {
    match node {
        Node::Pane(_) => 1,
        Node::Split { children, .. } => children.iter().map(|(_, n)| pane_count(n)).sum(),
    }
}

/// Each pane's advertised commands, in DFS (= focus) order.
fn collect_widget_cmds(node: &Node, out: &mut Vec<Vec<command::Command>>) {
    match node {
        Node::Pane(w) => out.push(w.commands()),
        Node::Split { children, .. } => {
            for (_, c) in children {
                collect_widget_cmds(c, out);
            }
        }
    }
}

/// Widget ids in DFS (= focus) order.
fn widget_ids(node: &Node, out: &mut Vec<String>) {
    match node {
        Node::Pane(w) => out.push(w.id().to_string()),
        Node::Split { children, .. } => {
            for (_, c) in children {
                widget_ids(c, out);
            }
        }
    }
}

/// Header (1) above, footer (1) below — the one geometry the renderer and the
/// hit-testing share.
fn body_area(area: Rect) -> Rect {
    Rect::new(area.x, area.y + 1, area.width, area.height.saturating_sub(2))
}

fn pane_at(root: &Node, area: Rect, x: u16, y: u16) -> Option<usize> {
    let mut rects = Vec::new();
    collect_rects(root, body_area(area), &mut rects);
    rects.iter().position(|r| x >= r.x && x < r.x + r.width && y >= r.y && y < r.y + r.height)
}

fn inner(r: Rect) -> Rect {
    Rect::new(r.x + 1, r.y + 1, r.width.saturating_sub(2), r.height.saturating_sub(2))
}

// --- resize: split boundaries ------------------------------------------------

/// A draggable boundary between children `idx` and `idx+1` of the split at
/// `path`, with the current rendered cell lengths of the two neighbors.
struct Boundary {
    path: Vec<usize>,
    idx: usize,
    dir: Direction,
    coord: u16,
    cross_lo: u16,
    cross_hi: u16,
    len_a: u16,
    len_b: u16,
}

fn collect_boundaries(node: &Node, area: Rect, path: &mut Vec<usize>, out: &mut Vec<Boundary>) {
    if let Node::Split { dir, children } = node {
        let cons: Vec<Constraint> = children.iter().map(|(s, _)| s.to_constraint()).collect();
        let areas = Layout::new(*dir, cons).split(area);
        for i in 0..children.len().saturating_sub(1) {
            let a = areas[i];
            let (coord, lo, hi, len_a, len_b) = match dir {
                Direction::Horizontal => {
                    (a.x + a.width, area.y, area.y + area.height, areas[i].width, areas[i + 1].width)
                }
                Direction::Vertical => {
                    (a.y + a.height, area.x, area.x + area.width, areas[i].height, areas[i + 1].height)
                }
            };
            out.push(Boundary { path: path.clone(), idx: i, dir: *dir, coord, cross_lo: lo, cross_hi: hi, len_a, len_b });
        }
        for (i, (_, child)) in children.iter().enumerate() {
            path.push(i);
            collect_boundaries(child, areas[i], path, out);
            path.pop();
        }
    }
}

/// The boundary a click at (x, y) lands on (within 1 cell of the divider).
fn boundary_at(bs: &[Boundary], x: u16, y: u16) -> Option<usize> {
    bs.iter().position(|b| {
        let (main, cross) = match b.dir {
            Direction::Horizontal => (x, y),
            Direction::Vertical => (y, x),
        };
        main.abs_diff(b.coord) <= 1 && cross >= b.cross_lo && cross < b.cross_hi
    })
}

/// Walk a stored path to the split it names. Every step is CHECKED: the path in
/// an in-flight `Drag` was captured against an earlier tree, and a panic here
/// would abort the process with the terminal still in raw mode — a mangled
/// shell is a much worse outcome than a resize that quietly does nothing.
fn split_children_mut<'a>(root: &'a mut Node, path: &[usize]) -> Option<&'a mut Vec<(Size, Node)>> {
    let mut node = root;
    for &i in path {
        match node {
            Node::Split { children, .. } => node = &mut children.get_mut(i)?.1,
            Node::Pane(_) => return None,
        }
    }
    match node {
        Node::Split { children, .. } => Some(children),
        Node::Pane(_) => None,
    }
}

/// Active drag: which boundary, and the neighbor sizes when the drag began.
struct Drag {
    path: Vec<usize>,
    idx: usize,
    dir: Direction,
    start: u16,
    base_a: u16,
    base_b: u16,
}

/// The application: a layout tree, focus, the shared World, and the config it
/// persists to.
pub struct App {
    pub root: Node,
    pub focused: usize,
    pub panes: usize,
    pub world: World,
    pub cfg: Config,
    drag: Option<Drag>,
    dirty: bool,
    // Native discovery shell (core, plugin-independent).
    help: bool,
    help_scroll: u16,
    pal: bool,
    pal_input: String,
    pal_sel: usize,
    // Tabs: top-level views. The ACTIVE tab's state lives in the fields above
    // (root/focused/panes); `tabs` holds every tab, and the active slot is
    // synced from the live fields only on switch/persist.
    tabs: Vec<TabState>,
    active: usize,
    /// Last left-click (time, pane, absolute row), for double-click detection.
    last_click: Option<(std::time::Instant, usize, u16)>,
    /// An action to perform after the current event, outside the draw cycle.
    pending: Option<Action>,
    /// Set when `Enter` lands on a mission that is already open elsewhere: the
    /// resume is held until the user says how to proceed.
    conflict: Option<Conflict>,
    /// The `o` launch-options overlay, when open.
    opts: Option<options::Options>,
}

/// A resume that would attach to a session another claude is already running.
///
/// This is the one place a prompt sits in front of `Enter`, which is otherwise a
/// one-keystroke resume on purpose. It earns the exception because the
/// alternative is two processes appending to one transcript — and the fix
/// (`--fork-session`) is a different command, not a retry.
struct Conflict {
    id: String,
    cwd: String,
    path: String,
    prefs: houston_core::model::Launch,
    /// What we knew about the running session, and how old that reading is.
    live: houston_core::agents::Live,
    age_secs: u64,
}

/// An interaction the event loop must perform outside the draw cycle (it needs
/// to suspend the terminal).
enum Action {
    /// Resume a Claude session — account auto-balanced (v1 behavior), opened
    /// with the mission's stored launch preferences. The prefs travel with the
    /// action because the store lives here, not in `launch`: core builds the
    /// command, the UI decides what the mission wants.
    Resume { id: String, cwd: String, path: String, prefs: houston_core::model::Launch },
    /// Launch a NEW Claude session forced onto a specific account.
    Launch { id: String, config_dir: Option<std::path::PathBuf> },
}

/// The keys a pane taking typed input needs and `on_key(char)` cannot deliver.
enum EditKey {
    Commit,
    Cancel,
    Backspace,
}

/// How close two clicks must be (same pane + row) to count as a double-click.
const DOUBLE_CLICK: std::time::Duration = std::time::Duration::from_millis(400);

/// Houston's own tab. Named here so the header, the key and the tab all agree.
const SETTINGS_TAB: &str = "Settings";

/// One tab's stored state (name + its container tree). The active tab's slot
/// is stale while its live copy sits in `App.root`/`focused`/`panes`.
struct TabState {
    name: String,
    root: Node,
    focused: usize,
    panes: usize,
    /// Houston's own tab, not one from the config. It exists in every install,
    /// renders on the right of the bar, and — critically — is NOT written back to
    /// `config-v2.json` on save: persisting it would invent a tab the user never
    /// asked for and, with the default single-tab config, would flip `persist`
    /// into its multi-tab branch and rewrite the file's shape.
    synthetic: bool,
}

impl TabState {
    /// A cheap inert stand-in left in a slot while its real state is live.
    fn placeholder(name: String) -> Self {
        TabState {
            name,
            root: Node::Pane(Box::new(widgets::MissingWidget::default())),
            focused: 0,
            panes: 0,
            synthetic: false,
        }
    }
}

impl App {
    pub fn from_config(cfg: Config, store: houston_core::store::Store) -> Self {
        Self::from_config_with_plugins(cfg, store, &houston_core::plugin::discover())
    }

    pub fn from_config_with_plugins(
        cfg: Config,
        store: houston_core::store::Store,
        plugins: &[houston_core::plugin::Plugin],
    ) -> Self {
        let palette = Palette::from_theme(&cfg.theme);
        // Build every tab's tree; each starts focused on its Missions pane.
        let mut tabs: Vec<TabState> = cfg
            .resolved_tabs()
            .into_iter()
            .map(|(name, layout)| {
                let root = build::build_tree(&layout, plugins);
                let panes = pane_count(&root);
                let mut ids = Vec::new();
                widget_ids(&root, &mut ids);
                let focused = ids.iter().position(|i| i == "missions").unwrap_or(0);
                TabState { name, root, focused, panes, synthetic: false }
            })
            .collect();
        // Houston's own Settings tab, appended after the configured ones and
        // reached with `0`. Core, not a plugin, and never persisted.
        tabs.push(TabState {
            name: SETTINGS_TAB.into(),
            root: Node::Pane(Box::<settings_view::SettingsWidget>::default()),
            focused: 0,
            panes: 1,
            synthetic: true,
        });
        // Make tab 0 the live/active tab, leaving a placeholder in its slot.
        let active = 0;
        let name0 = tabs[active].name.clone();
        let live = std::mem::replace(&mut tabs[active], TabState::placeholder(name0));
        let mut world = World::new(store, palette);
        // The account list is cheap (a local registry read) and the accounts
        // view + resume balancing both need it available up front.
        world.accounts = houston_core::accounts::load().unwrap_or_default();
        App {
            root: live.root,
            focused: live.focused,
            panes: live.panes,
            world,
            cfg,
            drag: None,
            dirty: false,
            help: false,
            help_scroll: 0,
            pal: false,
            pal_input: String::new(),
            pal_sel: 0,
            tabs,
            active,
            last_click: None,
            pending: None,
            conflict: None,
            opts: None,
        }
    }

    /// Queue a resume of the selected mission — the account is auto-balanced at
    /// launch time (v1 behavior), so this never blocks the UI.
    fn begin_resume(&mut self) {
        let Some(m) = self.world.selected() else { return };
        let meta = self.world.meta_of(m);
        let cwd = if meta.cwd_override.is_empty() { m.cwd.clone() } else { meta.cwd_override.clone() };
        // Already open somewhere? Ask, instead of quietly starting a second
        // claude on the same transcript.
        if let Some(l) = self.world.live_of(&m.id) {
            self.conflict = Some(Conflict {
                id: m.id.clone(),
                cwd,
                path: m.path.clone(),
                prefs: meta.launch.clone(),
                live: l.clone(),
                age_secs: self.world.live_age_secs().unwrap_or(0),
            });
            return;
        }
        self.pending =
            Some(Action::Resume { id: m.id.clone(), cwd, path: m.path.clone(), prefs: meta.launch.clone() });
    }

    /// Open the launch-options overlay on the selected mission.
    fn begin_options(&mut self) {
        let Some(m) = self.world.selected() else { return };
        let meta = self.world.meta_of(m);
        self.opts =
            Some(options::Options::new(m.key(), m.id.clone(), m.title.clone(), meta.launch.clone()));
    }

    /// Persist the overlay's preferences. Called after every change rather than
    /// on close: pinning and tagging already work that way, and it means `esc`
    /// has exactly one meaning instead of being half a cancel.
    fn save_options(&mut self) {
        let Some(o) = self.opts.as_ref() else { return };
        let (key, prefs) = (o.key.clone(), o.prefs.clone());
        if let Err(e) = self.world.store.set_launch(&key, prefs) {
            self.world.status = format!("could not save launch options: {e}");
        }
    }

    /// What the conflict prompt should say: (session name, pid, status, age in
    /// seconds). `None` when no prompt is open. Exposed as plain data so the
    /// renderer never reaches into `Conflict` itself.
    pub fn conflict_view(&self) -> Option<(String, i64, String, u64)> {
        let c = self.conflict.as_ref()?;
        Some((c.live.name.clone(), c.live.pid, c.live.status.clone(), c.age_secs))
    }

    /// Resolve the conflict prompt: fork into a copy, attach anyway, or cancel.
    fn resolve_conflict(&mut self, fork: bool) {
        let Some(c) = self.conflict.take() else { return };
        let mut prefs = c.prefs;
        if fork {
            prefs.fork = true;
        }
        self.pending = Some(Action::Resume { id: c.id, cwd: c.cwd, path: c.path, prefs });
    }

    /// Queue a fresh launch on the account selected in the Accounts view —
    /// this is how you force an account for a chat (v1's Accounts tab).
    fn begin_launch(&mut self) {
        let Some(a) = self.world.selected_account() else { return };
        self.pending = Some(Action::Launch { id: a.id.clone(), config_dir: a.resolve_config_dir() });
    }

    /// Apply a config change a widget asked for.
    ///
    /// Typed variants only (see `ConfigRequest`), so there is no way to produce a
    /// config that fails to parse and therefore nothing to validate. The theme is
    /// applied LIVE by recomputing the palette; a layout or tab change would need
    /// the whole tree rebuilt, which would discard every pane's state, so if such a
    /// request is ever added it must say "next start" in the UI rather than pretend.
    fn apply_config_request(&mut self, req: world::ConfigRequest) {
        match req {
            world::ConfigRequest::ThemePreset(name) => {
                self.cfg.theme = match name {
                    "blue" => houston_core::config::Theme::blue(),
                    _ => houston_core::config::Theme::default(),
                };
                self.world.palette = world::Palette::from_theme(&self.cfg.theme);
                self.dirty = true;
                self.persist();
                self.world.status = format!("Houston colour: {name}");
            }
        }
    }

    /// Whether the focused pane is currently taking typed input.
    fn focused_is_editing(&mut self) -> bool {
        let i = self.focused;
        let mut ws = Vec::new();
        widgets_mut(&mut self.root, &mut ws);
        ws.get(i).map(|w| w.editing()).unwrap_or(false)
    }

    /// Give the focused pane the chance to claim `Enter`. `true` = it did.
    ///
    /// Same field destructuring as `edit_key`, and for the same reason: the widget
    /// is in `root` and needs `&mut World`, which are disjoint fields.
    fn offer_enter(&mut self) -> bool {
        let App { root, world, focused, .. } = self;
        let mut ws = Vec::new();
        widgets_mut(root, &mut ws);
        ws.into_iter().nth(*focused).map(|w| w.on_enter(world)).unwrap_or(false)
    }

    /// Hand the focused widget one of the keys `route_key` cannot carry (Enter,
    /// Esc, Backspace are not characters).
    ///
    /// Destructures `App` rather than calling a helper method: the widget lives in
    /// `root` and needs `&mut World`, and those are two disjoint fields — a method
    /// returning the widget would borrow all of `self` and make the second borrow
    /// impossible.
    fn edit_key(&mut self, which: EditKey) {
        let App { root, world, focused, .. } = self;
        let mut ws = Vec::new();
        widgets_mut(root, &mut ws);
        let Some(w) = ws.into_iter().nth(*focused) else { return };
        match which {
            EditKey::Commit => w.on_edit_commit(world),
            EditKey::Cancel => w.on_edit_cancel(),
            EditKey::Backspace => w.on_edit_backspace(),
        }
    }

    /// The id of the currently focused pane's widget.
    fn focused_widget_id(&self) -> Option<String> {
        let mut ids = Vec::new();
        widget_ids(&self.root, &mut ids);
        ids.into_iter().nth(self.focused)
    }

    /// Swap the live tab out to its slot and a target tab in. No-op for the
    /// current tab or an out-of-range index.
    fn switch_tab(&mut self, i: usize) {
        if i == self.active || i >= self.tabs.len() {
            return;
        }
        let cur_name = self.tabs[self.active].name.clone();
        // The flag travels with the slot: stashing the live tree back must not
        // turn the Settings tab into a configured one (persist would then write
        // it) nor the reverse.
        let cur_synthetic = self.tabs[self.active].synthetic;
        let placeholder = Node::Pane(Box::new(widgets::MissingWidget::default()));
        self.tabs[self.active] = TabState {
            name: cur_name,
            root: std::mem::replace(&mut self.root, placeholder),
            focused: self.focused,
            panes: self.panes,
            synthetic: cur_synthetic,
        };
        let name_i = self.tabs[i].name.clone();
        let synthetic_i = self.tabs[i].synthetic;
        let mut ph = TabState::placeholder(name_i);
        ph.synthetic = synthetic_i;
        let t = std::mem::replace(&mut self.tabs[i], ph);
        self.root = t.root;
        self.focused = t.focused;
        self.panes = t.panes;
        self.active = i;
        self.drag = None; // any in-flight resize belonged to the old tree
    }

    /// The configured tabs, in order — the ones `]`, `[` and the digits move
    /// between. Houston's own Settings tab is deliberately NOT among them: it is
    /// not a view of your chats, it is reached with `0`, and cycling into it would
    /// take a key away from whatever widget wanted `]` on a one-view layout.
    fn view_tabs(&self) -> Vec<usize> {
        self.tabs.iter().enumerate().filter(|(_, t)| !t.synthetic).map(|(i, _)| i).collect()
    }

    fn cycle_tab(&mut self, forward: bool) {
        let views = self.view_tabs();
        if views.len() < 2 {
            return;
        }
        // Where we are among the views; from Settings, cycling re-enters at the
        // first view rather than doing nothing.
        let here = views.iter().position(|&i| i == self.active);
        let next = match (here, forward) {
            (Some(pos), true) => (pos + 1) % views.len(),
            (Some(pos), false) => (pos + views.len() - 1) % views.len(),
            (None, _) => 0,
        };
        self.switch_tab(views[next]);
    }

    fn next_tab(&mut self) {
        self.cycle_tab(true);
    }
    fn prev_tab(&mut self) {
        self.cycle_tab(false);
    }

    /// The command registry for the current state: the always-present globals
    /// plus the focused widget's own commands. Feeds `?` and `:`.
    pub fn registry(&self) -> Vec<command::Command> {
        let mut reg = command::global_commands();
        if self.tabs.len() > 1 {
            reg.push(command::Command::core(&["]"], "]", "next tab", "Navigate", false));
            reg.push(command::Command::core(&["["], "[", "previous tab", "Navigate", false));
        }
        if self.tabs.iter().any(|t| t.name.eq_ignore_ascii_case("accounts")) {
            reg.push(command::Command::core(&["A"], "A", "accounts view", "Navigate", false));
        }
        let mut per_pane = Vec::new();
        collect_widget_cmds(&self.root, &mut per_pane);
        if let Some(cmds) = per_pane.into_iter().nth(self.focused) {
            reg.extend(cmds);
        }
        reg
    }

    fn focus_next(&mut self) {
        if self.panes > 0 {
            self.focused = (self.focused + 1) % self.panes;
        }
    }
    fn focus_prev(&mut self) {
        if self.panes > 0 {
            self.focused = (self.focused + self.panes - 1) % self.panes;
        }
    }

    /// Apply a drag to the tree: the boundary follows the cursor, moving cells
    /// between the two neighbors (both become Fixed — a resize you own).
    fn apply_drag(&mut self, cursor: u16) {
        let Some(d) = &self.drag else { return };
        let total = d.base_a + d.base_b;
        let delta = cursor as i32 - d.start as i32;
        let mut a = (d.base_a as i32 + delta).clamp(MIN_PANE as i32, total as i32 - MIN_PANE as i32);
        if total < 2 * MIN_PANE {
            a = d.base_a as i32; // too small to split meaningfully
        }
        let b = total as i32 - a;
        let (path, idx) = (d.path.clone(), d.idx);
        if let Some(children) = split_children_mut(&mut self.root, &path) {
            if idx + 1 < children.len() {
                children[idx].0 = Size::Fixed(a as u16);
                children[idx + 1].0 = Size::Fixed(b as u16);
                self.dirty = true;
            }
        }
    }

    /// Persist the layout(s) (structure + sizes) into the config file. One tab
    /// writes back `layout`; multiple tabs write the full `tabs` list. The
    /// active tab's live tree is used in place of its (stale) slot.
    fn persist(&mut self) {
        if !self.dirty {
            return;
        }
        // Only the CONFIGURED tabs are written back. The Settings tab is
        // Houston's own: saving it would invent a tab the user never wrote.
        //
        // The `active` check below is the load-bearing part, and getting it wrong
        // DESTROYED a real layout: `self.root` holds whichever tab is live, so
        // while Settings is open it holds the SETTINGS tree. An earlier version
        // wrote `to_layout(&self.root)` unconditionally in the single-view branch,
        // which replaced the user's whole layout with one `{"widget":"settings"}`
        // pane and cleared their tabs. The rule is: a configured tab's tree comes
        // from `self.root` only when that tab is the active one, and otherwise
        // from its stashed slot — in both branches.
        let active = self.active;
        let live = &self.root;
        let node_of = |idx: usize, t: &TabState| -> houston_core::config::LayoutNode {
            build::to_layout(if idx == active { live } else { &t.root })
        };
        let configured: Vec<(usize, &TabState)> =
            self.tabs.iter().enumerate().filter(|(_, t)| !t.synthetic).collect();
        if configured.len() == 1 {
            let (idx, t) = configured[0];
            self.cfg.layout = node_of(idx, t);
            self.cfg.tabs.clear();
        } else {
            self.cfg.tabs = configured
                .iter()
                .map(|(idx, t)| houston_core::config::TabConfig {
                    name: t.name.clone(),
                    layout: node_of(*idx, t),
                })
                .collect();
        }
        let _ = self.cfg.save();
        self.dirty = false;
    }
}

enum Bg {
    ScanDone(Vec<houston_core::model::Mission>),
    LiveDone(Vec<houston_core::agents::Live>),
}

/// Ask claude which sessions are running, off the render thread.
///
/// Called at start, after a session returns (the one moment the answer is
/// guaranteed to have changed) and on demand — never on a timer: the query costs
/// ~1.2 s of node startup, so polling it at any UI cadence would burn a core to
/// keep a dot up to date.
fn spawn_live_query(tx: &mpsc::Sender<Bg>) {
    let tx = tx.clone();
    std::thread::spawn(move || {
        // `refresh` rather than `list`: it writes the snapshot, so the NEXT start
        // paints its markers instantly instead of waiting 1.2 s again.
        let live = houston_core::agents::refresh(houston_core::agents::DEFAULT_TIMEOUT);
        // A closed channel means the TUI is gone; dropping the answer is right.
        let _ = tx.send(Bg::LiveDone(live));
    });
}

/// Run the TUI: load config, build the tree, capture the mouse, scan off-
/// thread, loop, persist and restore.
/// Put the console back the way a shell expects it: cooked mode, no mouse
/// capture, no alternate screen.
///
/// Houston does this on the way out, but it cannot do it when it is **killed** —
/// on Windows a TerminateProcess runs no cleanup at all — and the state it leaves
/// behind is genuinely confusing rather than merely untidy: the shell keeps
/// receiving mouse movement as escape sequences, so moving the pointer types
/// garbage. So this runs on the way IN as well, and is exposed as
/// `houston reset-terminal` for a shell that is already in that state.
pub fn restore_terminal() {
    let _ = execute!(stdout(), DisableMouseCapture);
    // `ratatui::restore` leaves the alternate screen and disables raw mode. Both
    // are no-ops when they were never enabled.
    ratatui::restore();
}

pub fn run() -> anyhow::Result<()> {
    // A previous instance may have been killed mid-draw (an install that had to
    // replace a locked binary used to do exactly that). Start from a known
    // console state instead of inheriting a broken one.
    restore_terminal();
    let cfg = Config::load();
    let store = houston_core::store::Store::load()?;
    let plugins = houston_core::plugin::discover();
    // Heal the shared data links at startup (v1 did the same), before the scan
    // reads projects/ through them.
    let _ = houston_core::heal::heal(&houston_core::accounts::load().unwrap_or_default());
    let mut app = App::from_config_with_plugins(cfg, store, &plugins);

    // The markers appear on the FIRST frame, from the last snapshot, while the
    // real query runs behind them. Painting nothing for 1.2 s and then adding dots
    // reads as a glitch; painting a reading with an age is just true.
    let (cached, age) = houston_core::agents::read_cached();
    app.world.live = cached;
    app.world.live_at = age.map(|s| {
        std::time::Instant::now().checked_sub(std::time::Duration::from_secs(s as u64)).unwrap_or_else(std::time::Instant::now)
    });

    let (tx, rx) = mpsc::channel::<Bg>();
    // Scan and live query run in parallel: neither needs the other, and the live
    // one is the slower of the two.
    spawn_live_query(&tx);
    app.world.live_loading = true;
    let scan_tx = tx.clone();
    std::thread::spawn(move || {
        let _ = scan_tx.send(Bg::ScanDone(houston_core::scan::scan_all()));
    });

    let mut terminal = ratatui::init();
    let _ = execute!(stdout(), EnableMouseCapture);
    // A panic inside the draw loop would otherwise leave the terminal in raw mode
    // with mouse capture on, and the panic message itself unreadable — printed
    // into an alternate screen that is never left. Restore FIRST, then let the
    // default hook say what happened.
    let previous = std::panic::take_hook();
    std::panic::set_hook(Box::new(move |info| {
        restore_terminal();
        previous(info);
    }));
    let res = event_loop(&mut terminal, &mut app, rx, tx);
    let _ = execute!(stdout(), DisableMouseCapture);
    ratatui::restore();
    app.persist();
    res
}

fn event_loop(
    terminal: &mut ratatui::DefaultTerminal,
    app: &mut App,
    rx: mpsc::Receiver<Bg>,
    tx: mpsc::Sender<Bg>,
) -> anyhow::Result<()> {
    loop {
        while let Ok(msg) = rx.try_recv() {
            match msg {
                Bg::ScanDone(missions) => {
                    app.world.missions = missions;
                    app.world.scanning = false;
                    app.world.rebuild();
                }
                Bg::LiveDone(live) => {
                    app.world.live = live;
                    app.world.live_at = Some(std::time::Instant::now());
                    app.world.live_loading = false;
                }
            }
        }

        terminal.draw(|f| ui(f, app))?;

        let full = terminal.size().map(|s| Rect::new(0, 0, s.width, s.height))?;
        let mut rects = Vec::new();
        collect_rects(&app.root, body_area(full), &mut rects);
        {
            let focused = app.focused;
            let mut ws = Vec::new();
            widgets_mut(&mut app.root, &mut ws);
            for (i, (w, r)) in ws.iter_mut().zip(rects.iter()).enumerate() {
                w.post_render(inner(*r), &app.world, i == focused);
            }
        }

        if !event::poll(Duration::from_millis(100))? {
            continue;
        }
        match event::read()? {
            Event::Key(k) if k.kind == KeyEventKind::Press => {
                // The conflict prompt is modal on purpose: it exists because the
                // next keystroke decides between two different commands.
                let quit = if app.conflict.is_some() {
                    conflict_key(app, k)
                } else if app.opts.is_some() {
                    options_key(app, k)
                } else if app.pal {
                    palette_key(app, k)
                } else if app.help {
                    help_key(app, k)
                } else {
                    normal_key(app, k)
                };
                if quit {
                    return Ok(());
                }
            }
            Event::Mouse(m) if !app.pal && !app.help && app.conflict.is_none() && app.opts.is_none() => {
                if let Some(action) = handle_mouse(app, m, full, &rects) {
                    app.pending = Some(action);
                }
            }
            _ => {}
        }
        // Perform any queued action (resume) outside the draw cycle.
        if let Some(action) = app.pending.take() {
            run_action(terminal, app, action)?;
            // A session just started or ended, so the live snapshot is stale by
            // construction — this is the one refresh that is always worth it.
            app.world.live_loading = true;
            spawn_live_query(&tx);
        }
        // A pane asked for Houston's own config to change. Applied here because
        // `App` owns `cfg`, is the only writer of `config-v2.json`, and is the only
        // thing that can refresh what a changed config affects.
        if let Some(req) = app.world.config_request.take() {
            app.apply_config_request(req);
        }
        if app.world.live_wanted {
            app.world.live_wanted = false;
            if !app.world.live_loading {
                app.world.live_loading = true;
                spawn_live_query(&tx);
            }
        }
    }
}

/// Perform an out-of-loop action: tear the TUI down, run it, restore. Used for
/// resume (hand the terminal to `claude`, then take it back).
fn run_action(terminal: &mut ratatui::DefaultTerminal, _app: &mut App, action: Action) -> anyhow::Result<()> {
    let _ = execute!(stdout(), DisableMouseCapture);
    ratatui::restore();
    // Repair drifted data links before handing the terminal to claude (v1
    // healed on every launch/resume path), so the session sees the shared store.
    let _ = houston_core::heal::heal(&houston_core::accounts::load().unwrap_or_default());
    let status = match action {
        Action::Resume { id, cwd, path, prefs } => {
            houston_core::launch::resume_command(&id, &cwd, &path, &prefs).status()
        }
        Action::Launch { id, config_dir } => {
            // Forcing an account from the Accounts view IS a session, so it is
            // journalled — unlike the same builder's use by `fleet` for MCP work.
            let cwd = std::env::current_dir().map(|p| p.to_string_lossy().into_owned()).unwrap_or_default();
            houston_core::launch::record_launch(&id, houston_core::usage::Pick::Forced, &cwd);
            houston_core::launch::launch_command(&id, config_dir.as_deref()).status()
        }
    };
    if status.is_err() {
        eprintln!("houston: could not launch claude — is it on PATH?");
    }
    // Reclaim the terminal for the TUI.
    *terminal = ratatui::init();
    let _ = execute!(stdout(), EnableMouseCapture);
    let _ = terminal.clear();
    Ok(())
}

/// Key handling in the normal state: globals (`?`, `:`, focus, quit) first,
/// then route the rest to the focused widget.
fn normal_key(app: &mut App, k: KeyEvent) -> bool {
    let ctrl = k.modifiers.contains(KeyModifiers::CONTROL);
    // The status line is transient: the next keystroke clears it, and whatever
    // this key does may set a fresh one.
    app.world.status.clear();
    match k.code {
        KeyCode::Char('c') if ctrl => return true,
        KeyCode::Char('q') => return true,
        KeyCode::Char('?') => {
            app.help = true;
            app.help_scroll = 0;
        }
        KeyCode::Char(':') => open_palette(app),
        KeyCode::Char('p') if ctrl => open_palette(app),
        KeyCode::Tab => app.focus_next(),
        KeyCode::BackTab => app.focus_prev(),
        KeyCode::Up => route_key(app, 'k'),
        KeyCode::Down => route_key(app, 'j'),
        // Enter is context-aware, and a pane that is taking typed input comes
        // FIRST: resuming a chat because somebody pressed Enter to save a value
        // would be a genuinely surprising thing to do.
        KeyCode::Enter if app.focused_is_editing() => app.edit_key(EditKey::Commit),
        KeyCode::Esc if app.focused_is_editing() => app.edit_key(EditKey::Cancel),
        KeyCode::Backspace if app.focused_is_editing() => app.edit_key(EditKey::Backspace),
        // Enter, in order of who has the strongest claim: a pane taking typed
        // input, then a pane that says Enter means something there, and only then
        // the default — which stays a one-keystroke resume.
        KeyCode::Enter => {
            if !app.offer_enter() {
                if app.focused_widget_id().as_deref() == Some("basics:accounts") {
                    app.begin_launch();
                } else {
                    app.begin_resume();
                }
            }
        }
        // '0' opens Houston's own Settings tab. The digit shortcuts below skip 0
        // on purpose, so it was free for exactly this.
        KeyCode::Char('0') => {
            if let Some(i) = app.tabs.iter().position(|t| t.synthetic) {
                app.switch_tab(i);
            }
        }
        // 'o' opens the launch options for the selected chat. Deliberately NOT on
        // Enter's path: Enter stays one keystroke, and this is where the choices
        // live (the settled decision from the integration plan).
        KeyCode::Char('o') => app.begin_options(),
        // 'L' re-asks claude which sessions are live. Explicit rather than timed:
        // the query costs ~1.2s, so Houston refreshes it at start and after every
        // session, and this is the manual nudge in between.
        KeyCode::Char('L') => {
            app.world.live_wanted = true;
            app.world.status =
                if app.world.live_loading { "already checking…".into() } else { "checking live sessions…".into() };
        }
        // 'A' jumps straight to the Accounts view (v1's shortcut).
        KeyCode::Char('A') => {
            if let Some(i) = app.tabs.iter().position(|t| t.name.eq_ignore_ascii_case("accounts")) {
                app.switch_tab(i);
            }
        }
        // Tab switching is global, but only when there's more than one tab —
        // otherwise digits and brackets pass through to the focused widget.
        // The guards count VIEWS, not tabs: with one configured view these keys
        // still fall through to the focused widget, exactly as before Settings
        // existed.
        KeyCode::Char(']') if app.view_tabs().len() > 1 => app.next_tab(),
        KeyCode::Char('[') if app.view_tabs().len() > 1 => app.prev_tab(),
        KeyCode::Char(d) if app.view_tabs().len() > 1 && d.is_ascii_digit() && d != '0' => {
            // The digit selects the nth VIEW, so it keeps meaning what the header
            // shows even if the tab order ever changes.
            if let Some(&i) = app.view_tabs().get(d as usize - '1' as usize) {
                app.switch_tab(i);
            }
        }
        KeyCode::Char(c) => route_key(app, c),
        _ => {}
    }
    false
}

/// Keys in the launch-options overlay. Two modes: navigating rows, and typing a
/// value. Nothing here can start a session — the overlay only decides HOW the
/// next resume runs, so a stray key must never launch one.
fn options_key(app: &mut App, k: KeyEvent) -> bool {
    // The overlay is mutated in a scope, then persisted: holding a borrow of
    // `app.opts` across `app.save_options()` would be a second mutable borrow of
    // the same App.
    let (mut changed, mut close) = (false, false);
    {
        let Some(o) = app.opts.as_mut() else { return false };
        if o.editing.is_some() {
            match k.code {
                KeyCode::Enter => {
                    let text = o.editing.take().unwrap_or_default();
                    let f = o.field();
                    o.set_text(f, &text);
                    changed = true;
                }
                KeyCode::Esc => o.editing = None,
                KeyCode::Backspace => {
                    if let Some(b) = o.editing.as_mut() {
                        b.pop();
                    }
                }
                KeyCode::Char(c) => {
                    if let Some(b) = o.editing.as_mut() {
                        b.push(c);
                    }
                }
                _ => {}
            }
        } else {
            match k.code {
                KeyCode::Esc | KeyCode::Char('o') | KeyCode::Char('q') => close = true,
                KeyCode::Down | KeyCode::Char('j') => o.move_row(1),
                KeyCode::Up | KeyCode::Char('k') => o.move_row(-1),
                KeyCode::Enter | KeyCode::Char(' ') => {
                    o.primary();
                    changed = true;
                }
                KeyCode::Char('e') => o.begin_edit(),
                KeyCode::Char('d') | KeyCode::Backspace => {
                    o.clear_field();
                    changed = true;
                }
                // Clearing everything is one key because the alternative — nine
                // clears — is how a mission ends up with a stale flag nobody
                // meant to keep.
                KeyCode::Char('X') => {
                    o.prefs = houston_core::model::Launch::default();
                    changed = true;
                }
                _ => {}
            }
        }
    }
    if changed {
        app.save_options();
    }
    if close {
        app.opts = None;
    }
    false
}

/// Keys while the "already open" prompt is up. Deliberately tiny: two ways
/// forward and a way out, and nothing else does anything — a stray key here must
/// not fall through and resume something.
fn conflict_key(app: &mut App, k: KeyEvent) -> bool {
    match k.code {
        KeyCode::Char('f') => app.resolve_conflict(true),
        KeyCode::Enter => app.resolve_conflict(false),
        KeyCode::Esc | KeyCode::Char('q') => app.conflict = None,
        // Rechecking is useful right here: the reading may be stale, and the
        // answer changes which of the two choices is even needed.
        KeyCode::Char('L') => {
            app.world.live_wanted = true;
            app.conflict = None;
            app.world.status = "rechecking live sessions…".into();
        }
        _ => {}
    }
    false
}

fn open_palette(app: &mut App) {
    app.help = false;
    app.pal = true;
    app.pal_input.clear();
    app.pal_sel = 0;
}

/// The `?` overlay behaves which-key style: it stays open while you scroll it,
/// and any other key closes it and runs as if you'd pressed it underneath.
fn help_key(app: &mut App, k: KeyEvent) -> bool {
    let n = shell::help_line_count(app) as u16;
    match k.code {
        KeyCode::Esc | KeyCode::Char('?') => app.help = false,
        KeyCode::Up | KeyCode::Char('k') => app.help_scroll = app.help_scroll.saturating_sub(1),
        KeyCode::Down | KeyCode::Char('j') => app.help_scroll = (app.help_scroll + 1).min(n.saturating_sub(1)),
        KeyCode::PageUp => app.help_scroll = app.help_scroll.saturating_sub(8),
        KeyCode::PageDown => app.help_scroll = (app.help_scroll + 8).min(n.saturating_sub(1)),
        KeyCode::Tab => {
            app.help = false;
            app.focus_next();
        }
        KeyCode::Char(c) => {
            app.help = false;
            return normal_key(app, KeyEvent::new(KeyCode::Char(c), k.modifiers));
        }
        _ => {}
    }
    false
}

/// Palette input: type to filter, up/down (or ctrl-n/p) to move, enter runs.
fn palette_key(app: &mut App, k: KeyEvent) -> bool {
    let ctrl = k.modifiers.contains(KeyModifiers::CONTROL);
    let len = shell::palette_matches(app).len();
    let clamp = |s: usize| if len == 0 { 0 } else { s.min(len - 1) };
    match k.code {
        KeyCode::Esc => app.pal = false,
        KeyCode::Enter => {
            let matches = shell::palette_matches(app);
            let picked = matches.get(clamp(app.pal_sel)).cloned();
            app.pal = false;
            if let Some(cmd) = picked {
                return run_command(app, &cmd);
            }
        }
        KeyCode::Up => app.pal_sel = clamp(app.pal_sel.saturating_sub(1)),
        KeyCode::Down => app.pal_sel = clamp(app.pal_sel + 1),
        KeyCode::Backspace => {
            app.pal_input.pop();
            app.pal_sel = 0;
        }
        KeyCode::Char('p') if ctrl => app.pal_sel = clamp(app.pal_sel.saturating_sub(1)),
        KeyCode::Char('n') if ctrl => app.pal_sel = clamp(app.pal_sel + 1),
        KeyCode::Char(c) if !ctrl => {
            app.pal_input.push(c);
            app.pal_sel = 0;
        }
        _ => {}
    }
    false
}

/// Run a registry command chosen from the palette by replaying its primary
/// key binding through the normal dispatch.
fn run_command(app: &mut App, cmd: &command::Command) -> bool {
    let Some(key) = cmd.keys.first() else { return false };
    match key.as_str() {
        "q" => return true,
        "?" => {
            app.help = true;
            app.help_scroll = 0;
        }
        ":" | "ctrl+p" => open_palette(app),
        "enter" => app.begin_resume(),
        "tab" => app.focus_next(),
        "shift+tab" => app.focus_prev(),
        "]" => app.next_tab(),
        "[" => app.prev_tab(),
        "A" => {
            if let Some(i) = app.tabs.iter().position(|t| t.name.eq_ignore_ascii_case("accounts")) {
                app.switch_tab(i);
            }
        }
        "esc" => {}
        // Globals must be listed here explicitly: the fallback below routes a
        // single character to the FOCUSED WIDGET, so a global key picked from the
        // palette would otherwise reach a widget that ignores it and appear
        // broken.
        "o" => app.begin_options(),
        "L" => app.world.live_wanted = true,
        s => {
            let mut chars = s.chars();
            if let (Some(c), None) = (chars.next(), chars.next()) {
                route_key(app, c);
            }
        }
    }
    false
}

/// Which tab label sits under column `x` on the header row — mirrors the
/// layout in `render_header` exactly (brand, then " n name " per tab with a
/// one-cell separator).
fn header_tab_at(app: &App, x: u16, width: u16) -> Option<usize> {
    header_layout(app, width).1.into_iter().find(|c| x >= c.col && x < c.col + c.width).map(|c| c.tab)
}

fn handle_mouse(
    app: &mut App,
    m: ratatui::crossterm::event::MouseEvent,
    full: Rect,
    rects: &[Rect],
) -> Option<Action> {
    match m.kind {
        MouseEventKind::Down(MouseButton::Left) => {
            // Click on the header row → switch tab.
            if m.row == full.y {
                if let Some(i) = header_tab_at(app, m.column, full.width) {
                    app.switch_tab(i);
                }
                return None;
            }
            // A boundary under the cursor starts a resize; otherwise focus +
            // forward the click to the pane's widget.
            let mut bounds = Vec::new();
            collect_boundaries(&app.root, body_area(full), &mut Vec::new(), &mut bounds);
            if let Some(bi) = boundary_at(&bounds, m.column, m.row) {
                let b = &bounds[bi];
                let start = if b.dir == Direction::Horizontal { m.column } else { m.row };
                app.drag = Some(Drag {
                    path: b.path.clone(),
                    idx: b.idx,
                    dir: b.dir,
                    start,
                    base_a: b.len_a,
                    base_b: b.len_b,
                });
                return None;
            }
            if let Some(i) = pane_at(&app.root, full, m.column, m.row) {
                app.focused = i;
                let r = inner(rects[i]);
                if m.column >= r.x && m.row >= r.y && m.column < r.x + r.width && m.row < r.y + r.height {
                    let mut ws = Vec::new();
                    widgets_mut(&mut app.root, &mut ws);
                    let wid = ws[i].id().to_string();
                    ws[i].on_click(m.row - r.y, m.column - r.x, &mut app.world);
                    // Double-click resumes a mission / launches an account.
                    let now = std::time::Instant::now();
                    let dbl = app
                        .last_click
                        .map(|(t, pane, row)| pane == i && row == m.row && now.duration_since(t) <= DOUBLE_CLICK)
                        .unwrap_or(false);
                    app.last_click = Some((now, i, m.row));
                    if dbl && wid == "missions" {
                        if let Some(mi) = app.world.selected() {
                            let meta = app.world.meta_of(mi);
                            let cwd =
                                if meta.cwd_override.is_empty() { mi.cwd.clone() } else { meta.cwd_override.clone() };
                            let id = mi.id.clone();
                            let path = mi.path.clone();
                            let prefs = meta.launch.clone();
                            app.last_click = None; // don't treat a 3rd click as another double
                            return Some(Action::Resume { id, cwd, path, prefs });
                        }
                    }
                    if dbl && wid == "basics:accounts" {
                        if let Some(a) = app.world.selected_account() {
                            let (id, config_dir) = (a.id.clone(), a.resolve_config_dir());
                            app.last_click = None;
                            return Some(Action::Launch { id, config_dir });
                        }
                    }
                }
            }
        }
        MouseEventKind::Drag(MouseButton::Left) => {
            if let Some(d) = &app.drag {
                let cursor = if d.dir == Direction::Horizontal { m.column } else { m.row };
                app.apply_drag(cursor);
            }
        }
        MouseEventKind::Up(MouseButton::Left) => {
            if app.drag.take().is_some() {
                app.persist();
            }
        }
        MouseEventKind::ScrollUp | MouseEventKind::ScrollDown => {
            if let Some(i) = pane_at(&app.root, full, m.column, m.row) {
                let up = m.kind == MouseEventKind::ScrollUp;
                let mut ws = Vec::new();
                widgets_mut(&mut app.root, &mut ws);
                ws[i].on_scroll(up, &mut app.world);
            }
        }
        _ => {}
    }
    None
}

fn route_key(app: &mut App, c: char) {
    let mut ws = Vec::new();
    widgets_mut(&mut app.root, &mut ws);
    if let Some(w) = ws.get_mut(app.focused) {
        let _ = w.on_key(c, &mut app.world);
    }
}

fn render_pane(w: &dyn Widget, area: Rect, frame: &mut Frame, world: &World, focused: bool) {
    let color = if focused { world.palette.border_focus } else { world.palette.border };
    let mut bs = Style::new().fg(color);
    if focused {
        bs = bs.add_modifier(Modifier::BOLD);
    }
    // One cell of horizontal breathing room so content never sits flush
    // against the border. inner() already accounts for the padding.
    let block = Block::bordered().title(w.title(world)).border_style(bs).padding(Padding::horizontal(1));
    let content = block.inner(area);
    frame.render_widget(block, area);
    w.render(content, frame, world, focused);
}

fn ui(frame: &mut Frame, app: &App) {
    let area = frame.area();
    let p = &app.world.palette;
    let header = Rect::new(area.x, area.y, area.width, 1.min(area.height));
    let body = body_area(area);
    let footer = Rect::new(area.x, area.y + area.height.saturating_sub(1), area.width, 1.min(area.height));

    render_header(frame, header, app);

    let mut rects = Vec::new();
    collect_rects(&app.root, body, &mut rects);
    let mut i = 0;
    render_tree(&app.root, &rects, &mut i, frame, app);

    // The discovery shell draws on top of the container tree. The conflict
    // prompt outranks both: it is answering a question the user just asked.
    if app.conflict.is_some() {
        shell::render_conflict(frame, body, app);
    } else if let Some(o) = app.opts.as_ref() {
        options::render(frame, body, &app.world, o);
    } else if app.pal {
        shell::render_palette(frame, body, app);
    } else if app.help {
        shell::render_help(frame, body, app);
    }

    // A footer that never collapses: only hint-flagged commands, derived from
    // the registry. `?` and `:` carry the full discoverability. A transient
    // status (an export path, an error) takes the line while it's set.
    let (text, style) = if app.world.status.is_empty() {
        (command::footer_hint(&app.registry()), Style::new().fg(p.grey))
    } else {
        (app.world.status.clone(), Style::new().fg(p.accent))
    };
    frame.render_widget(Paragraph::new(Line::raw(text)).style(style), footer);
}

/// The header: a single title for one tab, or a tab bar (active tab bold, the
/// rest dim) once there's more than one. The whole bar rides on the theme's
/// header colors so it stays legible in both the B&W core and blue basics.
/// A clickable tab chip in the header: where it sits and which tab it selects.
struct Chip {
    tab: usize,
    col: u16,
    width: u16,
}

/// The header, computed ONCE: the spans to draw and the boxes to click.
///
/// These used to be two functions doing the same arithmetic, with a comment
/// asking them to stay in step. Adding a right-aligned tab would have made it
/// three copies, and the failure mode is quiet and infuriating — a click lands on
/// the tab next to the one you aimed at.
fn header_layout(app: &App, width: u16) -> (Vec<Span<'static>>, Vec<Chip>) {
    let p = &app.world.palette;
    let brand = " Houston ";
    let mut spans: Vec<Span<'static>> =
        vec![Span::styled(brand.to_string(), Style::new().fg(p.accent).add_modifier(Modifier::BOLD))];
    let mut chips = Vec::new();
    let mut col = brand.chars().count() as u16;

    let chip_style = |active: bool| {
        if active {
            Style::new().fg(p.header_fg).bg(p.accent).add_modifier(Modifier::BOLD)
        } else {
            Style::new().fg(p.grey)
        }
    };

    // Configured tabs on the left, numbered as they are keyed. The Settings tab is
    // synthetic and goes on the right.
    let configured: Vec<(usize, &TabState)> = app.tabs.iter().enumerate().filter(|(_, t)| !t.synthetic).collect();
    if configured.len() <= 1 {
        // One view: keep the quiet single-tab form rather than a strip of one.
        if let Some((_, t)) = configured.first() {
            let label = format!("· {} ", t.name);
            col += label.chars().count() as u16;
            spans.push(Span::styled(label, Style::new().fg(p.grey)));
        }
    } else {
        for (drawn, (i, t)) in configured.iter().enumerate() {
            if drawn > 0 {
                spans.push(Span::styled("│".to_string(), Style::new().fg(p.dim)));
                col += 1;
            }
            let label = format!(" {} {} ", drawn + 1, t.name);
            let w = label.chars().count() as u16;
            spans.push(Span::styled(label, chip_style(*i == app.active)));
            chips.push(Chip { tab: *i, col, width: w });
            col += w;
        }
    }

    // Right side, quiet: Houston's own Settings tab, then the mission count.
    // Settings sits apart from the view tabs because it is not a view of your
    // chats — numbering it among them would imply it is.
    let settings = app.tabs.iter().position(|t| t.synthetic);
    let settings_label = settings.map(|_| format!(" 0 {SETTINGS_TAB} ")).unwrap_or_default();
    let info = format!("{} missions ", app.world.visible.len());
    let right = (settings_label.chars().count() + info.chars().count()) as u16;
    if width > col + right {
        let gap = width - col - right;
        spans.push(Span::raw(" ".repeat(gap as usize)));
        col += gap;
        if let Some(i) = settings {
            let w = settings_label.chars().count() as u16;
            spans.push(Span::styled(settings_label, chip_style(i == app.active)));
            chips.push(Chip { tab: i, col, width: w });
        }
        spans.push(Span::styled(info, Style::new().fg(p.grey)));
    }
    (spans, chips)
}

fn render_header(frame: &mut Frame, header: Rect, app: &App) {
    // Transparent top bar (no full-row background). Blue brand; only the ACTIVE
    // tab wears a filled block, inactive tabs are quiet grey text with dim
    // separators — v1's strip.
    let (spans, _) = header_layout(app, header.width);
    frame.render_widget(Paragraph::new(Line::from(spans)), header);
}

fn render_tree(node: &Node, rects: &[Rect], i: &mut usize, frame: &mut Frame, app: &App) {
    match node {
        Node::Pane(w) => {
            render_pane(w.as_ref(), rects[*i], frame, &app.world, *i == app.focused);
            *i += 1;
        }
        Node::Split { children, .. } => {
            for (_, child) in children {
                render_tree(child, rects, i, frame, app);
            }
        }
    }
}

// ------------------------------------------------------------ screenshots --
// A faithful, headless render of the real UI to HTML, for design review. Not
// used at runtime; kept out of the hot path but in-crate so it can drive the
// private ui()/overlay state exactly as the terminal would.

/// xterm-256 index → RGB. Base 0-15 use a Campbell-ish scheme (Windows
/// Terminal default), 16-231 the color cube, 232-255 the gray ramp.
fn ansi256_rgb(n: u8) -> (u8, u8, u8) {
    const BASE: [(u8, u8, u8); 16] = [
        (12, 12, 12),
        (197, 15, 31),
        (19, 161, 14),
        (193, 156, 0),
        (0, 55, 218),
        (136, 23, 152),
        (58, 150, 221),
        (204, 204, 204),
        (118, 118, 118),
        (231, 72, 86),
        (22, 198, 12),
        (249, 241, 165),
        (59, 120, 255),
        (180, 0, 158),
        (97, 214, 214),
        (242, 242, 242),
    ];
    match n {
        0..=15 => BASE[n as usize],
        16..=231 => {
            let n = n - 16;
            let step = |v: u8| if v == 0 { 0 } else { 55 + 40 * v };
            (step(n / 36), step((n / 6) % 6), step(n % 6))
        }
        _ => {
            let v = 8 + 10 * (n - 232);
            (v, v, v)
        }
    }
}

fn color_rgb(c: ratatui::style::Color) -> (u8, u8, u8) {
    use ratatui::style::Color::*;
    match c {
        Reset => (204, 204, 204),
        Black => (12, 12, 12),
        Red => (197, 15, 31),
        Green => (19, 161, 14),
        Yellow => (193, 156, 0),
        Blue => (0, 55, 218),
        Magenta => (136, 23, 152),
        Cyan => (58, 150, 221),
        Gray => (204, 204, 204),
        DarkGray => (118, 118, 118),
        LightRed => (231, 72, 86),
        LightGreen => (22, 198, 12),
        LightYellow => (249, 241, 165),
        LightBlue => (59, 120, 255),
        LightMagenta => (180, 0, 158),
        LightCyan => (97, 214, 214),
        White => (242, 242, 242),
        Indexed(n) => ansi256_rgb(n),
        Rgb(r, g, b) => (r, g, b),
    }
}

fn html_escape(s: &str) -> String {
    s.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;")
}

/// Render the current UI to an in-memory buffer at the given size.
pub fn render_snapshot(app: &mut App, width: u16, height: u16) -> ratatui::buffer::Buffer {
    let backend = ratatui::backend::TestBackend::new(width, height);
    let mut term = ratatui::Terminal::new(backend).expect("test terminal");
    term.draw(|f| ui(f, app)).expect("draw");
    term.backend().buffer().clone()
}

/// Turn a rendered buffer into an HTML `<pre>` grid with the exact resolved
/// colors — REVERSED swaps fg/bg, DIM darkens the fg — so the page shows what
/// the terminal paints, cell for cell.
type CellStyle = ((u8, u8, u8), (u8, u8, u8), bool); // (fg, bg, bold)

fn buffer_to_html(buf: &ratatui::buffer::Buffer) -> String {
    use ratatui::style::{Color, Modifier};
    let area = buf.area();
    let default_bg = (12, 12, 12);
    // The resolved visual style of one cell (what actually gets painted).
    let resolve = |cell: &ratatui::buffer::Cell| -> CellStyle {
        let mut fg = if cell.fg == Color::Reset { (204, 204, 204) } else { color_rgb(cell.fg) };
        let mut bg = if cell.bg == Color::Reset { default_bg } else { color_rgb(cell.bg) };
        let m = cell.modifier;
        if m.contains(Modifier::REVERSED) {
            std::mem::swap(&mut fg, &mut bg);
        }
        if m.contains(Modifier::DIM) {
            fg = ((fg.0 as f32 * 0.55) as u8, (fg.1 as f32 * 0.55) as u8, (fg.2 as f32 * 0.55) as u8);
        }
        (fg, bg, m.contains(Modifier::BOLD))
    };
    let mut out = String::from("<pre class=\"term\">");
    for y in 0..area.height {
        // Coalesce consecutive cells sharing a style into one span.
        let mut run = String::new();
        let mut cur: Option<CellStyle> = None;
        let flush = |out: &mut String, run: &mut String, st: &Option<CellStyle>| {
            if let Some((fg, bg, bold)) = st {
                out.push_str(&format!(
                    "<span style=\"color:rgb({},{},{});background:rgb({},{},{});{}\">{}</span>",
                    fg.0,
                    fg.1,
                    fg.2,
                    bg.0,
                    bg.1,
                    bg.2,
                    if *bold { "font-weight:700" } else { "" },
                    html_escape(run)
                ));
            }
            run.clear();
        };
        for x in 0..area.width {
            let cell = &buf[(x, y)];
            let st = resolve(cell);
            if cur.as_ref() != Some(&st) {
                flush(&mut out, &mut run, &cur);
                cur = Some(st);
            }
            run.push_str(cell.symbol());
        }
        flush(&mut out, &mut run, &cur);
        out.push('\n');
    }
    out.push_str("</pre>");
    out
}

/// A running session for a given mission id, shaped like the real payload.
#[cfg(test)]
fn live_session(mission_id: &str) -> houston_core::agents::Live {
    houston_core::agents::Live {
        pid: 12164,
        cwd: "C:\\Users\\me".into(),
        kind: "interactive".into(),
        started_at: 1_785_018_546_730,
        session_id: mission_id.to_string(),
        name: "demo-session".into(),
        status: "busy".into(),
    }
}

/// Build a demo App on the given config with a few representative missions,
/// so a snapshot shows real selection bars, tabs, quota, git, etc.
fn demo_app(cfg: Config) -> App {
    use houston_core::model::Mission;
    // Same reason as `isolate_store`: anything that can persist must not be
    // pointed at the real store. Inlined because this helper is not inside the
    // test module.
    #[cfg(test)]
    tests::isolate_store();
    let dir = std::env::temp_dir().join("houston-shot-store");
    let store = houston_core::store::Store::load_from(dir).unwrap();
    let mut app = App::from_config_with_plugins(cfg, store, &[]);
    let samples = [
        ("lifeos-stack: deploy postgres+pgvector", true),
        ("houston v2: native help + palette shell", false),
        ("pokewalker-v2: rtc bridge firmware", false),
        ("homelab-docs: router + wireguard notes", false),
        ("notion life crm: sync module", false),
    ];
    for (i, (title, pinned)) in samples.iter().enumerate() {
        let m = Mission {
            id: format!("s{i}"),
            project: "demo".into(),
            title: (*title).into(),
            cwd: std::env::temp_dir().to_string_lossy().to_string(),
            ..Default::default()
        };
        if *pinned {
            let _ = app.world.store.toggle_pin(&m.key());
        }
        app.world.missions.push(m);
    }
    app.world.scanning = false;
    app.world.cursor = 1;
    app.world.rebuild();
    app
}

/// HTML fragment: a set of labeled v2 screenshots (basics theme).
pub fn demo_screens_html() -> String {
    let mut out = String::new();
    let mut push = |title: &str, app: &mut App| {
        out.push_str(&format!("<figure><figcaption>{}</figcaption>", html_escape(title)));
        out.push_str(&buffer_to_html(&render_snapshot(app, 108, 30)));
        out.push_str("</figure>");
    };

    let mut a = demo_app(Config::basics());
    push("v2 — vista principal (Missions enfocado)", &mut a);

    let mut b = demo_app(Config::basics());
    b.focused = 0; // filters pane
    push("v2 — Filters enfocado (misión seleccionada sin foco)", &mut b);

    let mut c = demo_app(Config::basics());
    c.help = true;
    push("v2 — ayuda ? (which-key)", &mut c);

    let mut d = demo_app(Config::basics());
    d.pal = true;
    d.pal_input = "ref".into();
    push("v2 — palette : (filtro \"ref\")", &mut d);

    let mut e = demo_app(Config::basics());
    e.next_tab();
    push("v2 — tab Focus", &mut e);

    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use houston_core::store::Store;

    /// Point the STORE at a throwaway directory, once for the whole test process.
    ///
    /// This is not tidiness. `App::persist` writes `config-v2.json` through
    /// `store_dir()`, which is `$HOUSTON_HOME` or the real store — so a test that
    /// persists without this writes into the developer's own configuration. It
    /// did: a test on a default config, sitting on the Settings tab, replaced a
    /// working four-tab layout with a single `{"widget":"settings"}` pane. The
    /// persist bug was real, but the test suite was the thing that fired it.
    ///
    /// Set on every call rather than once: it is a couple of instructions, and it
    /// cannot be undone by another test clearing the variable.
    pub(crate) fn isolate_store() {
        static DIR: std::sync::OnceLock<std::path::PathBuf> = std::sync::OnceLock::new();
        let dir = DIR.get_or_init(|| {
            let d = std::env::temp_dir().join(format!("houston-tui-tests-{}", std::process::id()));
            let _ = std::fs::create_dir_all(&d);
            d
        });
        unsafe { std::env::set_var("HOUSTON_HOME", dir) };
    }

    fn test_app() -> App {
        isolate_store();
        let tmp = tempfile::tempdir().unwrap();
        let store = Store::load_from(tmp.path().to_path_buf()).unwrap();
        std::mem::forget(tmp);
        App::from_config_with_plugins(Config::default(), store, &[])
    }

    fn app_with(cfg: Config) -> App {
        isolate_store();
        let tmp = tempfile::tempdir().unwrap();
        let store = Store::load_from(tmp.path().to_path_buf()).unwrap();
        std::mem::forget(tmp);
        App::from_config_with_plugins(cfg, store, &[])
    }

    /// Everything the live snapshot changes about the mission list, in one pass:
    /// a busy session is marked, an idle one differently, an unrelated one not at
    /// all, and the marker never displaces the title.
    #[test]
    fn a_live_session_marks_its_mission_row() {
        fn cells(app: &mut App) -> String {
            let buf = render_snapshot(app, 90, 12);
            let mut s = String::new();
            for y in 0..buf.area.height {
                for x in 0..buf.area.width {
                    s.push_str(buf[(x, y)].symbol());
                }
                s.push('\n');
            }
            s
        }

        let mut app = demo_app(Config::basics());
        let sel = app.world.selected().expect("demo missions exist");
        let (title, id) = (sel.title.clone(), sel.id.clone());
        let plain = cells(&mut app);
        assert!(!plain.contains('●') && !plain.contains('○'), "no snapshot, no markers");
        // A stable prefix of the title, char-safe whatever the sample says.
        let head: String = title.chars().take(15).collect();
        assert!(plain.contains(&head));

        app.world.live = vec![live_session(&id)];
        let busy = cells(&mut app);
        assert!(busy.contains('●'), "a busy session gets the filled marker");
        assert!(busy.contains(&head), "the title is not pushed off the row");

        app.world.live[0].status = "idle".into();
        let idle = cells(&mut app);
        assert!(idle.contains('○') && !idle.contains('●'), "idle is hollow, not filled");

        // A live session Houston has no mission for changes nothing in the list.
        app.world.live = vec![live_session("no-such-mission")];
        let other = cells(&mut app);
        assert!(!other.contains('●') && !other.contains('○'));
    }

    /// Enter is a one-keystroke resume — except when the chat is already open,
    /// where the alternatives are two different commands.
    #[test]
    fn enter_asks_before_attaching_to_a_running_session() {
        let mut app = demo_app(Config::basics());
        // The mission under the cursor, not missions[0]: demo_app pins through a
        // persisted store, so which row is selected varies between runs.
        let id = app.world.selected().expect("demo missions exist").id.clone();

        // Nothing live: straight to a resume, no prompt.
        app.begin_resume();
        assert!(app.conflict.is_none());
        assert!(matches!(app.pending, Some(Action::Resume { .. })));

        // Live: the resume is held and the prompt describes what it knows.
        app.pending = None;
        app.world.live = vec![live_session(&id)];
        app.world.live_at = Some(std::time::Instant::now());
        app.begin_resume();
        assert!(app.pending.is_none(), "nothing runs until the user answers");
        let (name, pid, status, _age) = app.conflict_view().expect("prompt is open");
        assert_eq!((name.as_str(), pid, status.as_str()), ("demo-session", 12164, "busy"));

        // 'f' forks: the same resume, plus --fork-session.
        conflict_key(&mut app, KeyEvent::from(KeyCode::Char('f')));
        assert!(app.conflict.is_none());
        match app.pending.take() {
            Some(Action::Resume { prefs, id: rid, .. }) => {
                assert!(prefs.fork, "forking is the whole point of that key");
                assert_eq!(rid, id);
            }
            other => panic!("expected a forked resume, got {}", other.is_some()),
        }

        // Enter attaches anyway: resume unchanged.
        app.begin_resume();
        conflict_key(&mut app, KeyEvent::from(KeyCode::Enter));
        match app.pending.take() {
            Some(Action::Resume { prefs, .. }) => assert!(!prefs.fork, "attaching must not silently fork"),
            _ => panic!("expected a plain resume"),
        }

        // Esc cancels: no prompt, and nothing queued.
        app.begin_resume();
        conflict_key(&mut app, KeyEvent::from(KeyCode::Esc));
        assert!(app.conflict.is_none() && app.pending.is_none());

        // A stray key while the prompt is up must NOT fall through to a resume.
        app.begin_resume();
        conflict_key(&mut app, KeyEvent::from(KeyCode::Char('j')));
        assert!(app.conflict.is_some() && app.pending.is_none());
    }

    /// `o` is the setter the launch preferences were missing: what it changes
    /// must reach the store, survive closing, and come back the same.
    #[test]
    fn the_options_overlay_saves_as_you_go() {
        let key = |c: char| KeyEvent::from(KeyCode::Char(c));
        let mut app = demo_app(Config::basics());
        let mkey = app.world.selected().expect("demo missions exist").key();

        normal_key(&mut app, key('o'));
        assert!(app.opts.is_some(), "o opens the overlay");

        // Start from a KNOWN state: demo_app's store is a persisted temp dir, so a
        // previous run of this test can leave preferences behind — assuming an
        // empty start is how it failed the first time.
        options_key(&mut app, key('X'));
        assert_eq!(app.world.store.launch_of(&mkey), houston_core::model::Launch::default());

        // enter on the first row cycles the model, and that is persisted at once.
        // The first alias is whatever the cycle leads with, so read it from there
        // rather than hard-coding a model name that moves with the CLI.
        let first_model = options::Field::Model.cycle_values()[1].to_string();
        options_key(&mut app, KeyEvent::from(KeyCode::Enter));
        assert_eq!(app.world.store.launch_of(&mkey).model, first_model);

        // Move to worktree and type a name.
        for _ in 0..4 {
            options_key(&mut app, key('j'));
        }
        options_key(&mut app, key('e'));
        for c in "spike".chars() {
            options_key(&mut app, key(c));
        }
        options_key(&mut app, KeyEvent::from(KeyCode::Enter));
        assert_eq!(app.world.store.launch_of(&mkey).worktree.as_deref(), Some("spike"));

        // Esc closes without undoing anything (there is no cancel to confuse).
        options_key(&mut app, KeyEvent::from(KeyCode::Esc));
        assert!(app.opts.is_none());
        let saved = app.world.store.launch_of(&mkey);
        assert_eq!((saved.model.as_str(), saved.worktree.as_deref()), (first_model.as_str(), Some("spike")));

        // Reopening shows what was saved, and the resume path would use it.
        normal_key(&mut app, key('o'));
        assert_eq!(app.opts.as_ref().unwrap().prefs, saved);
        app.begin_resume();
        match app.pending.take() {
            Some(Action::Resume { prefs, .. }) => {
                assert_eq!(
                    houston_core::launch::args_for(&prefs),
                    vec!["--model", &first_model, "--worktree", "spike"]
                );
            }
            _ => panic!("expected a resume carrying the saved preferences"),
        }

        // X clears everything, and an all-default Launch prunes the metadata.
        normal_key(&mut app, key('o'));
        options_key(&mut app, key('X'));
        assert_eq!(app.world.store.launch_of(&mkey), houston_core::model::Launch::default());

        // While typing, a stray navigation key is TEXT, not navigation.
        options_key(&mut app, key('e'));
        options_key(&mut app, key('j'));
        assert_eq!(app.opts.as_ref().unwrap().editing.as_deref(), Some("j"));
        options_key(&mut app, KeyEvent::from(KeyCode::Esc));
        assert!(app.opts.as_ref().unwrap().editing.is_none(), "esc leaves the edit, not the overlay");
        assert!(app.opts.is_some());
    }

    /// Not an assertion — a way to LOOK at the overlay, since layout and colour
    /// placement are the things tests are worst at judging:
    /// `cargo test -p houston-tui shot_options_overlay -- --ignored --nocapture`
    #[test]
    #[ignore = "prints a frame for eyeballing; asserts nothing"]
    fn shot_options_overlay() {
        let mut app = demo_app(Config::basics());
        app.begin_options();
        if let Some(o) = app.opts.as_mut() {
            o.prefs.model = "sonnet".into();
            o.prefs.worktree = Some("spike".into());
            o.prefs.fork = true;
            o.row = 4;
        }
        let buf = render_snapshot(&mut app, 100, 30);
        for y in 0..buf.area.height {
            let mut s = String::new();
            for x in 0..buf.area.width {
                s.push_str(buf[(x, y)].symbol());
            }
            println!("{}", s.trim_end());
        }
    }

    /// The refresh is explicit because it costs ~1.2 s; asking twice must not
    /// start two queries.
    #[test]
    fn live_refresh_is_requested_once() {
        let mut app = demo_app(Config::basics());
        normal_key(&mut app, KeyEvent::from(KeyCode::Char('L')));
        assert!(app.world.live_wanted);
        app.world.live_loading = true;
        app.world.live_wanted = false;
        normal_key(&mut app, KeyEvent::from(KeyCode::Char('L')));
        assert!(app.world.live_wanted, "the request still stands");
        assert!(app.world.status.contains("already"), "and it says why nothing new happened");
    }

    #[test]
    fn built_from_config_three_panes_missions_focused() {
        let app = test_app();
        assert_eq!(app.panes, 3);
        let mut ids = Vec::new();
        widget_ids(&app.root, &mut ids);
        assert_eq!(ids, vec!["filters", "missions", "preview"]);
        assert_eq!(app.focused, 1);
    }

    #[test]
    fn unknown_widget_id_becomes_placeholder_not_crash() {
        use houston_core::config::{Dir, LayoutChild, LayoutNode, SizeSpec};
        let tmp = tempfile::tempdir().unwrap();
        let store = Store::load_from(tmp.path().to_path_buf()).unwrap();
        std::mem::forget(tmp);
        let cfg = Config {
            layout: LayoutNode::Split {
                dir: Dir::Row,
                children: vec![LayoutChild {
                    size: SizeSpec::Fill(1),
                    node: LayoutNode::Pane { widget: "quota-cockpit".into(), settings: None },
                }],
            },
            ..Default::default()
        };
        let app = App::from_config_with_plugins(cfg, store, &[]);
        let mut ids = Vec::new();
        widget_ids(&app.root, &mut ids);
        assert_eq!(ids, vec!["quota-cockpit"]); // MissingWidget carries the id
    }

    #[test]
    fn boundaries_found_between_columns() {
        let app = test_app();
        let mut bounds = Vec::new();
        collect_boundaries(&app.root, body_area(Rect::new(0, 0, 100, 30)), &mut Vec::new(), &mut bounds);
        assert_eq!(bounds.len(), 2); // three columns → two dividers
        // First divider sits at the right edge of the 20-wide Filters column.
        assert_eq!(bounds[0].coord, 20);
        assert!(boundary_at(&bounds, 20, 10).is_some());
        assert!(boundary_at(&bounds, 50, 10).is_none()); // mid-pane, not a divider
    }

    /// A pane's settings must survive the round trip through the live tree,
    /// because that trip happens on every save — including the one a border drag
    /// triggers. Without `Widget::settings()` the first resize would silently
    /// erase every configured pane, and the user would find out when their probe
    /// pane came back empty.
    #[test]
    fn pane_settings_survive_layout_persistence() {
        use houston_core::config::{Dir, LayoutChild, LayoutNode, SizeSpec};
        let cfg = serde_json::json!({
            "run": ["ssh", "homelab", "uptime"],
            "title": "homelab",
            "every_secs": 30
        });
        let layout = LayoutNode::Split {
            dir: Dir::Row,
            children: vec![
                LayoutChild { size: SizeSpec::Fixed(20), node: LayoutNode::Pane { widget: "missions".into(), settings: None } },
                LayoutChild {
                    size: SizeSpec::Fill(1),
                    node: LayoutNode::Pane { widget: "probe".into(), settings: Some(cfg.clone()) },
                },
            ],
        };

        let tree = build::build_tree(&layout, &[]);
        let back = build::to_layout(&tree);

        let LayoutNode::Split { children, .. } = &back else { panic!("shape changed") };
        match &children[1].node {
            LayoutNode::Pane { widget, settings } => {
                assert_eq!(widget, "probe");
                assert_eq!(settings.as_ref(), Some(&cfg), "the pane's configuration came back intact");
            }
            other => panic!("expected a pane, got {other:?}"),
        }
        // And a pane that has no settings still writes none, so the common layout
        // does not grow a `"settings": null` on every save.
        match &children[0].node {
            LayoutNode::Pane { settings, .. } => assert!(settings.is_none()),
            other => panic!("expected a pane, got {other:?}"),
        }
        let json = serde_json::to_string(&back).unwrap();
        assert!(!json.contains("null"), "no empty settings keys in the file: {json}");
    }

    #[test]
    fn drag_moves_cells_between_neighbors_and_persists_shape() {
        let mut app = test_app();
        // Start a drag on the first divider (coord 20) and drag +6 cells.
        let mut bounds = Vec::new();
        collect_boundaries(&app.root, body_area(Rect::new(0, 0, 100, 30)), &mut Vec::new(), &mut bounds);
        let b = &bounds[0];
        app.drag = Some(Drag {
            path: b.path.clone(),
            idx: b.idx,
            dir: b.dir,
            start: b.coord,
            base_a: b.len_a,
            base_b: b.len_b,
        });
        let sum_before = b.len_a + b.len_b;
        app.apply_drag(26); // +6
        // Neighbors are now Fixed, summing to the same total (cells conserved).
        if let Node::Split { children, .. } = &app.root {
            let (a, bb) = (children[0].0, children[1].0);
            assert_eq!(a, Size::Fixed(26));
            match bb {
                Size::Fixed(n) => assert_eq!(26 + n, sum_before),
                _ => panic!("neighbor should be fixed after drag"),
            }
        }
        // to_layout round-trips the new fixed sizes.
        let layout = build::to_layout(&app.root);
        let json = serde_json::to_string(&layout).unwrap();
        assert!(json.contains("\"size\":\"26\""));
    }

    #[test]
    fn focus_cycles_and_wraps() {
        let mut app = test_app();
        app.focused = 0;
        app.focus_next();
        app.focus_next();
        assert_eq!(app.focused, 2);
        app.focus_next();
        assert_eq!(app.focused, 0);
        app.focus_prev();
        assert_eq!(app.focused, 2);
    }

    /// The whole UI, at sizes a real terminal can actually be, in every overlay
    /// state. TUIs die on u16 arithmetic — a centred box wider than the screen,
    /// a padded area with negative width, a label longer than its column — and a
    /// panic here leaves the terminal in raw mode, so a mangled shell is the
    /// user-visible cost. Cheap to assert, so assert it broadly.
    #[test]
    fn renders_at_absurd_sizes_without_panicking() {
        let sizes = [
            (1, 1),
            (1, 40),
            (40, 1),
            (2, 2),
            (3, 3),
            (4, 4),
            (5, 8),
            (8, 5),
            (12, 6),
            (20, 3),
            (30, 10),
            (80, 24),
            (250, 4),
            (4, 250),
        ];
        for cfg in [Config::default(), Config::basics()] {
            for (w, h) in sizes {
                // Normal, help open, palette open (empty and with a query), and
                // a non-first tab — every state has its own geometry maths.
                let mut app = demo_app(cfg.clone());
                render_snapshot(&mut app, w, h);

                let mut app = demo_app(cfg.clone());
                app.help = true;
                app.help_scroll = 3;
                render_snapshot(&mut app, w, h);

                let mut app = demo_app(cfg.clone());
                app.pal = true;
                render_snapshot(&mut app, w, h);
                app.pal_input = "refresh quota".into();
                app.pal_sel = 5;
                render_snapshot(&mut app, w, h);

                let mut app = demo_app(cfg.clone());
                app.next_tab();
                render_snapshot(&mut app, w, h);

                // The conflict prompt, whose box is wider than the palette's and
                // therefore clips first on a narrow terminal.
                let mut app = demo_app(cfg.clone());
                // Whatever the cursor is on — demo_app's pin state is persisted,
                // so the selected mission is not always the first one.
                let sel = app.world.selected().map(|m| m.id.clone()).unwrap_or_default();
                app.world.live = vec![live_session(&sel)];
                app.begin_resume();
                assert!(app.conflict.is_some(), "a live mission must raise the prompt");
                render_snapshot(&mut app, w, h);

                // The options overlay, both idle and mid-edit: the widest box of
                // the three, and the one that renders a caret.
                let mut app = demo_app(cfg.clone());
                app.begin_options();
                render_snapshot(&mut app, w, h);
                if let Some(o) = app.opts.as_mut() {
                    o.row = 4;
                    o.editing = Some("a-very-long-worktree-name-that-will-not-fit".into());
                }
                render_snapshot(&mut app, w, h);

                // And with nothing scanned yet, which is what the first frame
                // after launch actually looks like.
                let mut app = demo_app(cfg.clone());
                app.world.missions.clear();
                app.world.visible.clear();
                app.world.scanning = true;
                render_snapshot(&mut app, w, h);
            }
        }
    }

    /// Every key, in every overlay state, against a full layout. The palette in
    /// particular does index arithmetic on a list whose length changes with each
    /// keystroke, so a selection can outrun its matches; the help overlay
    /// re-dispatches unknown keys into the normal handler. Neither may panic,
    /// and the invariants they rely on must hold afterwards.
    #[test]
    fn every_key_in_every_state_is_survivable() {
        use ratatui::crossterm::event::{KeyCode, KeyEvent, KeyModifiers};

        // Includes the keys that OPEN the newer overlays ('o') and the ones that
        // only mean something inside them ('f', 'X', 'd', 'L'), so the loop walks
        // into those states by itself instead of only testing the old two.
        let mut codes: Vec<KeyCode> = "?:jkgGaeqrnp*[]12390AZ/oLfXd \t".chars().map(KeyCode::Char).collect();
        codes.extend([
            KeyCode::Up,
            KeyCode::Down,
            KeyCode::Enter,
            KeyCode::Esc,
            KeyCode::Tab,
            KeyCode::BackTab,
            KeyCode::Backspace,
            KeyCode::PageUp,
            KeyCode::PageDown,
            KeyCode::Left,
            KeyCode::Home,
            KeyCode::F(5),
        ]);
        let mods = [KeyModifiers::NONE, KeyModifiers::CONTROL, KeyModifiers::SHIFT];

        for open_help in [false, true] {
            for open_pal in [false, true] {
                let mut app = app_with(Config::basics());
                app.help = open_help;
                app.pal = open_pal;
                for &code in &codes {
                    for &m in &mods {
                        let k = KeyEvent::new(code, m);
                        // Route exactly as the event loop does.
                        if app.conflict.is_some() {
                            conflict_key(&mut app, k);
                        } else if app.opts.is_some() {
                            options_key(&mut app, k);
                        } else if app.pal {
                            palette_key(&mut app, k);
                        } else if app.help {
                            help_key(&mut app, k);
                        } else {
                            normal_key(&mut app, k);
                        }

                        // Invariants that later frames depend on.
                        assert!(app.active < app.tabs.len(), "active tab out of range");
                        assert!(app.focused < app.panes.max(1), "focus out of range");
                        assert!(
                            app.world.cursor == 0 || app.world.cursor < app.world.visible.len(),
                            "mission cursor out of range"
                        );
                        assert!(
                            app.pal_sel == 0 || app.pal_sel < shell::palette_matches(&app).len().max(1),
                            "palette selection outran its matches"
                        );
                        // Whatever state we ended in must still draw.
                        render_snapshot(&mut app, 60, 20);
                    }
                }
            }
        }
    }

    /// The api version in a manifest is a CONTRACT, so a mismatch must be
    /// refused. Loading such a plugin anyway either explodes as a confusing
    /// parse error or — worse — looks like it works while a field it expects is
    /// quietly absent.
    #[test]
    fn a_plugin_targeting_another_api_is_blocked_not_loaded() {
        use houston_core::plugin::{Plugin, PluginManifest, Runtime, WidgetDecl};
        let mk = |api: u32| Plugin {
            dir: std::env::temp_dir(),
            manifest: PluginManifest {
                api,
                name: "futuristic".into(),
                widgets: vec![WidgetDecl { id: "future".into(), title: "Future".into() }],
                // An exec runtime, so a load failure could not be what blocks it.
                runtime: Runtime::Exec { command: vec!["pwsh".into()] },
                ..Default::default()
            },
        };

        // A future api, and an UNSTATED one (0), are both refused: "unstated" is
        // not the same as "compatible".
        for api in [houston_api::API_VERSION + 1, 0] {
            let w = build::widget_for_test("future", None, &[mk(api)]);
            assert_eq!(w.id(), "future", "the pane keeps the layout's id");
            let title = w.title(&test_app().world);
            assert!(title.starts_with('⚠'), "a blocked plugin is visibly flagged, got {title:?}");
        }

        // The matching api gets past the version gate (it then hits the trust
        // gate, which is a different, also-inert pane — either way it never runs
        // the command unreviewed).
        let w = build::widget_for_test("future", None, &[mk(houston_api::API_VERSION)]);
        assert_eq!(w.id(), "future");
    }

    #[test]
    fn default_config_is_single_tab() {
        let app = test_app();
        // One VIEW from the config, plus Houston's own Settings tab, which is
        // always there and is not one of the config's.
        assert_eq!(app.view_tabs().len(), 1);
        assert_eq!(app.tabs.len(), 2);
        assert!(app.tabs.iter().filter(|t| t.synthetic).count() == 1);
        assert_eq!(app.active, 0);
    }

    #[test]
    fn basics_has_two_tabs_and_switch_preserves_state() {
        let mut app = app_with(Config::basics());
        assert_eq!(app.view_tabs().len(), 3, "three configured views");
        assert_eq!(app.tabs.len(), 4, "plus Settings");
        assert_eq!(app.tabs[0].name, "Missions");
        // The live Missions tab has the batteries layout (filters+quota, etc.).
        let missions_panes = app.panes;
        assert!(missions_panes >= 4);

        // Switch to Focus: live fields now describe the Focus tab (2 panes).
        app.next_tab();
        assert_eq!(app.active, 1);
        assert_eq!(app.panes, 2);
        let mut ids = Vec::new();
        widget_ids(&app.root, &mut ids);
        assert_eq!(ids, vec!["missions", "preview"]);

        // Switching back restores the Missions tab's pane count intact.
        app.prev_tab();
        assert_eq!(app.active, 0);
        assert_eq!(app.panes, missions_panes);
    }

    /// Houston's own tab: reachable with `0`, never by cycling the views, and
    /// never written into the user's config. The last part is the one that would
    /// hurt quietly — persisting it would invent a tab they never asked for and,
    /// on a default config, flip `persist` into its multi-tab branch.
    /// Enter is claimed by the focused pane before the app's default.
    ///
    /// The bug this pins: the Settings pane advertises `↵ change`, but Enter is a
    /// global key that `on_key` never sees — so pressing it in Settings queued a
    /// RESUME of whatever chat the missions list had selected. Only the space bar
    /// worked, and the footer said otherwise.
    #[test]
    fn enter_goes_to_the_pane_that_claims_it_before_resuming_a_chat() {
        use ratatui::crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
        let enter = KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE);
        let mut app = demo_app(Config::basics());

        // On a missions pane, Enter still resumes — the settled one-keystroke rule.
        assert_eq!(app.focused_widget_id().as_deref(), Some("missions"));
        normal_key(&mut app, enter);
        assert!(matches!(app.pending, Some(Action::Resume { .. })), "missions must still resume on Enter");
        app.pending = None;

        // On the Settings tab, Enter belongs to the pane and queues nothing.
        normal_key(&mut app, KeyEvent::new(KeyCode::Char('0'), KeyModifiers::NONE));
        assert_eq!(app.focused_widget_id().as_deref(), Some("settings"));
        normal_key(&mut app, enter);
        assert!(app.pending.is_none(), "Enter in Settings must not resume a chat");
    }

    #[test]
    fn the_settings_tab_is_houstons_own_and_stays_out_of_the_config() {
        use ratatui::crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
        let key = |c: char| KeyEvent::new(KeyCode::Char(c), KeyModifiers::NONE);
        let mut app = app_with(Config::basics());
        let settings = app.tabs.iter().position(|t| t.synthetic).expect("always present");

        // `0` gets there; the digits that select views never do.
        normal_key(&mut app, key('0'));
        assert_eq!(app.active, settings);
        for d in ['1', '2', '3', '4'] {
            normal_key(&mut app, key(d));
            assert_ne!(app.active, settings, "digit {d} selected the Settings tab");
        }

        // Cycling stays among the views, however long you press it.
        for _ in 0..8 {
            normal_key(&mut app, key(']'));
            assert_ne!(app.active, settings, "] cycled into Settings");
        }
        // …but cycling FROM Settings re-enters the views rather than sticking.
        normal_key(&mut app, key('0'));
        assert_eq!(app.active, settings);
        normal_key(&mut app, key(']'));
        assert_ne!(app.active, settings);

        // Persisting from inside Settings writes only the configured tabs.
        normal_key(&mut app, key('0'));
        app.dirty = true;
        app.persist();
        let names: Vec<&str> = app.cfg.tabs.iter().map(|t| t.name.as_str()).collect();
        assert_eq!(names, vec!["Missions", "Focus", "Accounts"], "Settings must not appear in the config");

        // And on a single-view config, persisting still takes the single-tab
        // branch: `tabs` cleared, the layout written to `layout`.
        let mut single = test_app();
        normal_key(&mut single, key('0'));
        single.dirty = true;
        single.persist();
        assert!(single.cfg.tabs.is_empty(), "a one-view config must not grow a tabs array");
    }

    /// The bug this test exists for DESTROYED a real layout, and the screenshot of
    /// it was unmistakable: one pane saying "unknown widget: settings".
    ///
    /// `self.root` holds whichever tab is LIVE, so while Settings is open it holds
    /// the Settings tree. Persisting from there wrote `{"widget":"settings"}` as
    /// the user's entire layout and cleared their tabs, and the next start had
    /// nothing left to draw. The first version of the test above walked this exact
    /// path and asserted only that `tabs` was empty — which was true, and useless.
    #[test]
    fn persisting_from_the_settings_tab_never_overwrites_a_layout() {
        use ratatui::crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
        let key = |c: char| KeyEvent::new(KeyCode::Char(c), KeyModifiers::NONE);

        let widget_ids_of = |node: &houston_core::config::LayoutNode| {
            let mut out = Vec::new();
            fn walk(n: &houston_core::config::LayoutNode, out: &mut Vec<String>) {
                match n {
                    houston_core::config::LayoutNode::Pane { widget, .. } => out.push(widget.clone()),
                    houston_core::config::LayoutNode::Split { children, .. } => {
                        for c in children {
                            walk(&c.node, out);
                        }
                    }
                }
            }
            walk(node, &mut out);
            out
        };

        // One configured view — the shape the damage happened on.
        let mut app = test_app();
        let before = widget_ids_of(&app.cfg.layout);
        assert!(before.contains(&"missions".to_string()), "{before:?}");

        normal_key(&mut app, key('0'));
        assert!(app.tabs[app.active].synthetic, "we are on Settings");
        app.dirty = true;
        app.persist();

        let after = widget_ids_of(&app.cfg.layout);
        assert_eq!(after, before, "the user's layout is untouched by saving from Settings");
        assert!(!after.contains(&"settings".to_string()), "the Settings pane leaked into the layout: {after:?}");

        // Same for a multi-view config: every tab keeps its own tree.
        let mut multi = app_with(Config::basics());
        let names_before: Vec<String> = multi.cfg.tabs.iter().map(|t| t.name.clone()).collect();
        let first_before = widget_ids_of(&multi.cfg.tabs[0].layout);
        normal_key(&mut multi, key('0'));
        multi.dirty = true;
        multi.persist();
        assert_eq!(multi.cfg.tabs.iter().map(|t| t.name.clone()).collect::<Vec<_>>(), names_before);
        assert_eq!(widget_ids_of(&multi.cfg.tabs[0].layout), first_before);
        for t in &multi.cfg.tabs {
            assert!(!widget_ids_of(&t.layout).contains(&"settings".to_string()), "leaked into {}", t.name);
        }
    }

    /// And if a layout DOES name `settings` — a hand edit, or a file written by
    /// the version with the bug above — it must render the real pane rather than
    /// "unknown widget".
    #[test]
    fn a_layout_naming_settings_gets_the_real_pane() {
        let w = build::widget_for_test("settings", None, &[]);
        assert_eq!(w.id(), "settings");
        assert!(!w.title(&test_app().world).starts_with('?'), "not a placeholder");
    }

    #[test]
    fn digit_and_bracket_keys_switch_tabs_only_when_multitab() {
        use ratatui::crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
        let mut app = app_with(Config::basics());
        // ']' advances, '[' goes back, digit jumps.
        assert!(!normal_key(&mut app, KeyEvent::new(KeyCode::Char(']'), KeyModifiers::NONE)));
        assert_eq!(app.active, 1);
        assert!(!normal_key(&mut app, KeyEvent::new(KeyCode::Char('1'), KeyModifiers::NONE)));
        assert_eq!(app.active, 0);

        // Single-tab: the same keys fall through to the widget (no tab change,
        // and no panic — active stays 0).
        let mut single = test_app();
        let _ = normal_key(&mut single, KeyEvent::new(KeyCode::Char(']'), KeyModifiers::NONE));
        assert_eq!(single.active, 0);
    }
}
