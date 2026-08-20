//! Status-line segments contributed by anything that can write a file.
//!
//! v1 let a module add to the status line by declaring a command Houston ran on
//! every render (with a TTL and a 2.5 s timeout). That is exactly the shape
//! Decision 2 removed: Claude cancels a status-line script that is still running,
//! so a render may not execute anything — least of all a plugin, whose cost is
//! unbounded.
//!
//! So the mechanism inverts. **The producer writes; Houston reads.** Anything
//! that already knows the fact — the `hcx` bus server counting its peers, a
//! scheduled task, a Claude `Stop` hook, a `houston` verb — drops one short line
//! into `<store>/segments/<name>.txt`, and the render concatenates the segments
//! the config asks for. Cost is one file read each; there is no timeout to blow,
//! no process to spawn, and nothing to sandbox.
//!
//! **Everything read here is untrusted text**, because a file has no author. It
//! is sanitized on the way in: first line only, escape sequences and control
//! characters removed, length capped. That is not tidiness — a segment that could
//! emit ANSI could repaint the whole line and forge Houston's own fields, for
//! instance showing the `▸` active-account marker against the wrong account.
//! Truncating an honest segment is a cosmetic bug; letting one lie about which
//! account is live is not.

use crate::paths::store_dir;
use std::path::PathBuf;
use std::time::{Duration, SystemTime};

/// Longest a segment may be, in characters, unless its config says otherwise.
pub const DEFAULT_MAX_CHARS: usize = 24;

pub fn dir() -> PathBuf {
    store_dir().join("segments")
}

pub fn path_of(name: &str) -> PathBuf {
    // A name is a file stem, never a path: a segment called "../../.credentials"
    // must not be able to read outside the segments dir.
    let safe: String = name.chars().filter(|c| c.is_alphanumeric() || *c == '-' || *c == '_').collect();
    dir().join(format!("{safe}.txt"))
}

/// One configured segment.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Spec {
    pub name: String,
    /// Drop the segment when its file has not been written for this long. `None`
    /// keeps it forever, which is right for something slow-moving (a git badge)
    /// and wrong for a heartbeat — so it is the producer's call, expressed in
    /// config rather than guessed here.
    pub ttl: Option<Duration>,
    pub max_chars: usize,
}

/// Read one segment's text, or `None` when it is missing, empty, expired, or
/// unreadable. A segment that cannot be read is simply absent — never an error
/// and never a placeholder, because the status line has no room to explain
/// itself.
pub fn read(spec: &Spec) -> Option<String> {
    let path = path_of(&spec.name);
    let meta = std::fs::metadata(&path).ok()?;
    if let Some(ttl) = spec.ttl {
        let age = SystemTime::now().duration_since(meta.modified().ok()?).unwrap_or_default();
        if age > ttl {
            return None;
        }
    }
    let raw = std::fs::read_to_string(&path).ok()?;
    let text = sanitize(&raw, spec.max_chars);
    (!text.is_empty()).then_some(text)
}

/// Read every configured segment, in order, keeping only the ones with something
/// to say.
pub fn read_all(specs: &[Spec]) -> Vec<String> {
    specs.iter().filter_map(read).collect()
}

/// Make untrusted text safe to print on one shared line.
///
/// - **First line only.** A multi-line file must not break the one-line contract.
/// - **No escape sequences.** ESC and everything up to a sequence terminator go,
///   so a segment cannot colour, move the cursor, or clear the screen.
/// - **No control characters.** Tabs become spaces; the rest are dropped.
/// - **Capped length**, char-safe, with `…` to show that it was cut.
pub fn sanitize(raw: &str, max_chars: usize) -> String {
    let first = raw.lines().next().unwrap_or("");
    let mut out = String::with_capacity(first.len());
    let mut chars = first.chars().peekable();
    while let Some(c) = chars.next() {
        if c == '\u{1b}' {
            // Drop the whole escape sequence. The shapes, from the ANSI/ECMA-48
            // split — getting this wrong is easy in a way that LEAKS: a first
            // attempt stopped at the first byte in @..~, which is the CSI
            // introducer `[` itself, so `ESC[31m` printed "31m".
            match chars.peek().copied() {
                // String sequences (OSC, DCS, PM, APC): terminated by BEL or ST.
                Some(']') | Some('P') | Some('X') | Some('^') | Some('_') => loop {
                    match chars.next() {
                        // BEL terminates. So does ST, which is ESC followed by a
                        // backslash — the backslash has to go with it, or it
                        // prints. (A backslash NOT after ESC is payload, and
                        // treating it as a terminator would leak the rest.)
                        Some('\u{7}') | None => break,
                        Some('\u{1b}') => {
                            if chars.peek() == Some(&'\\') {
                                chars.next();
                            }
                            break;
                        }
                        Some(_) => {}
                    }
                },
                // CSI: introducer, then parameters/intermediates, then one final
                // byte in @..~.
                Some('[') => {
                    chars.next();
                    while chars.peek().is_some_and(|p| ('\u{20}'..='\u{3f}').contains(p)) {
                        chars.next();
                    }
                    chars.next();
                }
                // ESC + intermediates + final (e.g. `ESC ( B`, charset select).
                Some(p) if ('\u{20}'..='\u{2f}').contains(&p) => {
                    while chars.peek().is_some_and(|p| ('\u{20}'..='\u{2f}').contains(p)) {
                        chars.next();
                    }
                    chars.next();
                }
                // Any other two-character escape.
                Some(_) => {
                    chars.next();
                }
                // A trailing lone ESC: nothing left to drop.
                None => {}
            }
            continue;
        }
        if c == '\t' {
            out.push(' ');
            continue;
        }
        // Drop C0 controls and DEL. Everything printable — including the block
        // glyphs and arrows a segment will want — stays.
        if (c as u32) < 0x20 || c == '\u{7f}' {
            continue;
        }
        out.push(c);
    }
    let out = out.trim();
    if out.chars().count() <= max_chars {
        return out.to_string();
    }
    let mut cut: String = out.chars().take(max_chars.saturating_sub(1)).collect();
    cut.push('…');
    cut
}

/// Write a segment (the producer side, for `houston segment set` and for tests).
/// Sanitized on the way in too, so a bad value never reaches the file.
pub fn write(name: &str, text: &str) -> std::io::Result<()> {
    let path = path_of(name);
    std::fs::create_dir_all(path.parent().unwrap_or(std::path::Path::new(".")))?;
    let clean = sanitize(text, DEFAULT_MAX_CHARS);
    crate::atomic::write(&path, format!("{clean}\n").as_bytes())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sanitize_keeps_the_glyphs_and_drops_the_weapons() {
        // What a real segment looks like (v1's houston-connect counter).
        assert_eq!(sanitize("⇄ 3p·1q", 24), "⇄ 3p·1q");
        // Colour codes cannot survive: a segment that could paint could forge
        // Houston's own fields.
        assert_eq!(sanitize("\u{1b}[31mred\u{1b}[0m", 24), "red");
        // Cursor moves and screen clears go the same way.
        assert_eq!(sanitize("a\u{1b}[2Jb\u{1b}[1;1Hc", 24), "abc");
        // An OSC (window title) sequence, terminated by BEL and by ESC \.
        assert_eq!(sanitize("x\u{1b}]0;pwned\u{7}y", 24), "xy");
        assert_eq!(sanitize("x\u{1b}]0;pwned\u{1b}\\y", 24), "xy");
        // A backslash inside the payload is NOT a terminator; treating it as one
        // would leak the tail of the sequence as text.
        assert_eq!(sanitize("x\u{1b}]0;c:\\path\u{7}y", 24), "xy");
        // An UNTERMINATED escape eats the rest of the line rather than leaking.
        assert_eq!(sanitize("ok\u{1b}[38;5;", 24), "ok");
        // Charset selection (ESC + intermediate + final) and a bare two-character
        // escape both vanish whole — the regression that started this: stopping at
        // the first byte in @..~ stops at the CSI introducer itself and prints the
        // parameters as text.
        assert_eq!(sanitize("a\u{1b}(Bb", 24), "ab");
        assert_eq!(sanitize("a\u{1b}7b", 24), "ab");
        assert_eq!(sanitize("\u{1b}[38;5;196mdeep red\u{1b}[m", 24), "deep red");
        // A lone trailing ESC leaves nothing behind.
        assert_eq!(sanitize("done\u{1b}", 24), "done");
        // Control characters go; a tab becomes a space so words stay apart. A
        // BARE BEL is just another control character (it only terminates a
        // sequence when one is open), so it leaves nothing behind.
        assert_eq!(sanitize("a\rb\u{7}c\td", 24), "abc d");
        // Only the first line is ever used.
        assert_eq!(sanitize("first\nsecond\nthird", 24), "first");
        assert_eq!(sanitize("  padded  ", 24), "padded");
    }

    #[test]
    fn sanitize_caps_length_char_safely() {
        assert_eq!(sanitize("abcdefghij", 5), "abcd…");
        // Multi-byte characters are counted as characters, not bytes.
        let s = sanitize("⇄⇄⇄⇄⇄⇄", 3);
        assert_eq!(s.chars().count(), 3);
        assert!(s.ends_with('…'));
        // A cap of zero or one degrades rather than panicking.
        assert_eq!(sanitize("abc", 1), "…");
        assert_eq!(sanitize("", 24), "");
    }

    #[test]
    fn a_name_cannot_escape_the_segments_directory() {
        // Pin the store: this reads store_dir() twice, and another test switching
        // HOUSTON_HOME between the two reads would fail it for the wrong reason.
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("HOUSTON_HOME", tmp.path()) };

        let evil = path_of("../../.credentials");
        assert_eq!(evil.parent(), Some(dir().as_path()), "must stay inside the segments dir: {evil:?}");
        assert!(!evil.to_string_lossy().contains(".."));
        // Separators are stripped rather than rejected, so a typo still lands
        // somewhere harmless.
        assert_eq!(path_of("a/b\\c").file_name().unwrap().to_string_lossy(), "abc.txt");

        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }

    #[test]
    fn reading_honours_presence_emptiness_and_ttl() {
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("HOUSTON_HOME", tmp.path()) };

        let spec = Spec { name: "connect".into(), ttl: None, max_chars: DEFAULT_MAX_CHARS };
        assert!(read(&spec).is_none(), "a missing segment is absent, not an error");

        write("connect", "⇄ 3p·1q").unwrap();
        assert_eq!(read(&spec).as_deref(), Some("⇄ 3p·1q"));

        // An empty (or whitespace-only) file contributes nothing.
        write("connect", "   ").unwrap();
        assert!(read(&spec).is_none());

        // TTL: a file older than its ttl is dropped. Backdate it by writing and
        // asking for a zero ttl, which every existing file is already past.
        write("connect", "⇄ 3p·1q").unwrap();
        let expiring = Spec { ttl: Some(Duration::from_secs(0)), ..spec.clone() };
        std::thread::sleep(Duration::from_millis(1100));
        assert!(read(&expiring).is_none(), "a stale heartbeat must not keep showing");
        assert!(read(&spec).is_some(), "the same file without a ttl still shows");

        // read_all keeps order and skips the ones with nothing to say.
        write("a", "one").unwrap();
        let specs = vec![
            Spec { name: "a".into(), ttl: None, max_chars: 24 },
            Spec { name: "missing".into(), ttl: None, max_chars: 24 },
            Spec { name: "connect".into(), ttl: None, max_chars: 24 },
        ];
        assert_eq!(read_all(&specs), vec!["one".to_string(), "⇄ 3p·1q".to_string()]);

        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }

    #[test]
    fn the_writer_sanitizes_too() {
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("HOUSTON_HOME", tmp.path()) };
        write("x", "\u{1b}[31mhi\u{1b}[0m\nsecond line").unwrap();
        let raw = std::fs::read_to_string(path_of("x")).unwrap();
        assert_eq!(raw, "hi\n", "nothing dangerous is even stored");
        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }
}
