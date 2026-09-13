# Houston

A terminal session manager for Claude Code, written in Rust. Browse and resume
conversations, balance multiple accounts by quota, and arrange the interface
as a configurable tree of panels.

## Build and install

With Rust and PowerShell 7 installed, build the current implementation:

```powershell
cargo build --locked --release -p houston
# Windows: build and install to ~/.local/bin without stopping running sessions
pwsh -File install-local.ps1
```

`packaging/Install.ps1` authenticates release downloads using an independently
installed [minisign](https://github.com/jedisct1/minisign), a pinned public key,
and the requested release tag, then checks SHA-256 before installing. Invalid
signatures fail closed. If downloads or minisign are unavailable, a local source
checkout can be built with Cargo. Bundled local binaries are trusted as part of
the package; obtain that package from a trusted source. `houston update` verifies
signatures internally. `-NoProfileEdit` skips shell profile and PATH changes.

For a disposable preview without real accounts or Claude configuration:

```powershell
pwsh -File try-v2.ps1
```

The preview intentionally has no real conversations. Normal launches use
`~/.claude/houston`, per-account config directories and the shared data store.

## Use

```powershell
houston accounts add work
houston accounts add personal
houston run                     # choose an account by quota; /login when needed
houston run -a work              # force an account
houston                         # open the TUI
houston --help
```

Account creation provisions its config directory. Accounts have separate logins
and share conversation data through junctions on Windows or symlinks on Unix.
Normal launch and statusline paths can repair shared data links; they are not
read-only diagnostics.

| Command | Purpose |
|---|---|
| `houston run [-a <id>] [-- <args>]` | Launch Claude with a selected account |
| `houston accounts [ls\|add\|rm]` | Manage account registrations |
| `houston usage [--refresh] [--json\|--pick]` | Inspect quota and explain account selection |
| `houston live [--json]` | Query running sessions |
| `houston journal` | Inspect launch and hook events |
| `houston export <id> [out.md]` | Export a transcript as Markdown |
| `houston doctor [--fix]` | Audit or explicitly repair configuration |
| `houston compat` | Report recorded Claude compatibility assumptions |
| `houston retention [--keep <days>\|--default]` | Inspect or change transcript retention |
| `houston mcp`, `houston plugin`, `houston policy` | Manage Claude configuration across accounts |
| `houston hooks [status\|install\|uninstall]` | Manage Claude integration hooks |
| `houston statusline`, `houston segment` | Render quota and manage extra status text |
| `houston update [--check]` | Check or install a signed release |

`houston plugin` manages **Claude Code plugins**. Houston's own panel plugins
are WASM modules using `houston-api`, without WASI filesystem or network access.
See [plugin examples](examples/plugins/README.md). The old executable Go module
runtime is not supported in Rust.

## Interface

Use arrows or `j/k` to navigate, `Enter` to resume, `?` for shortcuts, `:` for
the command palette, `o` for launch options, and `0` for settings. Mouse focus,
scrolling and border resizing are supported. A running-session warning offers
resume/fork choices before attaching to an already open conversation.

The interface adapts to the terminal's character grid and each panel's minimum
usable size. It keeps the configured splits when they fit, then shows two related
panels side by side or stacked, and finally the focused panel alone. `Tab` /
`Shift+Tab` cycle all panels; `F10` maximizes the active panel and restores the
automatic layout. Resizing and maximizing do not overwrite saved proportions.
`[` / `]` switch tabs. Launch options follow the selected field on short screens;
long edits in options, settings and the command palette keep their suffix and
caret visible without changing the value. Unicode clipping preserves grapheme
clusters. Font size and DPI scaling are controlled by your terminal emulator;
very small grids necessarily show less content. The renderer is tested from
0×0 through 344×90 and 60×120 cells.

The layout, per-panel settings and theme live in `config-v2.json`. Built-in
panels include missions, filters, preview, quota, git, settings and `probe`.
A probe can read a file or run an explicitly configured argv in the background.

## Development and verification

```powershell
pwsh -File packaging/Test.ps1
cargo clippy --locked --workspace --all-targets -- -D warnings
```

The test wrapper redirects application paths and fallback home directories to
a fresh temporary tree; it never copies real credentials. Tests that require
live services remain ignored by default. CI validates Linux, Windows and macOS.

| Crate | Responsibility |
|---|---|
| `houston-core` | Session data, accounts, configuration, persistence and updates |
| `houston-api` | JSON contract for WASM panels |
| `houston-plugins` | Process-isolated WASM host |
| `houston-tui` | Container layout, built-in widgets and input routing |
| `houston` | CLI and application entry point |

[Architecture](docs/ARCHITECTURE.md) describes the Rust design. The Go
implementation this replaced is preserved in this repository's history at tag
`v1.2.1`; it is not part of a Cargo build.

Releases are cut from `v2.*` tags. What is published here is a curated tree
rather than a direct publication of development history, so this commit log is
shorter than the one it is derived from.

## License

[MIT](LICENSE).
