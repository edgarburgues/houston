//! Plugin discovery. A plugin is a directory under `<store>/houston2/plugins/`
//! holding a `plugin.json` manifest that declares the widgets it provides, its
//! runtime (a compiled `wasm` file, or an `exec` command for scripts), and the
//! capabilities it needs. Discovery only READS manifests — nothing runs here.
//! Instantiation and sandboxing live in the host (houston-tui / the wasm host).

use crate::paths::store_dir;
use serde::{Deserialize, Serialize};
use std::fs;
use std::path::{Path, PathBuf};

/// How a plugin's components are executed.
///
/// There is exactly ONE executable runtime, and that is the point. A `wasm`
/// module gets no WASI: no files, no network, no syscalls, so it cannot reach
/// the OAuth tokens Houston manages — a property of the design rather than a
/// policy someone can misconfigure.
///
/// An `exec` runtime used to exist for v1 compatibility, invoking a script per
/// call. It could not be made safe: an arbitrary command carries the user's full
/// privileges and can read every file the user can, credentials included. The
/// only real fix was OS-level sandboxing (Seatbelt / bubblewrap / AppContainer) —
/// a large per-platform project, and unavailable on native Windows. Deleting the
/// runtime is less code than sandboxing it, so it was removed rather than
/// contained. Scripts that genuinely need system access belong in Claude Code's
/// own plugin system, which Houston propagates across accounts (see `fleet`).
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "lowercase")]
pub enum Runtime {
    /// A compiled component: a `.wasm` file relative to the plugin dir.
    Wasm { file: String },
    /// REMOVED — retained in the parser only so a manifest written for the old
    /// runtime gets an explanation instead of being silently skipped as
    /// malformed. Houston never executes it; nothing in the codebase spawns a
    /// command from a manifest.
    Exec { command: Vec<String> },
}

/// One widget a plugin provides: the id a layout references, and its title.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct WidgetDecl {
    pub id: String,
    pub title: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default)]
pub struct PluginManifest {
    /// Targeted contract version (houston_api::API_VERSION).
    pub api: u32,
    pub name: String,
    pub version: String,
    pub description: String,
    pub widgets: Vec<WidgetDecl>,
    pub runtime: Runtime,
    /// Declared capabilities, INFORMATIONAL ONLY today.
    ///
    /// There is deliberately nothing to grant: a wasm plugin gets no WASI, so it
    /// has no files, no network and no syscalls by construction — a capability
    /// system would only matter once the host offers a way out, and the one that
    /// existed (an "open this URL" effect) was removed rather than gated, since a
    /// guest that cannot reach the network should not be able to ask the host to
    /// reach it instead. An `exec` plugin, conversely, cannot be constrained by a
    /// manifest at all: it runs an arbitrary command with the user's full
    /// privileges, which is why running one requires explicit approval (see
    /// `crate::trust`).
    ///
    /// So this field documents intent for a reader; it grants nothing. `doctor`
    /// says as much rather than implying enforcement that does not exist.
    pub capabilities: Vec<String>,
}

impl Default for PluginManifest {
    fn default() -> Self {
        PluginManifest {
            api: 0,
            name: String::new(),
            version: String::new(),
            description: String::new(),
            widgets: Vec::new(),
            runtime: Runtime::Exec { command: Vec::new() },
            capabilities: Vec::new(),
        }
    }
}

/// A discovered plugin: its manifest and the directory it lives in (the base
/// for resolving the wasm file / the exec working dir).
#[derive(Debug, Clone)]
pub struct Plugin {
    pub dir: PathBuf,
    pub manifest: PluginManifest,
}

impl Plugin {
    /// Whether this manifest asks for the removed `exec` runtime. Used only to
    /// explain why it will not load — never to run anything.
    pub fn wants_removed_exec_runtime(&self) -> bool {
        matches!(self.manifest.runtime, Runtime::Exec { .. })
    }
}

pub fn plugins_dir() -> PathBuf {
    store_dir().join("houston2").join("plugins")
}

/// Discover every plugin under the plugins dir. Malformed manifests are
/// skipped (a broken plugin never blocks startup). Sorted by name for a
/// reproducible id-claim order.
pub fn discover() -> Vec<Plugin> {
    discover_in(&plugins_dir())
}

pub fn discover_in(dir: &Path) -> Vec<Plugin> {
    let mut out = Vec::new();
    let Ok(entries) = fs::read_dir(dir) else {
        return out;
    };
    for e in entries.flatten() {
        let d = e.path();
        if !d.is_dir() {
            continue;
        }
        let mf = d.join("plugin.json");
        if let Ok(b) = fs::read(&mf) {
            let b = b.strip_prefix(&[0xEF, 0xBB, 0xBF]).unwrap_or(&b);
            if let Ok(m) = serde_json::from_slice::<PluginManifest>(b) {
                if !m.name.is_empty() {
                    out.push(Plugin { dir: d, manifest: m });
                }
            }
        }
    }
    out.sort_by(|a, b| a.manifest.name.cmp(&b.manifest.name));
    out
}

/// Index widgets across all plugins by widget id → (plugin index, decl).
/// First claimant wins on a duplicate id (plugins are name-sorted), matching
/// v1's action-key policy.
pub fn widget_index(plugins: &[Plugin]) -> std::collections::HashMap<String, usize> {
    let mut map = std::collections::HashMap::new();
    for (pi, p) in plugins.iter().enumerate() {
        for w in &p.manifest.widgets {
            map.entry(w.id.clone()).or_insert(pi);
        }
    }
    map
}

#[cfg(test)]
mod tests {
    use super::*;

    fn write_plugin(root: &Path, name: &str, json: &str) {
        let d = root.join(name);
        fs::create_dir_all(&d).unwrap();
        fs::write(d.join("plugin.json"), json).unwrap();
    }

    #[test]
    fn discovers_valid_skips_broken_sorts_by_name() {
        let tmp = tempfile::tempdir().unwrap();
        write_plugin(
            tmp.path(),
            "zeta",
            r#"{"api":1,"name":"zeta","version":"1.0","widgets":[{"id":"z","title":"Z"}],"runtime":{"kind":"exec","command":["pwsh","z.ps1"]}}"#,
        );
        write_plugin(
            tmp.path(),
            "alpha",
            r#"{"api":1,"name":"alpha","widgets":[{"id":"quota","title":"Quota"}],"runtime":{"kind":"wasm","file":"alpha.wasm"},"capabilities":["net"]}"#,
        );
        write_plugin(tmp.path(), "broken", "{ not json");

        let plugins = discover_in(tmp.path());
        assert_eq!(plugins.len(), 2, "broken manifest skipped");
        assert_eq!(plugins[0].manifest.name, "alpha"); // sorted
        assert!(matches!(plugins[0].manifest.runtime, Runtime::Wasm { .. }));
        assert_eq!(plugins[0].manifest.capabilities, vec!["net"]);

        // A manifest asking for the REMOVED exec runtime still parses, on
        // purpose: it has to be discoverable to be explained. Silently dropping
        // it would leave the author hunting for a pane that never appears.
        assert!(plugins[1].wants_removed_exec_runtime());
        assert!(!plugins[0].wants_removed_exec_runtime());

        let idx = widget_index(&plugins);
        assert_eq!(idx.get("quota"), Some(&0));
        assert_eq!(idx.get("z"), Some(&1));
    }

    #[test]
    fn missing_dir_is_empty_not_error() {
        let tmp = tempfile::tempdir().unwrap();
        assert!(discover_in(&tmp.path().join("nope")).is_empty());
    }
}
