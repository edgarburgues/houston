//! Small text helpers shared by the CLI, the TUI and the core reports.

/// `s` shortened to at most `w` display cells, with `…` marking the cut.
///
/// By CHARACTERS, never bytes: `&s[..w]` panics when the cut lands inside a
/// multibyte character, and titles here are arbitrary user prose.
///
/// There were four near-identical copies of this — `policy`, `options`,
/// `widgets` and a `trim_value` in the settings pane — which is how they came
/// to disagree about the edge: one replaced the last kept character with the
/// ellipsis, another appended it, so the same string clipped to the same width
/// came out one cell wider in one pane than the other. The ellipsis counts
/// toward `w` here, so the result never exceeds the space it was given.
pub fn clip(s: &str, w: usize) -> String {
    if s.chars().count() <= w {
        return s.to_string();
    }
    let mut out: String = s.chars().take(w.saturating_sub(1)).collect();
    out.push('…');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clip_counts_characters_and_the_ellipsis() {
        assert_eq!(clip("short", 10), "short");
        assert_eq!(clip("exactly-10", 10), "exactly-10", "at the width, nothing is cut");
        assert_eq!(clip("0123456789", 5), "0123…");
        // The result must FIT: the ellipsis is part of the budget, not extra.
        assert_eq!(clip("0123456789", 5).chars().count(), 5);
        // Multibyte prose is where a byte slice panics.
        assert_eq!(clip("échéance très longue", 6), "échéa…");
        assert_eq!(clip("日本語のタイトル", 4), "日本語…");
        // Degenerate widths must not panic.
        assert_eq!(clip("abc", 1), "…");
        assert_eq!(clip("abc", 0), "…");
        assert_eq!(clip("", 0), "");
    }
}
