//! User configuration for Houston 2.0: the whole layout TREE plus the theme,
//! declarative and serde-backed. Unlike v1 (three layout numbers + colors),
//! the layout here is a tree of splits and panes — the UI is data.
//!
//! Own file `<store>/config-v2.json`, so v1's `config.json` is never touched
//! while the two coexist. A missing or malformed config yields the built-in
//! default (a monochrome three-column layout): a typo can never break startup.
//! Houston writes it back when the layout changes (resize/move), atomically.

use crate::paths::store_dir;
use serde::{Deserialize, Serialize};
use std::fs;
use std::path::PathBuf;
use std::str::FromStr;

/// How a split divides space among a child.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SizeSpec {
    /// A fixed number of cells.
    Fixed(u16),
    /// A percentage of the parent (0–100).
    Percent(u16),
    /// A share of the leftover space (weight, like CSS `fr`).
    Fill(u16),
}

impl SizeSpec {
    pub fn as_string(&self) -> String {
        match self {
            SizeSpec::Fixed(n) => n.to_string(),
            SizeSpec::Percent(n) => format!("{n}%"),
            SizeSpec::Fill(n) => format!("{n}fr"),
        }
    }
}

impl FromStr for SizeSpec {
    type Err = String;
    fn from_str(s: &str) -> Result<Self, String> {
        let s = s.trim();
        if let Some(n) = s.strip_suffix('%') {
            Ok(SizeSpec::Percent(n.trim().parse().map_err(|_| "bad percent")?))
        } else if let Some(n) = s.strip_suffix("fr") {
            Ok(SizeSpec::Fill(n.trim().parse().map_err(|_| "bad fr")?))
        } else {
            Ok(SizeSpec::Fixed(s.parse().map_err(|_| "bad cells")?))
        }
    }
}

// Serialize as the compact string form ("20", "42%", "1fr") for hand-editing.
impl Serialize for SizeSpec {
    fn serialize<S: serde::Serializer>(&self, ser: S) -> Result<S::Ok, S::Error> {
        ser.serialize_str(&self.as_string())
    }
}
impl<'de> Deserialize<'de> for SizeSpec {
    fn deserialize<D: serde::Deserializer<'de>>(de: D) -> Result<Self, D::Error> {
        let s = String::deserialize(de)?;
        s.parse().map_err(serde::de::Error::custom)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Dir {
    /// Children laid out left-to-right.
    Row,
    /// Children stacked top-to-bottom.
    Col,
}

/// A node in the layout tree: a split (with sized children) or a leaf pane
/// bound to a widget by id ("filters", "missions", "preview", "probe", or a
/// plugin's).
///
/// A pane may carry its own `settings`, which is what makes a widget
/// configurable per instance rather than per build: two `probe` panes can watch
/// two different hosts. Plugins already had this in their `RenderRequest` — the
/// field existed and was always `null`, because nothing filled it in.
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "lowercase")]
pub enum LayoutNode {
    Split {
        dir: Dir,
        children: Vec<LayoutChild>,
    },
    Pane {
        widget: String,
        /// Free-form, interpreted by the widget. Absent for the panes that need
        /// nothing, so the common layout stays as short as it was.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        settings: Option<serde_json::Value>,
    },
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct LayoutChild {
    pub size: SizeSpec,
    #[serde(flatten)]
    pub node: LayoutNode,
}

/// Colors are strings the TUI resolves: a name ("white", "darkgray", "gray",
/// "black", "red"…) or an ANSI-256 index ("240"). Kept as strings in the
/// kernel, which has no TUI dependency.
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct Theme {
    pub fg: String,
    /// Very quiet gray: idle borders and separators ONLY, never body text.
    pub dim: String,
    /// Readable secondary gray for text (labels, footer, inactive tabs) —
    /// deliberately lighter than `dim` so secondary text never turns muddy.
    pub grey: String,
    pub accent: String,
    pub sel_fg: String,
    pub sel_bg: String,
    pub border: String,
    pub border_focus: String,
    pub header_fg: String,
    pub header_bg: String,
}

impl Default for Theme {
    /// Black & white out of the box — the user adds color.
    fn default() -> Self {
        Theme {
            fg: "white".into(),
            dim: "darkgray".into(),
            grey: "gray".into(),
            accent: "white".into(),
            sel_fg: "black".into(),
            sel_bg: "white".into(),
            border: "darkgray".into(),
            border_focus: "white".into(),
            header_fg: "black".into(),
            header_bg: "white".into(),
        }
    }
}

/// A named tab: its own full layout tree. Tabs are top-level views over the
/// same mission data (v1 had Missions/Accounts/Notices); switch with digits or
/// `[`/`]`. Serialized flat, like LayoutChild: `{"name":"…","type":"split",…}`.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct TabConfig {
    pub name: String,
    #[serde(flatten)]
    pub layout: LayoutNode,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct Config {
    pub theme: Theme,
    /// The single-tab layout (used when `tabs` is empty — the minimal core).
    pub layout: LayoutNode,
    /// Named tabs. When non-empty these REPLACE `layout` as the tab set.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub tabs: Vec<TabConfig>,
    /// Extra status-line segments, in the order they appear. Each names a file in
    /// `<store>/segments/` that some other process writes (see `segments`);
    /// Houston only ever reads them, because a render may not execute anything.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub segments: Vec<SegmentConfig>,
    /// Set when the file EXISTED but could not be parsed, so this value is the
    /// default standing in for content that is still on disk.
    ///
    /// It exists to make `save_to` refuse. Without it the sequence is: a typo in
    /// a hand-edited config → load falls back to the default in memory → the
    /// first border drag persists → the real config is gone, replaced by
    /// defaults, with nothing on screen having said so. A single mistyped comma
    /// should not cost a layout.
    ///
    /// `skip` on both sides: it is a fact about THIS load, never content.
    #[serde(skip)]
    pub unreadable: bool,
}

/// One configured status-line segment.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct SegmentConfig {
    /// File stem under `<store>/segments/` (`connect` → `connect.txt`).
    pub name: String,
    /// Hide the segment when its file has not been written for this long.
    /// Omitted = never expire, which suits something slow-moving and not a
    /// heartbeat — the producer knows which it is, so it is stated here.
    ///
    /// This file is snake_case throughout (`sel_fg`, `border_focus`), but Claude's
    /// own settings are camelCase, so both spellings are accepted: a config
    /// silently ignoring a key you typed in the obvious style is a bad afternoon.
    #[serde(default, alias = "ttlSecs", skip_serializing_if = "Option::is_none")]
    pub ttl_secs: Option<u64>,
    /// Character cap, defaulting to `segments::DEFAULT_MAX_CHARS`.
    #[serde(default, alias = "maxChars", skip_serializing_if = "Option::is_none")]
    pub max_chars: Option<usize>,
}

impl Config {
    /// The segment specs to read, resolved with their defaults. Nameless entries
    /// are dropped rather than reading `.txt`.
    pub fn segment_specs(&self) -> Vec<crate::segments::Spec> {
        self.segments
            .iter()
            .filter(|s| !s.name.trim().is_empty())
            .map(|s| crate::segments::Spec {
                name: s.name.trim().to_string(),
                ttl: s.ttl_secs.map(std::time::Duration::from_secs),
                max_chars: s.max_chars.unwrap_or(crate::segments::DEFAULT_MAX_CHARS),
            })
            .collect()
    }
}

impl Default for Config {
    fn default() -> Self {
        Config {
            theme: Theme::default(),
            layout: default_layout(),
            tabs: Vec::new(),
            segments: Vec::new(),
            unreadable: false,
        }
    }
}

impl Theme {
    /// The houston-basics palette. Five roles, kept distinct on purpose:
    /// background is the terminal's own (never painted); text is white (`fg`);
    /// decoration is gray (`dim`, idle `border`); accent is blue (ANSI 39) —
    /// used sparingly for the focused border, section headers and key labels;
    /// and the contrast/highlight is WHITE — the selected row and the active
    /// tab, so the "here you are" element pops hardest against the dark base.
    /// v1's blue accent stays; white does the high-contrast highlighting.
    pub fn blue() -> Self {
        Theme {
            fg: "white".into(),
            dim: "240".into(),
            grey: "245".into(),
            accent: "39".into(),
            // Highlight = a white bar with black text (the contrast colour).
            sel_fg: "black".into(),
            sel_bg: "white".into(),
            border: "240".into(),
            border_focus: "39".into(),
            // Header: white text on the blue accent bar; the active tab renders
            // reversed, so it becomes a white chip with blue text.
            header_fg: "white".into(),
            header_bg: "39".into(),
        }
    }
}

impl Config {
    /// The houston-basics preset: batteries on, the classic blue theme, and two
    /// tabs — the full "Missions" view plus a distraction-free "Focus" view —
    /// so tabs are real out of the box for existing users.
    pub fn basics() -> Self {
        Config {
            theme: Theme::blue(),
            layout: default_layout_basics(),
            tabs: vec![
                TabConfig { name: "Missions".into(), layout: default_layout_basics() },
                TabConfig { name: "Focus".into(), layout: default_layout_focus() },
                TabConfig { name: "Accounts".into(), layout: pane("basics:accounts") },
            ],
            // No segments by default: one is only useful once something writes it,
            // and a configured-but-never-written segment is invisible anyway.
            segments: Vec::new(),
            unreadable: false,
        }
    }

    /// The resolved tab set: the named `tabs` if any, else a single "Missions"
    /// tab wrapping `layout`. Always at least one tab.
    pub fn resolved_tabs(&self) -> Vec<(String, LayoutNode)> {
        if self.tabs.is_empty() {
            vec![("Missions".into(), self.layout.clone())]
        } else {
            self.tabs.iter().map(|t| (t.name.clone(), t.layout.clone())).collect()
        }
    }
}

/// The MINIMAL default layout: fixed Filters, filling Missions, percentage
/// Preview — the bare three columns of the core, no batteries.
pub fn default_layout() -> LayoutNode {
    LayoutNode::Split {
        dir: Dir::Row,
        children: vec![
            LayoutChild { size: SizeSpec::Fixed(20), node: pane("filters") },
            LayoutChild { size: SizeSpec::Fill(1), node: pane("missions") },
            LayoutChild { size: SizeSpec::Percent(42), node: pane("preview") },
        ],
    }
}

/// A pane with no settings of its own — which is most of them.
fn pane(w: &str) -> LayoutNode {
    LayoutNode::Pane { widget: w.into(), settings: None }
}

/// The houston-basics layout — the "batteries" arrangement that reproduces the
/// v1 feel: the three columns, PLUS a quota panel bottom-left (the classic
/// wish) and a git strip under the preview. All widgets ship in the binary.
pub fn default_layout_basics() -> LayoutNode {
    LayoutNode::Split {
        dir: Dir::Row,
        children: vec![
            // Left column: filters on top, the quota bars pinned at the bottom.
            LayoutChild {
                size: SizeSpec::Fixed(22),
                node: LayoutNode::Split {
                    dir: Dir::Col,
                    children: vec![
                        LayoutChild { size: SizeSpec::Fill(1), node: pane("filters") },
                        LayoutChild { size: SizeSpec::Fixed(7), node: pane("basics:quota") },
                    ],
                },
            },
            LayoutChild { size: SizeSpec::Fill(1), node: pane("missions") },
            // Right column: the preview, with a live git strip beneath it.
            LayoutChild {
                size: SizeSpec::Percent(42),
                node: LayoutNode::Split {
                    dir: Dir::Col,
                    children: vec![
                        LayoutChild { size: SizeSpec::Fill(1), node: pane("preview") },
                        LayoutChild { size: SizeSpec::Fixed(4), node: pane("basics:git") },
                    ],
                },
            },
        ],
    }
}

/// The "Focus" tab: just Missions and Preview, side by side — the reading
/// view, no filters or strips.
pub fn default_layout_focus() -> LayoutNode {
    LayoutNode::Split {
        dir: Dir::Row,
        children: vec![
            LayoutChild { size: SizeSpec::Fill(1), node: pane("missions") },
            LayoutChild { size: SizeSpec::Percent(55), node: pane("preview") },
        ],
    }
}

pub fn config_path() -> PathBuf {
    store_dir().join("config-v2.json")
}

impl Config {
    /// Load the config; a missing or malformed file yields the default IN
    /// MEMORY without writing anything — first-run provisioning (not load)
    /// owns creating the file, so "does config-v2.json exist?" stays a
    /// reliable first-run signal.
    pub fn load() -> Config {
        Self::load_from(config_path())
    }

    pub fn load_from(path: PathBuf) -> Config {
        match fs::read(&path) {
            Ok(b) => {
                // Strip a UTF-8 BOM some editors prepend; serde would reject it.
                let b = b.strip_prefix(&[0xEF, 0xBB, 0xBF]).unwrap_or(&b);
                match serde_json::from_slice(b) {
                    Ok(cfg) => cfg,
                    // The file is THERE and unreadable. Standing in with the
                    // default is right for rendering, but the default must not
                    // be allowed to overwrite it — see `Config::unreadable`.
                    Err(_) => Config { unreadable: true, ..Config::default() },
                }
            }
            // Genuinely absent: the default is the whole truth, and saving it is
            // exactly what first-run provisioning does.
            Err(_) => Config::default(),
        }
    }

    /// Whether a config file already exists — the "already provisioned" signal.
    pub fn exists() -> bool {
        config_path().exists()
    }

    pub fn save(&self) -> std::io::Result<()> {
        self.save_to(&config_path())
    }

    /// Write the config, atomically.
    ///
    /// Two things this must not do, both learned from real damage:
    ///
    /// - **Never a fixed temp name.** This used to write `config-v2.json.tmp`
    ///   directly, so two savers — two TUIs, or a TUI and `provision` — could
    ///   interleave into one temp and rename half a document into place. Every
    ///   other writer in this crate goes through `atomic::write`, whose header
    ///   documents this exact hazard; this one simply never did.
    /// - **Never overwrite content it could not read.** A config that failed to
    ///   parse is still the user's config; the in-memory default standing in for
    ///   it is not a value worth persisting over it.
    pub fn save_to(&self, path: &std::path::Path) -> std::io::Result<()> {
        if self.unreadable {
            return Err(std::io::Error::other(format!(
                "{} exists but could not be parsed; refusing to overwrite it with defaults. \
                 Fix or move the file, then try again.",
                path.display()
            )));
        }
        if let Some(dir) = path.parent() {
            fs::create_dir_all(dir)?;
        }
        crate::atomic::write(path, &serde_json::to_vec_pretty(self)?)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A config that exists and does not parse must survive the session that
    /// could not read it. The sequence this pins: typo → load stands in with the
    /// default → any persist (a border drag does one) → the file is defaults and
    /// the user's layout is gone with nothing said.
    #[test]
    fn an_unreadable_config_is_never_overwritten_by_the_default() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("config-v2.json");
        let original = b"{ \"theme\": {}, oops }";
        fs::write(&p, original).unwrap();

        let cfg = Config::load_from(p.clone());
        assert!(cfg.unreadable, "the file was there; standing in is not the same as absent");
        let err = cfg.save_to(&p).expect_err("saving the stand-in over the real file must fail");
        assert!(err.to_string().contains("refusing to overwrite"), "{err}");
        assert_eq!(fs::read(&p).unwrap(), original, "not one byte of the user's file may change");
    }

    /// The flag is about THIS load, not about content: it must never round-trip
    /// into the file (or a saved config would come back refusing to save).
    #[test]
    fn the_unreadable_flag_is_not_content() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("config-v2.json");
        Config::default().save_to(&p).unwrap();
        let text = fs::read_to_string(&p).unwrap();
        assert!(!text.contains("unreadable"), "{text}");
        assert!(!Config::load_from(p).unreadable);
    }

    /// A missing file is the first-run case: the default IS the truth and saving
    /// it is what provisioning does.
    #[test]
    fn a_missing_config_saves_the_default_fine() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("nested").join("config-v2.json");
        let cfg = Config::load_from(p.clone());
        assert!(!cfg.unreadable);
        cfg.save_to(&p).unwrap();
        assert!(Config::load_from(p).tabs.is_empty());
    }

    /// The write goes through `atomic::write`, so it never touches the fixed
    /// `config-v2.json.tmp` that two concurrent savers used to share — which is
    /// how half a document got renamed into place.
    ///
    /// Occupying that name with a directory is the cheap way to say "nobody may
    /// use this path": the old code wrote to it and failed, `atomic::write`
    /// never looks at it. Asserting only that no temp SURVIVES would prove
    /// nothing, because the rename removed the shared temp too.
    #[test]
    fn saving_does_not_go_through_the_shared_temp_name() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("config-v2.json");
        let squatted = tmp.path().join("config-v2.json.tmp");
        fs::create_dir(&squatted).unwrap();

        Config::default().save_to(&p).expect("a busy fixed temp name must not fail the save");
        assert!(squatted.is_dir(), "the shared temp name was written to");

        let left: Vec<String> = fs::read_dir(tmp.path())
            .unwrap()
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|n| n != "config-v2.json" && n != "config-v2.json.tmp")
            .collect();
        assert!(left.is_empty(), "temps must not survive: {left:?}");
    }

    /// Segments are hand-written config, so both the file's own snake_case and
    /// the camelCase people arrive with from Claude's settings must work — and a
    /// nameless entry must not turn into a read of `.txt`.
    #[test]
    fn segment_config_accepts_both_spellings() {
        let json = r#"{
            "theme": {}, "layout": {"type":"pane","widget":"missions"},
            "segments": [
              {"name":"a","ttl_secs":30,"max_chars":8},
              {"name":"b","ttlSecs":60,"maxChars":10},
              {"name":"c"},
              {"name":"  "}
            ]
        }"#;
        let cfg: Config = serde_json::from_str(json).expect("config parses");
        let specs = cfg.segment_specs();
        assert_eq!(specs.len(), 3, "the nameless entry is dropped");
        assert_eq!(specs[0].ttl, Some(std::time::Duration::from_secs(30)));
        assert_eq!(specs[0].max_chars, 8);
        assert_eq!(specs[1].ttl, Some(std::time::Duration::from_secs(60)), "camelCase is accepted too");
        assert_eq!(specs[1].max_chars, 10);
        assert_eq!(specs[2].ttl, None, "no ttl means never expire");
        assert_eq!(specs[2].max_chars, crate::segments::DEFAULT_MAX_CHARS);
        // A config with no segments key at all is the normal case.
        let bare: Config = serde_json::from_str(r#"{"theme":{},"layout":{"type":"pane","widget":"missions"}}"#).unwrap();
        assert!(bare.segment_specs().is_empty());
    }

    #[test]
    fn size_spec_string_roundtrip() {
        for (s, spec) in [
            ("20", SizeSpec::Fixed(20)),
            ("42%", SizeSpec::Percent(42)),
            ("2fr", SizeSpec::Fill(2)),
        ] {
            assert_eq!(s.parse::<SizeSpec>().unwrap(), spec);
            assert_eq!(spec.as_string(), s);
        }
        assert!("nope".parse::<SizeSpec>().is_err());
    }

    #[test]
    fn default_config_json_is_compact_and_reparses() {
        let c = Config::default();
        let json = serde_json::to_string(&c).unwrap();
        // The compact size strings made it in.
        assert!(json.contains("\"size\":\"20\""));
        assert!(json.contains("\"widget\":\"missions\""));
        let back: Config = serde_json::from_str(&json).unwrap();
        match back.layout {
            LayoutNode::Split { dir, children } => {
                assert_eq!(dir, Dir::Row);
                assert_eq!(children.len(), 3);
                assert_eq!(children[0].size, SizeSpec::Fixed(20));
            }
            _ => panic!("expected a split"),
        }
    }

    #[test]
    fn malformed_and_missing_yield_default_without_seeding() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("config-v2.json");
        // Missing → default IN MEMORY, and NOT written (first-run signal stays).
        let c = Config::load_from(p.clone());
        assert!(matches!(c.layout, LayoutNode::Split { .. }));
        assert!(!p.exists(), "load must not seed — provisioning owns that");
        // Malformed → default, no panic.
        fs::write(&p, b"{ not json").unwrap();
        let c2 = Config::load_from(p);
        assert_eq!(c2.theme.fg, "white");
    }

    #[test]
    fn basics_layout_places_quota_and_git() {
        let json = serde_json::to_string(&default_layout_basics()).unwrap();
        assert!(json.contains("basics:quota"));
        assert!(json.contains("basics:git"));
    }

    #[test]
    fn default_resolves_to_single_tab_basics_to_two() {
        let single = Config::default().resolved_tabs();
        assert_eq!(single.len(), 1);
        assert_eq!(single[0].0, "Missions");
        let basics = Config::basics().resolved_tabs();
        assert_eq!(basics.len(), 3);
        assert_eq!(basics[1].0, "Focus");
        assert_eq!(basics[2].0, "Accounts");
    }

    #[test]
    fn tabs_roundtrip_through_json_flat() {
        let c = Config::basics();
        let json = serde_json::to_string(&c).unwrap();
        assert!(json.contains("\"name\":\"Focus\""));
        let back: Config = serde_json::from_str(&json).unwrap();
        assert_eq!(back.tabs.len(), 3);
        assert_eq!(back.tabs[0].name, "Missions");
        assert!(matches!(back.tabs[1].layout, LayoutNode::Split { .. }));
        assert_eq!(back.tabs[2].name, "Accounts");
    }

    #[test]
    fn theme_default_is_monochrome() {
        let t = Theme::default();
        assert_eq!(t.accent, "white");
        assert_eq!(t.sel_bg, "white");
        assert_eq!(t.border, "darkgray");
    }
}
