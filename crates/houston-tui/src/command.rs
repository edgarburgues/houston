//! The command registry — every key-driven capability described as DATA, the
//! single source the `?` which-key overlay and the `:` palette both derive
//! from (v1's fix for help outgrowing a one-line footer once plugins piled
//! on). Core commands come first by category; each widget contributes its
//! own commands as its section. This is the in-process face of the
//! component API's "Command" surface.

/// One binding described as data.
#[derive(Clone, Debug)]
pub struct Command {
    /// Every key form that triggers it (e.g. ["j"], ["esc"]). keys[0] is the
    /// canonical dispatch key.
    pub keys: Vec<String>,
    /// Display form for the overlay ("↑/k", "r").
    pub label: String,
    pub title: String,
    /// Overlay section. Core commands use a category; widget commands use the
    /// widget's own id so each becomes its own section.
    pub category: String,
    /// "" for core; else the widget/plugin id (renders as a module section).
    pub source: String,
    /// Include in the short footer hint (kept minimal on purpose).
    pub hint: bool,
}

impl Command {
    pub fn core(keys: &[&str], label: &str, title: &str, category: &str, hint: bool) -> Self {
        Command {
            keys: keys.iter().map(|s| s.to_string()).collect(),
            label: label.into(),
            title: title.into(),
            category: category.into(),
            source: String::new(),
            hint,
        }
    }

    /// A widget-contributed command (single key), sectioned under the widget.
    pub fn widget(key: &str, title: &str, widget_id: &str) -> Self {
        Command {
            keys: vec![key.to_string()],
            label: key.to_string(),
            title: title.into(),
            category: widget_id.into(),
            source: widget_id.into(),
            hint: false,
        }
    }
}

/// The always-present global commands: focus, discovery, quit. (Tab switching
/// joins here when tabs land.)
pub fn global_commands() -> Vec<Command> {
    vec![
        Command::core(&["enter"], "↵", "resume selected chat", "Chat", false),
        Command::core(&["o"], "o", "how this chat opens (model, worktree, …)", "Chat", false),
        Command::core(&["L"], "L", "recheck which chats are live", "Chat", false),
        Command::core(&["tab"], "tab", "next pane", "Navigate", false),
        Command::core(&["shift+tab"], "⇧tab", "previous pane", "Navigate", false),
        Command::core(&[":", "ctrl+p"], ":", "command palette", "System", true),
        Command::core(&["?"], "?", "help", "System", true),
        Command::core(&["q", "esc"], "q", "quit", "System", true),
    ]
}

/// A short, curated footer hint (hint-flagged commands only) — the full list
/// lives in the `?` overlay, so the footer never collapses under plugins.
pub fn footer_hint(commands: &[Command]) -> String {
    commands
        .iter()
        .filter(|c| c.hint)
        .map(|c| format!("{} {}", c.label, c.title))
        .collect::<Vec<_>>()
        .join(" · ")
}

/// Group commands for the overlay: core categories in first-appearance order,
/// then one section per widget (source) in appearance order.
pub fn sections(commands: &[Command]) -> Vec<(String, bool, Vec<Command>)> {
    // (title, is_widget_section, rows)
    let mut order: Vec<String> = Vec::new();
    let mut map: std::collections::HashMap<String, (bool, Vec<Command>)> = std::collections::HashMap::new();
    for c in commands {
        let (key, is_widget) = if c.source.is_empty() {
            (c.category.clone(), false)
        } else {
            (c.source.clone(), true)
        };
        if !map.contains_key(&key) {
            order.push(key.clone());
            map.insert(key.clone(), (is_widget, Vec::new()));
        }
        map.get_mut(&key).unwrap().1.push(c.clone());
    }
    // Core categories first (in appearance order), then widget sections.
    order.sort_by_key(|k| map.get(k).map(|(w, _)| *w).unwrap_or(false));
    order
        .into_iter()
        .map(|k| {
            let (w, rows) = map.remove(&k).unwrap();
            (k, w, rows)
        })
        .collect()
}

/// Case-insensitive subsequence fuzzy score; lower ranks higher. None if the
/// query is not a subsequence of text.
pub fn fuzzy_score(query: &str, text: &str) -> Option<usize> {
    let q: Vec<char> = query.to_lowercase().chars().collect();
    if q.is_empty() {
        return Some(0);
    }
    let t: Vec<char> = text.to_lowercase().chars().collect();
    let mut score = 0usize;
    let mut ti = 0usize;
    for qc in q {
        let mut found = false;
        while ti < t.len() {
            if t[ti] == qc {
                score += ti;
                ti += 1;
                found = true;
                break;
            }
            ti += 1;
        }
        if !found {
            return None;
        }
    }
    Some(score)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn footer_hint_only_flagged() {
        let cmds = vec![
            Command::core(&["j"], "j", "move", "Navigate", false),
            Command::core(&["?"], "?", "help", "System", true),
            Command::core(&[":"], ":", "palette", "System", true),
        ];
        assert_eq!(footer_hint(&cmds), "? help · : palette");
    }

    #[test]
    fn sections_core_first_then_widgets() {
        let cmds = vec![
            Command::core(&["?"], "?", "help", "System", true),
            Command::widget("r", "refresh", "basics:quota"),
            Command::core(&["j"], "j", "move", "Navigate", false),
        ];
        let secs = sections(&cmds);
        assert_eq!(secs[0].0, "System");
        assert!(!secs[0].1);
        assert_eq!(secs[1].0, "Navigate");
        assert_eq!(secs[2].0, "basics:quota");
        assert!(secs[2].1, "widget section flagged");
    }

    #[test]
    fn fuzzy_matches_subsequence_only() {
        assert!(fuzzy_score("qt", "quota").is_some());
        assert!(fuzzy_score("", "anything").is_some());
        assert!(fuzzy_score("zzz", "quota").is_none());
        // tighter (earlier) match scores lower
        assert!(fuzzy_score("qu", "quota").unwrap() < fuzzy_score("ta", "quota").unwrap());
    }
}
