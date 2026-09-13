//! Text measured in terminal cells, preserving Unicode grapheme clusters.
use unicode_segmentation::UnicodeSegmentation;
use unicode_width::UnicodeWidthStr;

pub fn width(s: &str) -> usize {
    UnicodeWidthStr::width(s)
}

/// Shorten to at most `w` cells, including the ellipsis, without splitting emoji
/// sequences or a letter from its combining accents.
pub fn clip(s: &str, w: usize) -> String {
    if w == 0 {
        return String::new();
    }
    if width(s) <= w {
        return s.to_string();
    }
    let mut out = String::new();
    let mut used = 0;
    for grapheme in s.graphemes(true) {
        let cells = width(grapheme);
        if used + cells > w - 1 {
            break;
        }
        out.push_str(grapheme);
        used += cells;
    }
    out.push('…');
    out
}

/// Display the end of an append-only edit, reserving a cell for its caret.
/// The full value remains unchanged; clipping never splits a grapheme.
pub fn edit_line(s: &str, w: usize) -> String {
    if w == 0 { return String::new(); }
    if width(s) < w { return format!("{s}▌"); }
    if w == 1 { return "▌".into(); }
    let mut used = 0;
    let mut start = s.len();
    for (index, grapheme) in s.grapheme_indices(true).rev() {
        let cells = width(grapheme);
        if used + cells > w - 2 { break; }
        used += cells;
        start = index;
    }
    format!("…{}▌", &s[start..])
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn edits_keep_the_caret_and_complete_suffix_graphemes() {
        assert_eq!(edit_line("abc", 4), "abc▌");
        assert_eq!(edit_line("abcdef", 5), "…def▌");
        assert_eq!(edit_line("prefix👩‍💻", 4), "…👩‍💻▌");
        assert_eq!(edit_line("prefixe\u{301}", 3), "…e\u{301}▌");
        for value in ["long value", "日本語", "prefix👩‍💻", "e\u{301}cole"] {
            for cells in 0..30 {
                let shown = edit_line(value, cells);
                assert!(width(&shown) <= cells);
                assert!(cells == 0 || shown.ends_with('▌'));
            }
        }
    }

    #[test]
    fn clipping_fits_cells_and_preserves_graphemes() {
        assert_eq!(clip("short", 10), "short");
        assert_eq!(clip("0123456789", 5), "0123…");
        assert_eq!(clip("échéance très longue", 6), "échéa…");
        assert_eq!(clip("日本語のタイトル", 4), "日…");
        assert_eq!(clip("e\u{301}cole", 3), "e\u{301}c…");
        assert_eq!(clip("👩‍💻 coding", 3), "👩‍💻…");
        for s in ["abc", "日本語のタイトル", "👩‍💻 coding", "e\u{301}cole"] {
            for w in 0..20 {
                assert!(width(&clip(s, w)) <= w);
            }
        }
        assert_eq!(clip("abc", 0), "");
    }
}
