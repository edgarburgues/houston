//! What the Settings tab may change, described as data.
//!
//! Same shape as `policy::KEYS`, and for the same reason: a table is auditable at
//! a glance, and a UI generated from it cannot offer something nobody decided to
//! allow.
//!
//! Three columns carry the design:
//!
//! - **`scope`** says which file a row lives in, which decides who writes it.
//! - **`kind`** says how it is edited, so the view never has to guess whether a
//!   value is a flag, a vocabulary or free text.
//! - **`owner`** is the module that must perform the write. `permissions` and its
//!   neighbours belong to `policy`, `statusLine` to `claude_settings`, `hooks` to
//!   `hooks`. A settings screen that wrote those directly would be a second
//!   subsystem editing one file, and the two would eventually disagree about what
//!   is in it.
//!
//! What is deliberately absent is as much of the design as what is here:
//! `cleanupPeriodDays` is an Action rather than a value, because Houston must
//! never change retention as a side effect (Phase 0.3); and there is no row for
//! arbitrary keys, because "edit anything" over a file Claude parses at startup is
//! how you discover a typo when Claude stops opening.

/// Where the value lives.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Scope {
    /// Houston's own `config-v2.json`.
    Houston,
    /// Every account's `settings.json`, kept identical across the fleet.
    Fleet,
}

/// How a row is edited.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Kind {
    /// Present/absent, or true/false.
    Flag,
    /// One of these, cycled. The empty string means "unset".
    Choice(&'static [&'static str]),
    /// A whole number.
    Number,
    /// Free text.
    Text,
    /// Not a value: something to run, owned by a module. The string is the label.
    Action(&'static str),
}

/// The module responsible for writing a row.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Owner {
    /// Houston's config, written by the app itself after a parse check.
    Config,
    /// `policy::set_everywhere`.
    Policy,
    /// `claude_settings`.
    ClaudeSettings,
    /// `hooks`.
    Hooks,
}

pub struct Entry {
    pub key: &'static str,
    /// What the row says on screen.
    pub label: &'static str,
    pub scope: Scope,
    pub kind: Kind,
    pub owner: Owner,
    /// One line, shown next to the focused row.
    pub why: &'static str,
}

/// Claude's vocabularies, taken from `claude --help` rather than from memory —
/// the same source the launch-options overlay uses, and for the same reason: a
/// list invented here would offer values the CLI rejects.
const EFFORTS: &[&str] = &["", "low", "medium", "high", "xhigh", "max"];
const MODELS: &[&str] = &["", "fable", "opus", "sonnet", "haiku"];
const EDITORS: &[&str] = &["", "normal", "vim"];
/// Claude's own themes, verbatim from the binary's list — including the
/// daltonized and ANSI variants, which someone needs and nobody would guess.
const CLAUDE_THEMES: &[&str] =
    &["", "dark", "light", "light-daltonized", "dark-daltonized", "light-ansi", "dark-ansi"];
/// The four spellings Claude's schema accepts. Not a duration field: anything
/// else is rejected, so a free-text row would invite a value that fails to load.
const DIALOG_EXPIRY: &[&str] = &["", "60s", "5m", "10m", "never"];
/// `accept < hold < refuse`, in that order of strictness. Houston writes user
/// scope, which Claude reads as a trusted source, so a value set here governs
/// unless a project file is *stricter* still.
const CROSS_SESSION_INBOUND: &[&str] = &["", "accept", "hold", "refuse"];

/// Everything the Settings tab offers, in the order it is shown. Sections come
/// from the scope changing, so the order matters.
pub const ENTRIES: &[Entry] = &[
    // NOT called `theme`: Claude has a per-account setting by that name, and two
    // different things sharing one key is how a settings screen writes to the
    // wrong file. This one identifies a ROW, not a JSON key — it edits Houston's
    // whole palette as a preset.
    Entry {
        key: "theme.preset",
        label: "Houston colour",
        scope: Scope::Houston,
        kind: Kind::Choice(&["mono", "blue"]),
        owner: Owner::Config,
        why: "Houston's own palette: plain black-and-white, or the blue accent",
    },
    Entry {
        key: "theme",
        label: "Claude theme",
        scope: Scope::Fleet,
        kind: Kind::Choice(CLAUDE_THEMES),
        owner: Owner::Policy,
        why: "Claude's own colours, the same in every account",
    },
    Entry {
        key: "effortLevel",
        label: "effort",
        scope: Scope::Fleet,
        kind: Kind::Choice(EFFORTS),
        owner: Owner::Policy,
        why: "default reasoning effort for new sessions, in every account",
    },
    Entry {
        key: "fallbackModel",
        label: "fallback model",
        scope: Scope::Fleet,
        kind: Kind::Choice(MODELS),
        owner: Owner::Policy,
        why: "used when the first choice is unavailable",
    },
    Entry {
        key: "editorMode",
        label: "editor keys",
        scope: Scope::Fleet,
        kind: Kind::Choice(EDITORS),
        owner: Owner::Policy,
        // "normal" and "vim" are the two the binary lists — no emacs, whatever the
        // muscle memory says.
        why: "vim bindings inside Claude, or normal editing",
    },
    Entry {
        key: "outputStyle",
        label: "output style",
        scope: Scope::Fleet,
        kind: Kind::Text,
        owner: Owner::Policy,
        why: "named output style; free text because you can define your own",
    },
    Entry {
        key: "includeCoAuthoredBy",
        label: "co-author trailer",
        scope: Scope::Fleet,
        kind: Kind::Flag,
        owner: Owner::Policy,
        why: "whether Claude adds itself as co-author on commits",
    },
    Entry {
        key: "alwaysThinkingEnabled",
        label: "always thinking",
        scope: Scope::Fleet,
        kind: Kind::Flag,
        owner: Owner::Policy,
        why: "extended thinking on by default",
    },
    Entry {
        key: "autoCompactEnabled",
        label: "auto-compact",
        scope: Scope::Fleet,
        kind: Kind::Flag,
        owner: Owner::Policy,
        why: "compact the context automatically when it fills",
    },
    Entry {
        key: "fileCheckpointingEnabled",
        label: "file checkpoints",
        scope: Scope::Fleet,
        kind: Kind::Flag,
        owner: Owner::Policy,
        why: "Claude keeps restorable copies of files it edits",
    },
    Entry {
        key: "autoMemoryEnabled",
        label: "auto memory",
        scope: Scope::Fleet,
        kind: Kind::Flag,
        owner: Owner::Policy,
        // Sits next to the retention row on purpose: they are the two halves of
        // "what does Claude keep about this project". The memory directory is the
        // one part of the shared transcripts tree the cleanup sweep spares.
        why: "Claude's notes per project, in memory/ — retention never deletes those",
    },
    Entry {
        key: "dialogExpiry",
        label: "dialog expiry",
        scope: Scope::Fleet,
        kind: Kind::Choice(DIALOG_EXPIRY),
        owner: Owner::Policy,
        why: "how long a dialog forwarded to a phone waits before it answers itself",
    },
    Entry {
        key: "crossSessionInbound",
        label: "messages from your other sessions",
        scope: Scope::Fleet,
        kind: Kind::Choice(CROSS_SESSION_INBOUND),
        owner: Owner::Policy,
        why: "may your other sessions message this one (macOS/Linux only)",
    },
    // Actions: owned by a module, and never a value this screen writes itself.
    Entry {
        key: "statusLine",
        label: "status line",
        scope: Scope::Fleet,
        kind: Kind::Action("install / repair"),
        owner: Owner::ClaudeSettings,
        why: "installs Houston's line and its 60s refresh; never touches another tool's",
    },
    Entry {
        key: "hooks",
        label: "hooks",
        scope: Scope::Fleet,
        kind: Kind::Action("install / remove"),
        owner: Owner::Hooks,
        why: "lets Claude tell Houston about sessions and rate limits",
    },
    Entry {
        key: "cleanupPeriodDays",
        label: "keep history",
        scope: Scope::Fleet,
        kind: Kind::Action("keep for 10 years"),
        owner: Owner::ClaudeSettings,
        why: "Claude deletes transcripts past this; Houston never changes it on its own",
    },
];

/// The entry for a key, if the schema has one.
pub fn find(key: &str) -> Option<&'static Entry> {
    ENTRIES.iter().find(|e| e.key == key)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn every_owned_key_is_owned_by_the_module_that_already_writes_it() {
        for e in ENTRIES {
            match e.owner {
                // Anything routed to policy must actually be a key policy
                // propagates, or the write would go somewhere it does not govern.
                Owner::Policy => assert!(crate::policy::is_known(e.key), "{} is not a policy key", e.key),
                // And the reverse: a key policy owns must not be claimed here by
                // somebody else.
                // A row that is not policy's must not carry a key policy governs —
                // unless it is an Action (the module does the write) or it lives in
                // Houston's own file, where the same NAME means something else.
                _ => assert!(
                    !crate::policy::is_known(e.key)
                        || matches!(e.kind, Kind::Action(_))
                        || e.scope == Scope::Houston,
                    "{} belongs to policy",
                    e.key
                ),
            }
        }
    }

    #[test]
    fn retention_is_an_action_not_a_value() {
        let e = find("cleanupPeriodDays").expect("present");
        assert!(matches!(e.kind, Kind::Action(_)), "a value row would let Houston change retention silently");
        assert_eq!(e.owner, Owner::ClaudeSettings, "policy deliberately does not own retention");
        assert!(!crate::policy::is_known("cleanupPeriodDays"));
    }

    #[test]
    fn the_choices_are_ones_claude_documents() {
        // Same guard as the launch overlay's: the empty string means unset, and
        // everything else must be a value the CLI accepts.
        for e in ENTRIES {
            if let Kind::Choice(vals) = e.kind {
                assert!(vals.len() > 1, "{} cycles nothing", e.key);
                // A FLEET value can be absent, and "" is how you get back there:
                // removing the key returns the account to Claude's default. A
                // HOUSTON value always exists, so an empty option would mean
                // nothing — the choices must simply be exhaustive.
                match e.scope {
                    Scope::Fleet => assert!(vals.contains(&""), "{} cannot be unset again", e.key),
                    Scope::Houston => assert!(!vals.contains(&""), "{} has no unset state to offer", e.key),
                }
            }
        }
        assert_eq!(find("effortLevel").map(|e| &e.kind), Some(&Kind::Choice(EFFORTS)));
    }

    #[test]
    fn labels_and_reasons_are_present_and_short() {
        for e in ENTRIES {
            assert!(!e.label.is_empty() && !e.why.is_empty(), "{}", e.key);
            // The `why` sits beside a row on one line; a paragraph would wrap and
            // push the value off screen.
            assert!(e.why.chars().count() <= 80, "{}: {} chars", e.key, e.why.chars().count());
        }
    }
}
