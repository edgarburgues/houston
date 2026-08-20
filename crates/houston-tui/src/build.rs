//! Bridge between the declarative config (houston_core::config) and the live
//! widget tree. `build_tree` instantiates widgets by id: a built-in name wins
//! first; else a discovered plugin that declares the id gets mounted as a
//! `PluginWidget`; else a visible `MissingWidget` placeholder (forward-compat:
//! a layout may reference a widget not present yet). `to_layout` serializes
//! the live tree's structure and sizes back to config so resize can persist.

use crate::basics::{AccountsWidget, GitWidget, QuotaWidget};
use crate::widgets::{BlockedWidget, FiltersWidget, MissingWidget, MissionsWidget, PreviewWidget};
use crate::{Node, PluginWidget, Size};
use houston_core::config::{Dir, LayoutChild, LayoutNode, SizeSpec};
use houston_core::plugin::{Plugin, Runtime};
use ratatui::layout::Direction;

fn is_builtin(id: &str) -> bool {
    matches!(
        id,
        "filters" | "missions" | "preview" | "probe" | "settings" | "basics:quota" | "basics:git" | "basics:accounts"
    )
}

fn builtin(id: &str, settings: Option<&serde_json::Value>) -> Box<dyn crate::Widget> {
    let mut w: Box<dyn crate::Widget> = match id {
        "filters" => Box::new(FiltersWidget),
        "missions" => Box::<MissionsWidget>::default(),
        "basics:quota" => Box::<QuotaWidget>::default(),
        "basics:git" => Box::<GitWidget>::default(),
        "basics:accounts" => Box::<AccountsWidget>::default(),
        "probe" => Box::<crate::probe::ProbeWidget>::default(),
        // A real id, not only the synthetic tab's widget: a config that names
        // `settings` — whether hand-written or written by a Houston with the
        // persist bug that once did it — must show the pane, not "unknown
        // widget". It also lets someone mount it inside their own layout.
        "settings" => Box::<crate::settings_view::SettingsWidget>::default(),
        _ => Box::<PreviewWidget>::default(),
    };
    // One place hands every built-in its pane settings; the widgets that need
    // none inherit the trait's no-op.
    if let Some(v) = settings {
        w.configure(v);
    }
    w
}

/// Find the plugin (and its widget decl) that provides `id`. First declaring
/// plugin wins (discovery is name-sorted).
fn plugin_for<'a>(id: &str, plugins: &'a [Plugin]) -> Option<&'a Plugin> {
    plugins
        .iter()
        .find(|p| p.manifest.widgets.iter().any(|w| w.id == id))
}

/// Test hook: resolve one widget id exactly as the tree builder does.
#[cfg(test)]
pub(crate) fn widget_for_test(
    id: &str,
    settings: Option<&serde_json::Value>,
    plugins: &[Plugin],
) -> Box<dyn crate::Widget> {
    widget_for(id, settings, plugins)
}

fn widget_for(id: &str, settings: Option<&serde_json::Value>, plugins: &[Plugin]) -> Box<dyn crate::Widget> {
    if is_builtin(id) {
        return builtin(id, settings);
    }
    if let Some(p) = plugin_for(id, plugins) {
        let title = p
            .manifest
            .widgets
            .iter()
            .find(|w| w.id == id)
            .map(|w| w.title.clone())
            .unwrap_or_else(|| id.to_string());

        // The contract is versioned, so honour it. A plugin built against a
        // different api would otherwise be loaded anyway and fail as a confusing
        // parse error — or worse, appear to work while a field it expects is
        // quietly missing. An omitted `api` (0) is refused too: "unstated" is
        // not the same as "compatible".
        if p.manifest.api != houston_api::API_VERSION {
            let stated = if p.manifest.api == 0 { "no api".to_string() } else { format!("api {}", p.manifest.api) };
            return Box::new(BlockedWidget {
                id: id.to_string(),
                settings: settings.cloned(),
                headline: format!("{} targets {stated}", p.manifest.name),
                detail: vec![
                    format!("this Houston implements api {}", houston_api::API_VERSION),
                    "loading it anyway would fail in confusing ways,".into(),
                    "so the pane stays inert.".into(),
                ],
                action: Some("update the plugin, or its manifest's \"api\"".into()),
            });
        }

        match &p.manifest.runtime {
            // The exec runtime was removed: an arbitrary command carries the
            // user's full privileges and can read the OAuth tokens Houston
            // manages, and no manifest field can change that. A manifest asking
            // for it gets an explanation — silently skipping it would leave the
            // author wondering where their pane went.
            Runtime::Exec { .. } => {
                return Box::new(BlockedWidget {
                    id: id.to_string(),
                    settings: settings.cloned(),
                    headline: format!("{} uses the removed `exec` runtime", p.manifest.name),
                    detail: vec![
                        "a script runs with your full privileges, so it could read".into(),
                        "your Claude credentials. Houston only runs wasm plugins,".into(),
                        "which have no file or network access at all.".into(),
                    ],
                    action: Some("port it to wasm, or make it a Claude Code plugin".into()),
                });
            }
            Runtime::Wasm { file } => {
                // Isolated in its own process, re-invoking this binary as
                // `houston wasm-host <module>`: a guest that loops forever gets
                // KILLED instead of pegging a core for the rest of the session
                // (wasm fuel/epoch interruption aborts the process on this
                // toolchain, so it is not an option — see houston-plugins).
                let host = std::env::current_exe().map(|exe| {
                    houston_plugins::ProcHost::new(
                        exe,
                        p.dir.join(file),
                        houston_plugins::DEFAULT_TIMEOUT,
                    )
                });
                // The pane's own settings, at last: the field has always been in
                // the RenderRequest and was always null because nothing filled
                // it. A plugin reads its config from here.
                return Box::new(PluginWidget::new_wasm(
                    id.to_string(),
                    title,
                    host.map_err(anyhow::Error::from),
                    settings.cloned().unwrap_or(serde_json::Value::Null),
                ));
            }
        }
    }
    Box::new(MissingWidget { id: id.to_string(), settings: settings.cloned() })
}

fn size_from(s: SizeSpec) -> Size {
    match s {
        SizeSpec::Fixed(n) => Size::Fixed(n),
        SizeSpec::Percent(n) => Size::Percent(n),
        SizeSpec::Fill(n) => Size::Fill(n.max(1)),
    }
}

fn size_to(s: Size) -> SizeSpec {
    match s {
        Size::Fixed(n) => SizeSpec::Fixed(n),
        Size::Percent(n) => SizeSpec::Percent(n),
        Size::Fill(n) => SizeSpec::Fill(n),
    }
}

fn dir_from(d: Dir) -> Direction {
    match d {
        Dir::Row => Direction::Horizontal,
        Dir::Col => Direction::Vertical,
    }
}

/// Build the live widget tree from a layout node, resolving plugin ids.
pub fn build_tree(node: &LayoutNode, plugins: &[Plugin]) -> Node {
    match node {
        LayoutNode::Pane { widget, settings } => Node::Pane(widget_for(widget, settings.as_ref(), plugins)),
        LayoutNode::Split { dir, children } => Node::Split {
            dir: dir_from(*dir),
            children: children
                .iter()
                .map(|c| (size_from(c.size), build_tree(&c.node, plugins)))
                .collect(),
        },
    }
}

/// Serialize the live tree back to a layout node (structure + sizes + ids).
///
/// The pane's `settings` come back from the WIDGET, not from the layout we were
/// built from: this runs on every persist — including the one a border drag
/// triggers — so a widget that did not hand its settings back would have them
/// erased the first time the user resized anything.
pub fn to_layout(node: &Node) -> LayoutNode {
    match node {
        Node::Pane(w) => LayoutNode::Pane { widget: w.id().to_string(), settings: w.settings() },
        Node::Split { dir, children } => LayoutNode::Split {
            dir: match dir {
                Direction::Horizontal => Dir::Row,
                Direction::Vertical => Dir::Col,
            },
            children: children
                .iter()
                .map(|(sz, n)| LayoutChild { size: size_to(*sz), node: to_layout(n) })
                .collect(),
        },
    }
}
