# 🚀 Houston

Mission control for **Claude Code**: run several accounts behind one command,
and browse, organize and resume every conversation from one TUI.

A small Rust kernel under an interface that is customizable out of the box:
the whole screen is a tree of panes defined in config, every view is a widget,
and extensions are sandboxed WASM. (An earlier Go implementation lives on in
the history — tag `v1.2.1`, branch `v1-final`.)

## The idea

Claude Code ties login and onboarding to the authenticated account, not just
the config directory. Houston gives each account its own `CLAUDE_CONFIG_DIR`
and shares the data through native OS links:

- **One config dir per account** (`~/.claude-accounts/account-<id>`): its own
  `/login`, isolated onboarding, the account's real email.
- **Shared data** (`projects`, `sessions`, `plugins`, …) and user
  customizations (`skills`, `commands`, `agents`, …) linked into a common
  `~/.claude-shared` store — junctions on Windows, symlinks elsewhere — so
  every account sees and resumes **all** conversations.
- **Quota-aware balancing**: `houston run` and the TUI's resume rank accounts
  by pressure (each rate-limit window weighted by how far it is from
  resetting) and pick the least loaded one. Pin with `-a <id>`.
- **Concurrency-safe**: each terminal carries its account in its own
  `CLAUDE_CONFIG_DIR`; different terminals run different accounts at once.

## Install

```sh
cargo build --release -p houston
# then put target/release/houston(.exe) on PATH — or, on Windows:
pwsh -File install-local.ps1     # builds and installs without killing a running Houston
```

Self-updates are **signed**: releases carry a minisign signature bound to
their tag, verified against a public key compiled into the binary
(`houston update`).

## Use

```
houston                    the TUI: conversations, live sessions, quota, tabs
houston run [-a <id>] …    launch claude on the best (or a pinned) account
houston doctor [--fix]     audit accounts, links, config, hooks, retention
houston usage [--refresh]  per-account quota; --pick explains resume's choice
houston retention          how long transcripts survive; writes only when told
houston compat             what Houston assumes about Claude Code, and drift
```

In the TUI: `Enter` resumes (asking first if the chat is already open
elsewhere), `o` sets per-mission launch options, digits switch tabs, `0` opens
Settings, `?` shows every key.

### Claude Code integration

Houston installs a **status line** (pure read — nothing executes during a
render) showing every account's quota plus segments any process can
contribute by writing a file; **hooks** so it learns instantly about sessions
starting, rate limits and logins; and `claude agents --json` powers live
`●`/`○` markers with an "already open" prompt. `houston compat` records every
such surface, how to re-check it, and the Claude Code release it was last
verified against — they are unversioned and fail silently, so drift is
reported rather than discovered.

## Extending

- **Layout is config**: the screen is a `Split`/`Pane` tree in
  `config-v2.json`; move anything anywhere, drag borders, add tabs.
- **`probe` panes** show anything a command prints or a file contains,
  refreshed off the render thread — "show me the server" needs no code.
- **Plugins are WASM**, run in a killable child process with **no WASI**: no
  files, no network, no way to reach credentials. There is deliberately no
  script/exec runtime — an arbitrary command carries the user's privileges,
  and no manifest field changes that.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the design and its
decisions.

## Platform

Windows, macOS and Linux. Rust ≥ 1.75. The store lives in `~/.claude/houston`
(`HOUSTON_HOME` overrides it, which is also how the test suite stays away from
real data).

## License

MIT — see [LICENSE](LICENSE).
