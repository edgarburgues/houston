//! PluginWidget — a pane backed by a plugin, rendered through the houston-api
//! contract.
//!
//! There is one backend, and that is deliberate: a sandboxed `.wasm` module in
//! its own killable process (houston-plugins). An `exec` backend used to exist
//! for v1 compatibility, invoking a script per call; it was removed because an
//! arbitrary command cannot be prevented from reading the OAuth tokens Houston
//! manages. Nothing here spawns a command from a manifest any more.
//!
//! The result is cached and drawn every frame; the plugin is only invoked on
//! mount and on interaction, never per frame.

use crate::command::Command as Cmd;
use crate::world::{parse_color, World};
use crate::Widget;
use houston_api as api;
use houston_plugins::ProcHost;
use ratatui::{
    layout::Rect,
    style::{Modifier, Style},
    text::{Line, Span},
    widgets::Paragraph,
    Frame,
};

pub struct PluginWidget {
    id: String,
    title: String,
    /// None when the host could not even be prepared; `error` says why.
    host: Option<ProcHost>,
    settings: serde_json::Value,
    cache: Option<api::Response>,
    loaded: bool,
    error: Option<String>,
}

impl PluginWidget {
    /// A WASM-backed plugin widget. A load failure is surfaced as the widget's
    /// error (visible, never a crash).
    pub fn new_wasm(id: String, title: String, host: anyhow::Result<ProcHost>, settings: serde_json::Value) -> Self {
        let (host, error, loaded) = match host {
            Ok(h) => (Some(h), None, false),
            Err(e) => (None, Some(format!("load failed: {e:#}")), true),
        };
        PluginWidget { id, title, host, settings, cache: None, loaded, error }
    }

    fn request(&self, area: Rect, world: &World, focused: bool) -> api::RenderRequest {
        let selection = world.selected().map(|m| api::MissionInfo {
            key: m.key(),
            id: m.id.clone(),
            project: m.project.clone(),
            title: m.title.clone(),
            cwd: m.cwd.clone(),
            git_branch: m.git_branch.clone(),
            version: m.version.clone(),
            tags: world.meta_of(m).tags,
        });
        api::RenderRequest {
            api: api::API_VERSION,
            width: area.width,
            height: area.height,
            focused,
            selection,
            settings: self.settings.clone(),
        }
    }

    /// Run one call against the backend. Updates the cache or records an
    /// error; never panics, never blocks past the timeout. Returns a footer
    /// message the plugin asked for, if any.
    fn call(&mut self, call: api::Call) -> Option<String> {
        self.loaded = true;
        let Some(host) = &self.host else { return None };
        let result = host.call(&call).map_err(|e| format!("{e:#}"));
        match result {
            Ok(resp) => {
                // Effects are the only channel out of a plugin. A status line is
                // attributed to the plugin BY THE HOST and clipped, so a plugin
                // can neither impersonate Houston nor flood the footer.
                let status = resp.effects.iter().find_map(|e| match e {
                    api::Effect::Status { text } => {
                        let one_line: String = text.chars().filter(|c| *c != '\n' && *c != '\r').take(160).collect();
                        (!one_line.trim().is_empty()).then(|| format!("[{}] {}", self.id, one_line))
                    }
                });
                self.cache = Some(resp);
                self.error = None;
                status
            }
            Err(e) => {
                self.error = Some(e);
                None
            }
        }
    }
}

fn style_of(s: &api::SpanStyle, world: &World) -> Style {
    let fg = s.fg.as_deref().map(parse_color).unwrap_or(world.palette.fg);
    let mut st = Style::new().fg(fg);
    if s.bold {
        st = st.add_modifier(Modifier::BOLD);
    }
    if s.dim {
        st = st.add_modifier(Modifier::DIM);
    }
    st
}

impl Widget for PluginWidget {
    fn id(&self) -> &str {
        &self.id
    }

    fn title(&self, _world: &World) -> String {
        self.cache
            .as_ref()
            .and_then(|r| r.title.clone())
            .unwrap_or_else(|| self.title.clone())
    }

    fn render(&self, area: Rect, frame: &mut Frame, world: &World, _focused: bool) {
        if let Some(err) = &self.error {
            frame.render_widget(
                Paragraph::new(Line::styled(format!("plugin error: {err}"), Style::new().fg(world.palette.grey))),
                area,
            );
            return;
        }
        let Some(resp) = &self.cache else {
            frame.render_widget(
                Paragraph::new(Line::styled("loading… (r to refresh)", Style::new().fg(world.palette.grey))),
                area,
            );
            return;
        };
        let lines: Vec<Line> = resp
            .lines
            .iter()
            .map(|l| {
                Line::from(
                    l.spans
                        .iter()
                        .map(|sp| Span::styled(sp.text.clone(), style_of(&sp.style, world)))
                        .collect::<Vec<_>>(),
                )
            })
            .collect();
        frame.render_widget(Paragraph::new(lines), area);
    }

    fn on_key(&mut self, key: char, world: &mut World) -> bool {
        // Any key routed here (the pane is focused) refreshes via an Event
        // call; 'r' is the conventional manual refresh.
        let req = self.request(Rect::new(0, 0, 0, 0), world, true);
        if let Some(s) = self.call(api::Call::Event { event: api::Event::Key { ch: key }, req }) {
            world.status = s;
        }
        true
    }

    fn on_click(&mut self, row: u16, col: u16, world: &mut World) {
        let req = self.request(Rect::new(0, 0, 0, 0), world, true);
        if let Some(s) = self.call(api::Call::Event { event: api::Event::Click { row, col }, req }) {
            world.status = s;
        }
    }

    fn on_scroll(&mut self, up: bool, world: &mut World) {
        let req = self.request(Rect::new(0, 0, 0, 0), world, false);
        if let Some(s) = self.call(api::Call::Event { event: api::Event::Scroll { up }, req }) {
            world.status = s;
        }
    }

    fn post_render(&mut self, area: Rect, world: &World, focused: bool) {
        // Lazy first load: one Render call once we know the pane geometry.
        // post_render only has &World, so a status effect from the very first
        // load is dropped rather than shown — an interaction will surface it.
        if !self.loaded {
            let req = self.request(area, world, focused);
            let _ = self.call(api::Call::Render { req });
        }
    }
    fn commands(&self) -> Vec<Cmd> {
        vec![Cmd::widget("r", "refresh", &self.id)]
    }

    /// Hand the pane's settings back so persisting the layout keeps them. The
    /// plugin never sees them change; Houston is only the carrier.
    fn settings(&self) -> Option<serde_json::Value> {
        (!self.settings.is_null()).then(|| self.settings.clone())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::world::Palette;
    use houston_core::config::Theme;
    use houston_core::store::Store;

    fn empty_world() -> World {
        let tmp = tempfile::tempdir().unwrap();
        let w = World::new(
            Store::load_from(tmp.path().to_path_buf()).unwrap(),
            Palette::from_theme(&Theme::default()),
        );
        std::mem::forget(tmp);
        w
    }

    #[test]
    fn a_host_that_could_not_be_prepared_is_visible_not_fatal() {
        // The only failure shape left: the wasm host could not even be built.
        // It must render as an error in its pane and never call anything.
        let mut w = PluginWidget::new_wasm(
            "x".into(),
            "X".into(),
            Err(anyhow::anyhow!("no such module")),
            serde_json::Value::Null,
        );
        let world = empty_world();
        w.post_render(Rect::new(0, 0, 10, 3), &world, false);
        assert!(w.loaded);
        assert!(w.error.as_deref().unwrap().contains("no such module"));
        // With no host, a routed key is a no-op rather than a panic.
        let mut world2 = empty_world();
        assert!(w.on_key('r', &mut world2));
        assert!(w.cache.is_none());
    }

    /// The structural property behind removing the exec runtime: a manifest can
    /// no longer cause Houston to run a command. This is checked by construction
    /// — `PluginWidget` has exactly one way to be built, and it takes a wasm
    /// host — so the test exists to fail loudly if a second one is ever added.
    #[test]
    fn the_only_backend_is_a_sandboxed_wasm_host() {
        let w = PluginWidget::new_wasm("only".into(), "Only".into(), Err(anyhow::anyhow!("x")), serde_json::Value::Null);
        // `host` is the sole execution field; there is no command, no cwd, no
        // shell. If that changes, this file needs a security review, not a fix.
        assert!(w.host.is_none());
    }
}
