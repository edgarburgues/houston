//! `probe` — a pane that shows the state of something else: a server, a queue,
//! whatever answers a question in a few lines of text.
//!
//! Configured per pane, so nothing about a particular host lives in this code:
//!
//! ```json
//! { "type": "pane", "widget": "probe", "settings": {
//!     "title": "homelab",
//!     "run": ["ssh", "homelab", "uptime; free -m | head -2"],
//!     "every_secs": 60 } }
//!
//! { "type": "pane", "widget": "probe", "settings": {
//!     "title": "homelab",
//!     "read": "//192.0.2.20/status/server.txt",
//!     "stale_secs": 180 } }
//! ```
//!
//! ## Why running a command here is allowed, when plugins may not
//!
//! Decision 4 removed the `exec` plugin runtime because an arbitrary command
//! carries the user's full privileges and can read the OAuth tokens Houston
//! manages — and no manifest field changes that. **The difference is origin.** A
//! plugin manifest is third-party content that arrives by installing something;
//! `config-v2.json` is the user's own file, in the same directory as
//! `.credentials.json`. Anyone who can write it can already read the tokens, so
//! this adds no capability to an attacker. The same reasoning already governs the
//! status line's `command` and Claude Code's own hooks.
//!
//! What must stay true, and is tested: **a plugin cannot get here.** Manifests
//! contribute no layout nodes, and a WASM guest has no filesystem, so it cannot
//! write a `run` into the config either.
//!
//! ## Why `run` is an argv and never a shell string
//!
//! No quoting rules to get wrong, no accidental interpolation of a path with a
//! space in it. Someone who wants a pipeline writes `["pwsh","-c","…"]` and says
//! so.

use crate::world::World;
use crate::Widget;
use houston_core::segments;
use ratatui::{
    layout::Rect,
    style::Style,
    text::Line,
    widgets::Paragraph,
    Frame,
};
use std::sync::mpsc;
use std::time::{Duration, Instant};

/// How long a run may take before it is abandoned. An `ssh` to a sleeping host
/// can hang for a long time, and a pane that never updates again is worse than
/// one that says it timed out.
const RUN_TIMEOUT: Duration = Duration::from_secs(20);
/// Default cadence when the pane does not say.
const DEFAULT_EVERY: Duration = Duration::from_secs(60);
/// Lines kept from the output. A pane is a few rows; a command that prints a
/// thousand lines is a misconfiguration, not something to hold in memory.
const MAX_LINES: usize = 40;
/// Characters kept per line, sanitized.
const MAX_COLS: usize = 200;

/// Where a probe gets its text.
#[derive(Debug, Clone, PartialEq, Eq)]
enum Source {
    /// Run this argv and take its stdout.
    Run(Vec<String>),
    /// Read this file, which something else writes.
    Read(String),
    /// The pane said nothing usable — render an explanation, not an empty box.
    Unset(&'static str),
}

pub struct ProbeWidget {
    /// The pane's settings verbatim, so persistence round-trips them untouched
    /// (`Widget::settings`). Parsed fields below are derived from it.
    raw: serde_json::Value,
    title: String,
    source: Source,
    every: Duration,
    /// After this, the reading is called old instead of shown as current.
    stale: Option<Duration>,
    /// Last text and when it was taken.
    text: Vec<String>,
    at: Option<Instant>,
    /// True when the last attempt failed; the text then holds the error.
    failed: bool,
    /// In-flight work. Present means a refresh is running.
    rx: Option<mpsc::Receiver<Result<Vec<String>, String>>>,
    /// Set by `r` to force a refresh on the next pass.
    wanted: bool,
}

impl Default for ProbeWidget {
    fn default() -> Self {
        ProbeWidget {
            raw: serde_json::Value::Null,
            title: "probe".into(),
            source: Source::Unset("this pane has no settings"),
            every: DEFAULT_EVERY,
            stale: None,
            text: Vec::new(),
            at: None,
            failed: false,
            rx: None,
            wanted: false,
        }
    }
}

impl ProbeWidget {
    /// Parse the pane's settings. Anything unusable becomes a visible
    /// explanation rather than a silent empty pane: a misconfigured probe is the
    /// most likely probe, and it must say what is wrong with it.
    fn parse(&mut self, v: &serde_json::Value) {
        self.raw = v.clone();
        self.title = v.get("title").and_then(|t| t.as_str()).unwrap_or("probe").to_string();
        self.every = v
            .get("every_secs")
            .and_then(|n| n.as_u64())
            .map(Duration::from_secs)
            .unwrap_or(DEFAULT_EVERY)
            // A zero interval would mean "spawn constantly".
            .max(Duration::from_secs(1));
        self.stale = v.get("stale_secs").and_then(|n| n.as_u64()).map(Duration::from_secs);

        let run = v.get("run").and_then(|r| r.as_array()).map(|a| {
            a.iter().filter_map(|x| x.as_str()).map(|s| s.to_string()).collect::<Vec<String>>()
        });
        let read = v.get("read").and_then(|r| r.as_str()).map(|s| s.to_string());
        self.source = match (run, read) {
            (Some(argv), None) if !argv.is_empty() => Source::Run(argv),
            (None, Some(path)) if !path.trim().is_empty() => Source::Read(path),
            // Both is not a preference to guess at: one of them is what the user
            // meant and picking wrong shows the wrong data as if it were right.
            (Some(_), Some(_)) => Source::Unset("this pane sets both `run` and `read` — keep one"),
            (Some(_), None) => Source::Unset("`run` is empty"),
            _ => Source::Unset("this pane needs `run` (an argv) or `read` (a path)"),
        };
    }

    fn due(&self) -> bool {
        match self.at {
            None => true,
            Some(t) => t.elapsed() >= self.every,
        }
    }

    /// Start a refresh off the main thread. The render never waits: an `ssh` costs
    /// about a second, and the whole point of the container engine is that a slow
    /// pane cannot freeze the UI.
    fn start(&mut self) {
        let (tx, rx) = mpsc::channel();
        let source = self.source.clone();
        std::thread::spawn(move || {
            let _ = tx.send(fetch(&source));
        });
        self.rx = Some(rx);
    }

    fn collect(&mut self) {
        let Some(rx) = self.rx.as_ref() else { return };
        match rx.try_recv() {
            Ok(Ok(lines)) => {
                self.text = lines;
                self.failed = false;
                self.at = Some(Instant::now());
                self.rx = None;
            }
            Ok(Err(e)) => {
                // Show the failure IN the pane. A probe that silently keeps its
                // last good value looks like a healthy server.
                self.text = wrap_error(&e);
                self.failed = true;
                self.at = Some(Instant::now());
                self.rx = None;
            }
            Err(mpsc::TryRecvError::Empty) => {}
            Err(mpsc::TryRecvError::Disconnected) => {
                self.text = vec!["the probe thread died".into()];
                self.failed = true;
                self.at = Some(Instant::now());
                self.rx = None;
            }
        }
    }

    /// "12s ago" / "4m ago", or the reason there is nothing yet.
    fn age_label(&self) -> String {
        match self.at {
            None if self.rx.is_some() => "probing…".into(),
            None => "no reading".into(),
            Some(t) => {
                let s = t.elapsed().as_secs();
                let ago = if s < 60 { format!("{s}s ago") } else { format!("{}m ago", s / 60) };
                match self.stale {
                    Some(limit) if t.elapsed() > limit => format!("{ago} · stale"),
                    _ => ago,
                }
            }
        }
    }

    fn is_stale(&self) -> bool {
        matches!((self.at, self.stale), (Some(t), Some(limit)) if t.elapsed() > limit)
    }
}

/// Do the work. Pure with respect to the widget, so it can run on a thread and be
/// tested without one.
fn fetch(source: &Source) -> Result<Vec<String>, String> {
    match source {
        Source::Unset(why) => Err((*why).to_string()),
        Source::Read(path) => {
            let body = std::fs::read_to_string(path).map_err(|e| format!("{path}: {e}"))?;
            Ok(clean(&body))
        }
        Source::Run(argv) => {
            let (exe, args) = argv.split_first().ok_or_else(|| "empty argv".to_string())?;
            let mut cmd = std::process::Command::new(exe);
            cmd.args(args)
                .stdin(std::process::Stdio::null())
                .stdout(std::process::Stdio::piped())
                .stderr(std::process::Stdio::piped());
            #[cfg(windows)]
            {
                // The TUI owns the terminal; a child console would flash over it.
                use std::os::windows::process::CommandExt;
                const CREATE_NO_WINDOW: u32 = 0x0800_0000;
                cmd.creation_flags(CREATE_NO_WINDOW);
            }
            let child = cmd.spawn().map_err(|e| format!("{exe}: {e}"))?;

            // Bound the WAIT, as `agents::list` does: killing the child would need
            // a platform handle this crate deliberately does not depend on, and a
            // straggler costs nothing — it holds no lock and owns no terminal.
            let (tx, rx) = mpsc::channel();
            std::thread::spawn(move || {
                let _ = tx.send(child.wait_with_output());
            });
            let out = match rx.recv_timeout(RUN_TIMEOUT) {
                Ok(Ok(out)) => out,
                Ok(Err(e)) => return Err(format!("{exe}: {e}")),
                Err(_) => return Err(format!("{exe}: sin respuesta en {}s", RUN_TIMEOUT.as_secs())),
            };
            if !out.status.success() {
                let err = String::from_utf8_lossy(&out.stderr);
                let first = clean(&err).into_iter().next().unwrap_or_else(|| "sin salida de error".into());
                return Err(format!("exit {}: {first}", out.status.code().unwrap_or(-1)));
            }
            Ok(clean(&String::from_utf8_lossy(&out.stdout)))
        }
    }
}

/// Sanitize and bound foreign text before it reaches the screen. Reuses the
/// segments sanitizer, which already handles ANSI, OSC and control characters —
/// this is somebody else's output going onto the user's terminal.
fn clean(raw: &str) -> Vec<String> {
    raw.lines()
        .map(|l| segments::sanitize(l, MAX_COLS))
        .filter(|l| !l.is_empty())
        .take(MAX_LINES)
        .collect()
}

fn wrap_error(e: &str) -> Vec<String> {
    clean(e).into_iter().take(4).collect()
}

impl Widget for ProbeWidget {
    fn id(&self) -> &str {
        "probe"
    }

    fn title(&self, _w: &World) -> String {
        format!("{} · {}", self.title, self.age_label())
    }

    fn render(&self, area: Rect, frame: &mut Frame, world: &World, _focused: bool) {
        let p = &world.palette;
        let mut lines: Vec<Line> = Vec::new();
        if self.text.is_empty() {
            let hint = match (&self.source, self.rx.is_some()) {
                (Source::Unset(why), _) => (*why).to_string(),
                (_, true) => "probing…".into(),
                _ => "no reading yet".into(),
            };
            lines.push(Line::styled(hint, Style::new().fg(p.grey)));
        }
        for l in &self.text {
            // A failure is the accent colour, not red-on-nothing: it is
            // information, not an alarm. A STALE reading goes grey, so it never
            // passes for current.
            let style = if self.failed {
                Style::new().fg(p.accent)
            } else if self.is_stale() {
                Style::new().fg(p.grey)
            } else {
                Style::new().fg(p.fg)
            };
            lines.push(Line::styled(l.clone(), style));
        }
        frame.render_widget(Paragraph::new(lines), area);
    }

    fn on_key(&mut self, key: char, _w: &mut World) -> bool {
        if key == 'r' {
            self.wanted = true;
            return true;
        }
        false
    }

    fn on_click(&mut self, _r: u16, _c: u16, _w: &mut World) {}
    fn on_scroll(&mut self, _up: bool, _w: &mut World) {}

    fn post_render(&mut self, _area: Rect, _world: &World, _focused: bool) {
        self.collect();
        if self.rx.is_none() && (self.wanted || (self.due() && !matches!(self.source, Source::Unset(_)))) {
            self.wanted = false;
            self.start();
        }
    }

    fn commands(&self) -> Vec<crate::command::Command> {
        vec![crate::command::Command::widget("r", "refrescar ahora", "probe")]
    }

    fn configure(&mut self, settings: &serde_json::Value) {
        self.parse(settings);
    }

    /// Verbatim, so persisting the layout cannot rewrite or lose the user's
    /// configuration — including keys a future Houston will understand.
    fn settings(&self) -> Option<serde_json::Value> {
        (!self.raw.is_null()).then(|| self.raw.clone())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn configured(v: serde_json::Value) -> ProbeWidget {
        let mut w = ProbeWidget::default();
        w.configure(&v);
        w
    }

    #[test]
    fn a_misconfigured_pane_explains_itself_instead_of_sitting_empty() {
        // Nothing at all.
        let w = configured(serde_json::json!({}));
        assert!(matches!(w.source, Source::Unset(_)));
        // BOTH sources: guessing which one the user meant would show the wrong
        // data as if it were right.
        let w = configured(serde_json::json!({ "run": ["x"], "read": "y" }));
        match &w.source {
            Source::Unset(why) => assert!(why.contains("both"), "{why}"),
            other => panic!("expected an explanation, got {other:?}"),
        }
        // An empty argv is not a command.
        assert!(matches!(configured(serde_json::json!({ "run": [] })).source, Source::Unset(_)));
        // And an unset pane never starts a thread.
        let mut w = configured(serde_json::json!({}));
        w.post_render(Rect::new(0, 0, 10, 3), &crate::world::tests::test_world(), false);
        assert!(w.rx.is_none(), "nothing to run, so nothing was spawned");
    }

    #[test]
    fn settings_round_trip_verbatim_including_keys_we_do_not_model() {
        let v = serde_json::json!({
            "title": "homelab",
            "run": ["ssh", "homelab", "uptime"],
            "every_secs": 30,
            "something_a_later_houston_adds": true
        });
        let w = configured(v.clone());
        assert_eq!(w.settings().as_ref(), Some(&v), "kept byte-for-byte, unknown keys included");
        assert_eq!(w.title, "homelab");
        assert_eq!(w.every, Duration::from_secs(30));
        // A pane with no settings writes none.
        assert!(ProbeWidget::default().settings().is_none());
    }

    #[test]
    fn a_zero_interval_cannot_mean_spawn_constantly() {
        let w = configured(serde_json::json!({ "run": ["x"], "every_secs": 0 }));
        assert_eq!(w.every, Duration::from_secs(1));
    }

    #[test]
    fn read_reports_the_path_when_it_is_missing_and_sanitizes_when_it_is_not() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("status.txt");

        let missing = fetch(&Source::Read(p.to_string_lossy().into_owned()));
        let err = missing.expect_err("a missing file is an error, not empty output");
        assert!(err.contains("status.txt"), "the message names the path: {err}");

        // Escape sequences in a file somebody else writes must not reach the
        // screen — same sanitizer as the status-line segments.
        std::fs::write(&p, "up 27 days\n\u{1b}[31mload 0.31\u{1b}[0m\n\n").unwrap();
        let lines = fetch(&Source::Read(p.to_string_lossy().into_owned())).unwrap();
        assert_eq!(lines, vec!["up 27 days".to_string(), "load 0.31".to_string()], "blank lines dropped, ANSI stripped");
    }

    #[test]
    fn a_failing_command_shows_the_failure() {
        // A command that cannot exist at all.
        let err = fetch(&Source::Run(vec!["houston-no-such-binary-xyz".into()])).expect_err("must fail");
        assert!(err.contains("houston-no-such-binary-xyz"), "{err}");

        // And one that exists but exits non-zero: the exit code and its stderr.
        #[cfg(windows)]
        let argv = vec!["cmd".to_string(), "/c".into(), "echo boom 1>&2 & exit 3".into()];
        #[cfg(not(windows))]
        let argv = vec!["sh".to_string(), "-c".into(), "echo boom >&2; exit 3".into()];
        let err = fetch(&Source::Run(argv)).expect_err("non-zero exit is a failure");
        assert!(err.contains("exit 3"), "{err}");
        assert!(err.contains("boom"), "the stderr is shown, not swallowed: {err}");
    }

    #[test]
    fn a_successful_command_is_captured_and_bounded() {
        #[cfg(windows)]
        let argv = vec!["cmd".to_string(), "/c".into(), "echo hola & echo mundo".into()];
        #[cfg(not(windows))]
        let argv = vec!["sh".to_string(), "-c".into(), "echo hola; echo mundo".into()];
        let lines = fetch(&Source::Run(argv)).unwrap();
        assert_eq!(lines.len(), 2);
        assert!(lines[0].contains("hola") && lines[1].contains("mundo"));

        // Output is capped rather than held whole: a command printing forever is
        // a misconfiguration, not something to keep in memory.
        #[cfg(windows)]
        let many = vec!["cmd".to_string(), "/c".into(), "for /l %i in (1,1,200) do @echo line%i".into()];
        #[cfg(not(windows))]
        let many = vec!["sh".to_string(), "-c".into(), "seq 200".into()];
        assert_eq!(fetch(&Source::Run(many)).unwrap().len(), MAX_LINES);
    }

    /// Not an assertion — an end-to-end look at a probe against a real host,
    /// because "does the pane show the server" is not something a unit test can
    /// answer. Give it a command on the command line:
    /// `cargo test -p houston-tui shot_probe -- --ignored --nocapture`
    #[test]
    #[ignore = "hits a real host; prints the pane for eyeballing"]
    fn shot_probe() {
        let mut w = configured(serde_json::json!({
            "title": "homelab",
            "run": ["ssh", "homelab", "uptime; free -h | head -2"]
        }));
        let world = crate::world::tests::test_world();
        let area = Rect::new(0, 0, 60, 6);
        for _ in 0..400 {
            w.post_render(area, &world, false);
            if w.at.is_some() {
                break;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        println!("title:  {}", w.title(&world));
        println!("failed: {}", w.failed);
        for l in &w.text {
            println!("  {l}");
        }
    }

    #[test]
    fn the_age_label_says_when_there_is_nothing_and_when_it_is_old() {
        let mut w = configured(serde_json::json!({ "read": "x", "stale_secs": 0 }));
        assert_eq!(w.age_label(), "no reading");
        assert!(!w.is_stale(), "no reading is not a stale reading");

        w.at = Some(Instant::now() - Duration::from_secs(5));
        assert!(w.is_stale(), "past stale_secs it must admit it");
        assert!(w.age_label().contains("stale"), "{}", w.age_label());

        let mut fresh = configured(serde_json::json!({ "read": "x" }));
        fresh.at = Some(Instant::now());
        assert!(!fresh.is_stale(), "no stale_secs means never called old");
        assert!(fresh.age_label().ends_with("ago"), "{}", fresh.age_label());
    }
}
