//! houston-api — the versioned component contract Houston 2.0 plugins build
//! against. Language-agnostic (serde/JSON), so the SAME contract serves both
//! bindings: compiled WASM plugins and exec-JSON scripts. Built-in widgets use
//! the richer in-process `Widget` trait directly; this is the shape everything
//! crosses the plugin boundary as.
//!
//! Stability: the host announces `API_VERSION`; a plugin manifest states the
//! `api` it targets. The host refuses a plugin whose api it doesn't implement,
//! with a clear message — never a silent misbehavior.

use serde::{Deserialize, Serialize};

/// The contract version. Bump on any breaking change to the types below.
pub const API_VERSION: u32 = 1;

/// A read-only snapshot of the selected mission, handed to a widget so it can
/// render context (git badges, jira keys, quota for the active account…). A
/// subset of the full model — the plugin boundary never carries the whole
/// transcript.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct MissionInfo {
    pub key: String,
    pub id: String,
    pub project: String,
    pub title: String,
    pub cwd: String,
    pub git_branch: String,
    pub version: String,
    pub tags: Vec<String>,
}

/// What the host asks a widget to produce, for a given pane geometry.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct RenderRequest {
    pub api: u32,
    /// Inner width/height in cells (inside the pane border).
    pub width: u16,
    pub height: u16,
    pub focused: bool,
    /// The selected mission, if any.
    pub selection: Option<MissionInfo>,
    /// The plugin's settings from config (opaque; the plugin documents it).
    pub settings: serde_json::Value,
}

/// A single input event delivered to a focused/hovered widget.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "lowercase")]
pub enum Event {
    Key { ch: char },
    Click { row: u16, col: u16 },
    Scroll { up: bool },
}

/// A call across the boundary: render, or handle an event then render. One
/// entrypoint serves both — the plugin switches on the variant.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "call", rename_all = "lowercase")]
pub enum Call {
    Render { req: RenderRequest },
    Event { event: Event, req: RenderRequest },
}

/// A styled color: a name ("white", "cyan"), an ANSI-256 index as a string
/// ("240"), or None to inherit the pane's default. The host maps it through
/// the theme, so plugins stay theme-friendly.
pub type ColorSpec = Option<String>;

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct SpanStyle {
    pub fg: ColorSpec,
    pub bold: bool,
    pub dim: bool,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct Span {
    pub text: String,
    #[serde(flatten)]
    pub style: SpanStyle,
}

impl Span {
    pub fn plain(text: impl Into<String>) -> Self {
        Span { text: text.into(), style: SpanStyle::default() }
    }
}

/// One rendered line: a sequence of styled spans.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct Line {
    pub spans: Vec<Span>,
}

impl Line {
    pub fn plain(text: impl Into<String>) -> Self {
        Line { spans: vec![Span::plain(text)] }
    }
}

/// A side effect a widget can request from the host.
///
/// This is the ONLY channel out of a plugin, so it is deliberately tiny. A WASM
/// plugin has no WASI: no files, no network, no syscalls. Anything added here is
/// therefore a hole punched in that boundary, and has to earn it.
///
/// An `OpenUrl` effect used to live here. It was never implemented, and it is a
/// textbook exfiltration channel: a guest with no network could still encode
/// whatever it can see (mission titles, paths, branches) into a URL and have the
/// HOST fetch it. It was removed rather than wired, because "the plugin cannot
/// reach the network" should be a property of the design, not a promise that one
/// convenience feature quietly voids.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "effect", rename_all = "lowercase")]
pub enum Effect {
    /// A one-line message for the footer. The host prefixes it with the
    /// plugin's id, so a plugin cannot impersonate Houston's own messages.
    Status { text: String },
}

/// A widget's reply: what to draw, plus any effects. An optional title
/// overrides the pane title (e.g. a live counter).
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Response {
    pub title: Option<String>,
    pub lines: Vec<Line>,
    pub effects: Vec<Effect>,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn call_and_response_roundtrip_json() {
        let call = Call::Event {
            event: Event::Key { ch: 'r' },
            req: RenderRequest { api: API_VERSION, width: 40, height: 10, focused: true, ..Default::default() },
        };
        let s = serde_json::to_string(&call).unwrap();
        assert!(s.contains("\"call\":\"event\""));
        assert!(s.contains("\"kind\":\"key\""));
        let back: Call = serde_json::from_str(&s).unwrap();
        matches!(back, Call::Event { .. });

        let resp = Response {
            title: Some("Quota (2)".into()),
            lines: vec![Line {
                spans: vec![Span { text: "5h ".into(), style: SpanStyle { fg: Some("cyan".into()), bold: true, dim: false } }],
            }],
            effects: vec![Effect::Status { text: "refreshed".into() }],
        };
        let s = serde_json::to_string(&resp).unwrap();
        let back: Response = serde_json::from_str(&s).unwrap();
        assert_eq!(back.title.as_deref(), Some("Quota (2)"));
        assert_eq!(back.lines[0].spans[0].text, "5h ");
        assert!(back.lines[0].spans[0].style.bold);
    }

    #[test]
    fn helpers_build_plain_content() {
        let l = Line::plain("hello");
        assert_eq!(l.spans.len(), 1);
        assert_eq!(l.spans[0].text, "hello");
        assert!(!l.spans[0].style.bold);
    }
}
