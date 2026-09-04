//! Streaming .jsonl transcript parser. Read-only; files are parsed line by
//! line so 30 MB transcripts never land in memory whole. `scan_all` (roots.rs)
//! fans out across every discovered root and dedupes by logical key.

mod cache;
mod roots;

pub use cache::Cache;
pub use roots::{project_roots, scan_all};

use crate::model::Mission;
use crate::pathenc;
use chrono::{DateTime, Utc};
use serde::Deserialize;
use serde_json::value::RawValue;
use std::fs::File;
use std::io::{BufRead, BufReader};
use std::path::Path;

/// The subset of a transcript line Houston cares about.
#[derive(Deserialize, Default)]
#[serde(default)]
struct RawEntry<'a> {
    #[serde(rename = "type")]
    kind: String,
    #[serde(rename = "sessionId")]
    _session_id: String,
    cwd: String,
    #[serde(rename = "gitBranch")]
    git_branch: String,
    version: String,
    slug: String,
    // User-set session name (claude -n / /rename). The spelling has varied
    // across Claude Code versions; accept both. A top-level "name" is
    // deliberately NOT read — it's ambiguous with tool/other names.
    #[serde(rename = "sessionName")]
    session_name: String,
    #[serde(rename = "customName")]
    custom_name: String,
    timestamp: String,
    #[serde(rename = "isMeta")]
    is_meta: bool,
    #[serde(rename = "aiTitle")]
    ai_title: String,
    #[serde(rename = "lastPrompt")]
    last_prompt: String,
    #[serde(borrow)]
    message: Option<RawMessage<'a>>,
}

#[derive(Deserialize)]
struct RawMessage<'a> {
    #[serde(rename = "role")]
    _role: Option<String>,
    #[serde(borrow)]
    content: Option<&'a RawValue>,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct ContentPart {
    #[serde(rename = "type")]
    kind: String,
    text: String,
    /// For tool_use parts.
    name: String,
}

/// Parse one transcript into a Mission. Returns None when the file is
/// unreadable or carries no id.
pub fn parse_file(path: &Path) -> Option<Mission> {
    let f = File::open(path).ok()?;
    let meta = f.metadata().ok();

    let mut m = Mission {
        path: path.to_string_lossy().into_owned(),
        id: path.file_stem()?.to_string_lossy().into_owned(),
        project: path.parent()?.file_name()?.to_string_lossy().into_owned(),
        ..Default::default()
    };
    if let Some(fi) = &meta {
        m.size_bytes = fi.len();
        m.mtime_ns = mtime_ns(fi);
    }
    // A "subagents" dir under <dir>/<id>/ means this mission spawned subagents.
    if let Some(dir) = path.parent() {
        if dir.join(&m.id).join("subagents").is_dir() {
            m.has_subagents = true;
        }
    }

    let mut reader = BufReader::with_capacity(1 << 20, f);
    let mut line = String::new();
    loop {
        line.clear();
        match reader.read_line(&mut line) {
            Ok(0) => break,
            Ok(_) => {
                let s = line.trim();
                if !s.is_empty() {
                    ingest(&mut m, s);
                }
            }
            Err(_) => break,
        }
    }
    if m.title.is_empty() {
        m.title = first_line(&m.first_prompt).to_string();
    }
    if !m.name.is_empty() {
        // An explicit user-set name wins over aiTitle / first prompt.
        m.title = m.name.clone();
    }
    // m.cwd currently holds the creation cwd; resolve the dir that actually
    // encodes back to the project folder so `claude --resume` finds the session.
    m.cwd = pathenc::resolve_resume_dir(&m.cwd, &m.last_cwd, &m.project);
    Some(m)
}

pub(crate) fn mtime_ns(fi: &std::fs::Metadata) -> i64 {
    fi.modified()
        .ok()
        .and_then(|t| t.duration_since(std::time::UNIX_EPOCH).ok())
        .map(|d| d.as_nanos() as i64)
        .unwrap_or(0)
}

fn ingest(m: &mut Mission, line: &str) {
    let Ok(e) = serde_json::from_str::<RawEntry>(line) else {
        return;
    };
    // Capture common fields wherever they appear (they repeat on most lines).
    // Cwd: keep the FIRST seen as creation cwd and the latest as last_cwd.
    if !e.cwd.is_empty() {
        if m.cwd.is_empty() {
            m.cwd = e.cwd.clone();
        }
        m.last_cwd = e.cwd;
    }
    if !e.git_branch.is_empty() {
        m.git_branch = e.git_branch;
    }
    if !e.version.is_empty() {
        m.version = e.version;
    }
    if !e.slug.is_empty() {
        m.slug = e.slug;
    }
    if !e.session_name.is_empty() {
        m.name = e.session_name;
    } else if !e.custom_name.is_empty() {
        m.name = e.custom_name;
    }
    match e.kind.as_str() {
        "ai-title" => {
            if !e.ai_title.is_empty() {
                m.title = e.ai_title;
            }
        }
        "last-prompt" => {
            if !e.last_prompt.is_empty() {
                m.last_prompt = e.last_prompt;
            }
        }
        "user" => {
            let txt = message_text(e.message.as_ref());
            if !txt.is_empty() && !e.is_meta {
                m.user_msgs += 1;
                if m.first_prompt.is_empty() {
                    m.first_prompt = txt;
                }
            }
            stamp_time(m, &e.timestamp);
        }
        "assistant" => {
            m.assistant_msgs += 1;
            for name in tool_names(e.message.as_ref()) {
                *m.tools.entry(name).or_insert(0) += 1;
            }
            stamp_time(m, &e.timestamp);
        }
        _ => {}
    }
}

/// Pull human/assistant prose out of a message, ignoring tool_result /
/// tool_use / image payloads (huge and not useful to search).
fn message_text(msg: Option<&RawMessage>) -> String {
    let Some(raw) = msg.and_then(|m| m.content) else {
        return String::new();
    };
    let trimmed = raw.get().trim_start();
    if trimmed.starts_with('"') {
        return serde_json::from_str::<String>(raw.get()).unwrap_or_default();
    }
    let Ok(parts) = serde_json::from_str::<Vec<ContentPart>>(raw.get()) else {
        return String::new();
    };
    parts
        .into_iter()
        .filter(|p| p.kind == "text" && !p.text.trim().is_empty())
        .map(|p| p.text)
        .collect::<Vec<_>>()
        .join("\n")
}

fn tool_names(msg: Option<&RawMessage>) -> Vec<String> {
    let Some(raw) = msg.and_then(|m| m.content) else {
        return Vec::new();
    };
    if raw.get().trim_start().starts_with('"') {
        return Vec::new();
    }
    let Ok(parts) = serde_json::from_str::<Vec<ContentPart>>(raw.get()) else {
        return Vec::new();
    };
    parts
        .into_iter()
        .filter(|p| p.kind == "tool_use" && !p.name.is_empty())
        .map(|p| p.name)
        .collect()
}

fn stamp_time(m: &mut Mission, ts: &str) {
    if ts.is_empty() {
        return;
    }
    let Ok(t) = DateTime::parse_from_rfc3339(ts) else {
        return;
    };
    let t: DateTime<Utc> = t.with_timezone(&Utc);
    if m.first_time.is_none_or(|f| t < f) {
        m.first_time = Some(t);
    }
    if m.last_time.is_none_or(|l| t > l) {
        m.last_time = Some(t);
    }
}

fn first_line(s: &str) -> &str {
    let s = s.trim();
    s.split('\n').next().unwrap_or("").trim_end()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn write_transcript(dir: &Path, project: &str, id: &str, lines: &[&str]) -> std::path::PathBuf {
        let pdir = dir.join(project);
        std::fs::create_dir_all(&pdir).unwrap();
        let p = pdir.join(format!("{id}.jsonl"));
        let mut f = File::create(&p).unwrap();
        for l in lines {
            writeln!(f, "{l}").unwrap();
        }
        p
    }

    #[test]
    fn parses_a_minimal_transcript() {
        let tmp = tempfile::tempdir().unwrap();
        let p = write_transcript(
            tmp.path(),
            "C--work-proj",
            "sess1",
            &[
                r#"{"type":"user","cwd":"C:\\work\\proj","gitBranch":"main","version":"2.1.0","timestamp":"2026-07-01T10:00:00Z","message":{"role":"user","content":"hola houston"}}"#,
                r#"{"type":"assistant","timestamp":"2026-07-01T10:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"hola!"},{"type":"tool_use","name":"Bash"}]}}"#,
                r#"{"type":"ai-title","aiTitle":"Saludo inicial"}"#,
            ],
        );
        let m = parse_file(&p).unwrap();
        assert_eq!(m.id, "sess1");
        assert_eq!(m.project, "C--work-proj");
        assert_eq!(m.title, "Saludo inicial");
        assert_eq!(m.first_prompt, "hola houston");
        assert_eq!(m.user_msgs, 1);
        assert_eq!(m.assistant_msgs, 1);
        assert_eq!(m.tools.get("Bash"), Some(&1));
        assert_eq!(m.git_branch, "main");
        assert!(m.first_time.is_some());
    }

    #[test]
    fn meta_user_lines_do_not_count_or_set_first_prompt() {
        let tmp = tempfile::tempdir().unwrap();
        let p = write_transcript(
            tmp.path(),
            "C--x",
            "s",
            &[
                r#"{"type":"user","isMeta":true,"message":{"role":"user","content":"<system>meta</system>"}}"#,
                r#"{"type":"user","message":{"role":"user","content":"the real one"}}"#,
            ],
        );
        let m = parse_file(&p).unwrap();
        assert_eq!(m.user_msgs, 1);
        assert_eq!(m.first_prompt, "the real one");
        assert_eq!(m.title, "the real one");
    }

    #[test]
    fn explicit_session_name_beats_ai_title() {
        let tmp = tempfile::tempdir().unwrap();
        let p = write_transcript(
            tmp.path(),
            "C--x",
            "s",
            &[
                r#"{"type":"ai-title","aiTitle":"generated"}"#,
                r#"{"type":"user","sessionName":"my name","message":{"role":"user","content":"hi"}}"#,
            ],
        );
        let m = parse_file(&p).unwrap();
        assert_eq!(m.title, "my name");
    }

    #[test]
    fn garbage_lines_are_skipped() {
        let tmp = tempfile::tempdir().unwrap();
        let p = write_transcript(
            tmp.path(),
            "C--x",
            "s",
            &["not json at all", r#"{"type":"user","message":{"content":"ok"}}"#],
        );
        let m = parse_file(&p).unwrap();
        assert_eq!(m.user_msgs, 1);
    }
}
