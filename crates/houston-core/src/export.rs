//! Renders a mission's transcript to a readable Markdown file.

use crate::model::Mission;
use std::io::{BufRead, BufWriter, Write};
use std::path::{Path, PathBuf};

/// One line of the .jsonl transcript, reduced to what the export needs.
#[derive(serde::Deserialize)]
struct RawEntry {
    #[serde(default, rename = "type")]
    kind: String,
    #[serde(default)]
    message: Option<Msg>,
    #[serde(default, rename = "isMeta")]
    is_meta: bool,
}

#[derive(serde::Deserialize)]
struct Msg {
    #[serde(default)]
    content: serde_json::Value,
}

/// Write the mission as Markdown to `out_path` and return the path.
pub fn mission(m: &Mission, out_path: &Path) -> std::io::Result<PathBuf> {
    let file = std::fs::File::open(&m.path)?;
    if let Some(dir) = out_path.parent() {
        ensure_export_dir(dir)?;
    }
    // Transcripts routinely contain secrets (keys pasted into prompts, tool
    // output), so the file is created for the owner only — never handed to
    // every local user via default perms.
    let out = create_private(out_path)?;
    let mut w = BufWriter::new(out);

    let title = if m.title.is_empty() { m.id.as_str() } else { m.title.as_str() };
    writeln!(w, "# {title}\n")?;
    writeln!(
        w,
        "- **Mission**: `{}`\n- **Project**: {}\n- **cwd**: `{}`\n- **Branch**: {}",
        m.id, m.project, m.cwd, m.git_branch
    )?;
    if let (Some(first), Some(last)) = (m.first_time, m.last_time) {
        writeln!(
            w,
            "- **Period**: {} → {}",
            first.with_timezone(&chrono::Local).format("%Y-%m-%d %H:%M"),
            last.with_timezone(&chrono::Local).format("%Y-%m-%d %H:%M")
        )?;
    }
    writeln!(w, "- **Messages**: {} · **Tool calls**: {}\n\n---\n", m.message_count(), m.tool_calls())?;

    // 1 MiB buffer: transcript lines can be very long (whole file contents).
    let reader = std::io::BufReader::with_capacity(1 << 20, file);
    for line in reader.lines() {
        let Ok(line) = line else { break }; // a malformed tail ends the export, never fails it
        let line = line.trim();
        if !line.is_empty() {
            write_turn(&mut w, line)?;
        }
    }
    w.flush()?;
    Ok(out_path.to_path_buf())
}

/// Create/truncate a file readable only by its owner.
///
/// On unix that is a 0600 mode. On Windows a new file INHERITS the directory's
/// ACL, which for a shared or reconfigured location can be wider than the
/// profile default — so the inherited entries are stripped and an explicit
/// owner-only ACL is applied with `icacls`. Best effort: if `icacls` is missing
/// the export still happens (refusing to export would be worse), but the caller
/// is told the file holds secrets either way.
fn create_private(path: &Path) -> std::io::Result<std::fs::File> {
    let mut opts = std::fs::OpenOptions::new();
    opts.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    let f = opts.open(path)?;
    #[cfg(windows)]
    restrict_to_owner_windows(path);
    Ok(f)
}

/// Replace a file's inherited ACL with a single grant to the current user.
#[cfg(windows)]
fn restrict_to_owner_windows(path: &Path) {
    use std::os::windows::process::CommandExt;
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    let Ok(user) = std::env::var("USERNAME") else { return };
    // /inheritance:r drops inherited entries; /grant:r replaces any grant for
    // this user with full control and leaves nobody else on the DACL.
    let _ = std::process::Command::new("icacls")
        .arg(path)
        .args(["/inheritance:r", "/grant:r", &format!("{user}:F"), "/q"])
        .creation_flags(CREATE_NO_WINDOW)
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status();
}

/// Create the exports directory owner-only, so a transcript is never briefly
/// world-readable via a permissive parent.
pub fn ensure_export_dir(dir: &Path) -> std::io::Result<()> {
    std::fs::create_dir_all(dir)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700));
    }
    #[cfg(windows)]
    restrict_to_owner_windows(dir);
    Ok(())
}

/// The one-line warning a caller should show after exporting. Transcripts
/// routinely contain API keys pasted into prompts and secrets surfaced by tool
/// output, and the file is plaintext on disk from here on.
pub fn secrets_warning(path: &Path) -> String {
    format!("{} is PLAINTEXT and may contain secrets from the transcript", path.display())
}

fn write_turn<W: Write>(w: &mut W, line: &str) -> std::io::Result<()> {
    let Ok(e) = serde_json::from_str::<RawEntry>(line) else { return Ok(()) };
    let Some(msg) = e.message else { return Ok(()) };
    let txt = text(&msg.content);
    if txt.trim().is_empty() {
        return Ok(());
    }
    match e.kind.as_str() {
        // Meta user entries are machinery (command output, hook noise), not
        // something the person typed.
        "user" if !e.is_meta => writeln!(w, "### 🧑 User\n\n{txt}\n"),
        "assistant" => writeln!(w, "### 🤖 Claude\n\n{txt}\n"),
        _ => Ok(()),
    }
}

/// The prose of a message's content: either a bare string or the text parts of
/// a content array (tool_use/tool_result blocks are skipped).
fn text(raw: &serde_json::Value) -> String {
    if let Some(s) = raw.as_str() {
        return s.to_string();
    }
    let Some(parts) = raw.as_array() else { return String::new() };
    parts
        .iter()
        .filter(|p| p.get("type").and_then(|t| t.as_str()) == Some("text"))
        .filter_map(|p| p.get("text").and_then(|t| t.as_str()))
        .filter(|t| !t.trim().is_empty())
        .collect::<Vec<_>>()
        .join("\n\n")
}

/// A filesystem-safe default file name for a mission's export.
pub fn default_name(m: &Mission) -> String {
    let short: String = m.id.chars().take(8).collect();
    // No title: the short id alone, not "<full-id>-<short-id>".
    if m.title.trim().is_empty() {
        return format!("{short}.md");
    }
    let slug: String = m
        .title
        .chars()
        .map(|c| if c.is_alphanumeric() || c == '-' || c == '_' { c } else { '-' })
        .collect();
    let slug = slug.trim_matches('-').to_lowercase();
    let slug: String = slug.chars().take(60).collect();
    // A title of only punctuation slugs to nothing — fall back to the id.
    if slug.is_empty() {
        format!("{short}.md")
    } else {
        format!("{slug}-{short}.md")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn transcript() -> String {
        [
            // A plain-string user turn.
            r#"{"type":"user","message":{"role":"user","content":"arregla el bug"}}"#,
            // An assistant turn with mixed parts: text survives, tool_use doesn't.
            r#"{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Mirando el código"},{"type":"tool_use","name":"Read","input":{}},{"type":"text","text":"Ya está"}]}}"#,
            // Meta user entries are machinery, not prose.
            r#"{"type":"user","isMeta":true,"message":{"role":"user","content":"<command-output>noise</command-output>"}}"#,
            // Empty content is skipped.
            r#"{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"   "}]}}"#,
            // A summary line has no message at all.
            r#"{"type":"summary","summary":"x"}"#,
            // A malformed line must not abort the export.
            r#"{not json"#,
        ]
        .join("\n")
    }

    #[test]
    fn renders_prose_and_skips_machinery() {
        let dir = tempfile::tempdir().unwrap();
        let src = dir.path().join("t.jsonl");
        std::fs::write(&src, transcript()).unwrap();

        let mut m = Mission {
            id: "abcd1234efgh".into(),
            project: "C--Users-me-proj".into(),
            title: "Arreglar el scanner".into(),
            cwd: r"C:\p".into(),
            git_branch: "v2".into(),
            path: src.to_string_lossy().into_owned(),
            user_msgs: 2,
            assistant_msgs: 2,
            ..Default::default()
        };
        m.tools.insert("Read".into(), 3);

        let out = dir.path().join("out.md");
        mission(&m, &out).unwrap();
        let md = std::fs::read_to_string(&out).unwrap();

        // Header carries the identity and the counters.
        assert!(md.starts_with("# Arreglar el scanner"));
        assert!(md.contains("`abcd1234efgh`"));
        assert!(md.contains("**Branch**: v2"));
        assert!(md.contains("**Messages**: 4 · **Tool calls**: 3"));
        // Prose from both roles, in order.
        assert!(md.contains("### 🧑 User\n\narregla el bug"));
        assert!(md.contains("### 🤖 Claude\n\nMirando el código\n\nYa está"));
        // Machinery, empties and junk are all absent.
        assert!(!md.contains("command-output"), "meta turns excluded");
        assert!(!md.contains("tool_use"), "tool blocks excluded");
        assert_eq!(md.matches("### 🤖 Claude").count(), 1, "empty turn skipped");
    }

    #[test]
    fn title_falls_back_to_id_and_names_are_safe() {
        let m = Mission { id: "0123456789".into(), ..Default::default() };
        assert_eq!(default_name(&m), "01234567.md");
        let m2 = Mission { id: "0123456789".into(), title: "Fix: the/thing (v2)!".into(), ..Default::default() };
        let n = default_name(&m2);
        // Punctuation becomes '-', and the trailing run is trimmed.
        assert_eq!(n, "fix--the-thing--v2-01234567.md");
        assert!(!n.contains('/') && !n.contains(':'), "no path separators");
        // A title that slugs to nothing also falls back to the id.
        let m3 = Mission { id: "0123456789".into(), title: "!!! ///".into(), ..Default::default() };
        assert_eq!(default_name(&m3), "01234567.md");
    }

    #[test]
    fn text_handles_both_content_shapes() {
        assert_eq!(text(&serde_json::json!("hola")), "hola");
        assert_eq!(text(&serde_json::json!([{"type":"text","text":"a"},{"type":"text","text":"b"}])), "a\n\nb");
        assert_eq!(text(&serde_json::json!([{"type":"tool_result","content":"x"}])), "");
        assert_eq!(text(&serde_json::json!(null)), "");
        assert_eq!(text(&serde_json::json!(42)), "");
    }
}
