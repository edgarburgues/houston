//! Core types: a Mission (one Claude conversation) and a Program (a logical
//! grouping of missions). Missions are read-only projections of the .jsonl
//! transcripts; the mutable bits (tags, notes, programs) live in the Store.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;

/// Immutable metadata extracted from one .jsonl transcript.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Mission {
    /// sessionId (== file base name).
    pub id: String,
    /// Absolute path to the .jsonl.
    pub path: String,
    /// Encoded project dir name.
    pub project: String,
    /// Dir to cd into for `claude --resume` (encodes back to `project`).
    pub cwd: String,
    /// Cwd of the last message (where work actually happened).
    pub last_cwd: String,
    /// Display title: explicit name, else aiTitle, else first prompt.
    pub title: String,
    /// User-set session name (claude -n / /rename), if present.
    pub name: String,
    /// Human slug (starry-tinkering-sky).
    pub slug: String,
    pub git_branch: String,
    pub version: String,
    pub first_prompt: String,
    pub last_prompt: String,
    pub first_time: Option<DateTime<Utc>>,
    pub last_time: Option<DateTime<Utc>>,
    pub user_msgs: u32,
    pub assistant_msgs: u32,
    pub tools: HashMap<String, u32>,
    pub size_bytes: u64,
    pub has_subagents: bool,
    /// File mtime in nanos, for incremental rescans.
    pub mtime_ns: i64,
    // No `search` field. It held the whole lowercased transcript prose, nothing
    // ever read it, and because `Mission` is what the scan cache stores it was
    // ~92% of that cache. A search feature, if one is ever added, either
    // matches the fields already here (title, slug, prompts, branch) or
    // re-reads the transcripts — which is a cost paid when searching rather
    // than on every scan of every session.
    //
    // Removing it does not break an existing cache: serde ignores fields it
    // does not know, so an old file still loads, one field lighter.
}

impl Mission {
    /// A session-id alone is NOT unique: the same session resumed from
    /// different working dirs produces separate transcripts under different
    /// project dirs. The key is project + id.
    pub fn key(&self) -> String {
        format!("{}/{}", self.project, self.id)
    }

    pub fn message_count(&self) -> u32 {
        self.user_msgs + self.assistant_msgs
    }

    pub fn tool_calls(&self) -> u32 {
        self.tools.values().sum()
    }
}

/// User-editable data Houston keeps per mission (never written into the .jsonl).
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Meta {
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub tags: Vec<String>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub note: String,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub pinned: bool,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub archived: bool,
    /// Re-points the mission's working directory when the project folder
    /// moved: the transcript's own cwd is immutable history.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub cwd_override: String,
    /// How this mission opens. Empty for almost every mission, and then absent
    /// from the file entirely — which is what keeps `store.json` byte-identical
    /// for anyone still running v1 (Go ignores fields it does not model, and
    /// v1 is the rollback path).
    #[serde(skip_serializing_if = "Launch::is_empty")]
    pub launch: Launch,
}

/// Per-mission launch preferences: the flags this conversation should reopen
/// with. Set once, applied on every resume.
///
/// Strings rather than enums on purpose — `--model`, `--effort`, `--agent` and
/// `--permission-mode` all take values Claude Code owns and extends, and a
/// Houston that validated them would reject a value Claude accepts on the day
/// Claude adds one.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Launch {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub effort: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub permission_mode: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub agent: String,
    /// `--worktree [name]` takes an OPTIONAL value, so three states must be
    /// distinguishable: `None` = off, `Some("")` = on with no name, `Some(n)` =
    /// on with a name. A plain String would collapse the first two.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub worktree: Option<String>,
    /// `--fork-session`: reopen a copy instead of mutating the original.
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub fork: bool,
    /// `--safe-mode`: open with Houston's own customizations off — the way to
    /// tell Houston's fault from Claude's.
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub safe_mode: bool,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub add_dirs: Vec<String>,
    /// Passed verbatim after everything else and never parsed — the escape hatch
    /// for a flag Houston does not model yet.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub extra: Vec<String>,
}

impl Launch {
    /// Nothing set. This is the SINGLE definition of "empty": it decides both
    /// whether the field is serialized and whether the store prunes the mission's
    /// metadata. Two definitions would drift, and the drift would silently delete
    /// a mission whose only metadata is its launch preferences.
    pub fn is_empty(&self) -> bool {
        *self == Launch::default()
    }
}

/// A logical grouping of missions (the ".prog" manifest).
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Program {
    pub name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub description: String,
    /// Ordered mission keys.
    pub missions: Vec<String>,
}

#[cfg(test)]
mod tests {
    use super::*;

    /// `search` held the whole lowercased transcript prose and nothing ever
    /// read it. Because `Mission` is what the scan cache stores, it was 95.4%
    /// of the real cache on this machine — 8.79 MB down to 401 KB, measured on
    /// the file the previous build had written.
    ///
    /// Both halves matter: it must stay out of what we write, and a cache that
    /// still carries it has to keep loading, or the first scan after an upgrade
    /// silently re-parses every transcript.
    #[test]
    fn a_mission_writes_no_search_field_and_still_reads_an_old_one() {
        let m = Mission { id: "s".into(), title: "hi".into(), ..Default::default() };
        let json = serde_json::to_string(&m).unwrap();
        assert!(!json.contains("search"), "the dead field came back: {json}");

        let old = r#"{"id":"s","title":"hi","search":"a whole transcript, lowercased"}"#;
        let back: Mission = serde_json::from_str(old).expect("an existing cache entry must still load");
        assert_eq!(back.id, "s");
        assert_eq!(back.title, "hi");
    }

    #[test]
    fn key_is_project_plus_id() {
        let m = Mission {
            id: "abc".into(),
            project: "C--Users-me-proj".into(),
            ..Default::default()
        };
        assert_eq!(m.key(), "C--Users-me-proj/abc");
    }

    #[test]
    fn counters_sum() {
        let mut m = Mission { user_msgs: 3, assistant_msgs: 4, ..Default::default() };
        m.tools.insert("Bash".into(), 2);
        m.tools.insert("Read".into(), 5);
        assert_eq!(m.message_count(), 7);
        assert_eq!(m.tool_calls(), 7);
    }
}
