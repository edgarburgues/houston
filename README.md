# 🚀 Houston

Mission-control for **Claude Code**: balances multiple accounts behind a single
command **and** lets you browse, organize and resume your conversations — one
tool, one install.

## The idea

Claude Code (2.1.x) ties login and onboarding to the **authenticated account**,
not just the config directory. So that several accounts can coexist in
interactive mode without re-onboarding or an identity-less "Claude API",
Houston gives each account its own `CLAUDE_CONFIG_DIR` and shares the data
through native OS links:

- **One config dir per account** (`~/.claude-accounts/account-<id>`): its own
  login (`/login` once) and isolated onboarding. Claude shows the account's
  real email.
- **Shared data** (`projects`, `sessions`, `plugins`, `plans`, `todos`) and
  **user customizations** (`skills`, `commands`, `agents`, `workflows`,
  `rules`, `output-styles`, `themes`) linked into a common `~/.claude-shared`
  store via **junctions on Windows** and **symlinks on macOS/Linux** → every
  account sees and resumes **all** conversations, plugins, skills, subagents
  and rules, with no divergence.
- **Everything balances by quota**: both `houston run` and the TUI's *resume*
  probe each account's usage and pick the **least loaded** one (pressure
  weights the 5h/7d windows by how far they are from resetting). `run` first
  prints a table with each account's email and 5h/7d usage, prioritizes
  accounts that still need a login so you can set them up, and **only** pins
  you to a specific account with `-a <id>`.
- **Concurrency-safe**: each terminal carries its account in *its own*
  `CLAUDE_CONFIG_DIR`. Different terminals = different accounts at the same
  time, without stepping on each other.
- **Self-management and notices**: `houston doctor` keeps the layout healthy
  and Houston tells you when a new version is out (see below).

## Install

```powershell
# from the repo (downloads the Releases binary and verifies its SHA-256;
# falls back to building with Go):
git clone https://github.com/edgarburgues/houston
cd houston/packaging && pwsh ./Install.ps1

# or the zip: download houston-<ver>.zip from Releases (binary included)
#             unzip  →  cd houston && pwsh ./Install.ps1
```

Cross-platform (Windows / macOS / Linux), idempotent, no privileges required.
Installs the binary and puts `houston` on the PATH.

Binaries are published on [Releases](https://github.com/edgarburgues/houston/releases)
next to `checksums.txt`. The installer **verifies the SHA-256** before
installing; to check by hand: `sha256sum -c checksums.txt`. Useful options:
`./Install.ps1 -Version v0.4.0` (pin a version), `-NoProfileEdit` (don't touch
the profile or the PATH).

## Usage

```powershell
# 1) register each account (just a label; login happens later, per account):
houston account add work
houston account add personal

# 2) provision the per-account config dirs + shared links (idempotent):
pwsh ./houston-setup-accounts.ps1

# 3) launch: the first time per account, /login opens in the browser (once)
houston run                     # least-loaded account (or the one missing login)
houston run -a work2            # force a specific account
houston run --resume <id>       # any other arg is passed through to claude

# 4) browse and resume conversations (TUI):
houston
```

The installer also drops an alias into your profile so `claude` feels normal
while Houston orchestrates it: `claude ...` ≡ `houston run ...` (picks the
least-loaded account, sets its `CLAUDE_CONFIG_DIR` and launches the real
`claude`). It's a shell function, so it doesn't clash with the `claude` binary
on the PATH. To search/resume conversations, use `houston`.

### Commands
| Command | Action |
|---|---|
| `houston run` | launches the least-loaded account (or the one missing login) |
| `houston run -a <id>` | launches forcing a specific account |
| `houston` | TUI: browse, organize and resume conversations (balanced resume) |
| `houston account add <label>` | registers an account (login happens on the first `houston run`) |
| `houston account ls` | lists accounts with their email and quota pressure (5h / 7d) |
| `houston account rm <id>` | removes an account |
| `houston doctor` | audits the layout (links, logins, unshared dirs) |
| `houston doctor --fix` | repairs the layout idempotently (never clobbers data) |
| `houston doctor --resync-settings` | re-copies the shared `settings.json`/`mcp.json` into every account (settings are seeded, not linked) |
| `houston mcp add <name> ...` | adds a user-scope MCP server to **every** account (passthrough to `claude mcp add`, then propagates) |
| `houston mcp rm <name>` / `ls` | removes / lists MCP servers across accounts |
| `houston plugin add <plugin>@<mkt>` | installs once (files are shared) and enables it in **every** account |
| `houston plugin rm` / `ls` / `marketplace ...` | disables everywhere / drift table / manages shared marketplaces |
| `houston skill add <git-url\|dir>` | installs a skill into the shared store — every account sees it instantly |
| `houston skill rm <name>` / `ls` | removes / lists shared skills |
| `houston module add <path\|git-url>` | installs a Houston module (snapshot, lands **disabled**, commands printed for review) |
| `houston module enable <name>` | the consent point: the module's handlers start running (see [docs/modules.md](docs/modules.md)) |
| `houston module ls` / `rm` / `test` / `log` | inventory / uninstall / author test loop / failure log |
| `houston version` | prints the version and warns if a newer one exists |
| `houston update` | self-updates the binary from GitHub Releases (verifies SHA-256, asks first) |
| `houston update --check` | only checks; reports whether a newer version exists |

## The TUI

| Term | What it is |
|---|---|
| **Mission** | One conversation (`.jsonl`). |
| **Program** | A logical grouping of missions (`.prog` manifest). |

Shortcuts: `↑↓`/`jk` move · `tab`/`←→` pane · `/` search · `enter` **resume** ·
`*` pin · `a` archive · `t` tag · `n` note · `p`→program · `P` new · `x`
remove · `e` export · **`A` accounts** · `r` reindex · `?` help · `q` quit.

**Resume** `cd`s into the correct directory (it even resolves names with
ambiguous hyphens, dots or spaces) and launches `claude --resume` with the
chosen account — goodbye "No conversation found".

## Fleet config: MCP, plugins and skills on every account

The shared store already propagates FILES (skills, plugin installs, agents…),
but per-account CONFIG never propagated: user-scope MCP servers live in each
account's `.claude.json` and plugin enablement in each account's
`settings.json`. The `houston mcp|plugin|skill` commands close that gap:

- **`houston mcp add <name> ...`** accepts the same flags as `claude mcp add`
  — the real `claude` runs once against the first account (full validation),
  and the resulting server entry is patched surgically into every other
  account's `.claude.json` (everything else in the file is preserved).
- **`houston plugin add <plugin>@<marketplace>`** installs once (files land in
  the shared plugins dir) and enables it in every account's `settings.json`.
  Marketplaces (`houston plugin marketplace ...`) are shared, so adding one
  once is already fleet-wide.
- **`houston skill add <git-url|dir>`** installs into the shared skills dir —
  no propagation needed at all. Git installs are hardened (no `ext::`/`file://`
  transports, no option injection, shallow + timeout, symlinks never copied,
  staged installs).

`houston mcp ls` / `plugin ls` print a per-account drift table (✓/—/✗).

## Modules: extend the TUI and the statusline

A module is a reviewable directory under `~/.claude/houston/modules/<name>/`
— a `module.json` manifest plus handler scripts in any language. Houston
never loads module code in-process: every contribution is an exec with a
JSON envelope on stdin and a JSON reply on stdout, under timeouts, output
caps and ANSI stripping. Modules can add TUI key bindings, badge/hide/re-sort
the mission list, append preview sections, contribute a cached statusline
segment and override theme colors.

```powershell
houston module add ./examples/module-hello   # snapshot install — lands DISABLED
houston module test module-hello             # runs every handler against fixtures
houston module enable module-hello           # the consent point: code runs from here on
```

Installs never execute anything and always land **disabled**; `add` prints
every command the module declares so you review before enabling. The
hardening is robustness against *buggy* modules, **not** a sandbox against
hostile ones — an enabled module runs arbitrary code as your user. The
manifest reference, wire contract, authoring loop and security model live in
[docs/modules.md](docs/modules.md); [examples/module-hello](examples/module-hello)
is a copyable module touching every surface.

## Logins in a private window

Claude Code honors `$BROWSER` as the opener for every URL it launches — the
OAuth login page included. Houston sets `BROWSER` to itself for every `claude`
it starts, so **Anthropic OAuth pages open in a private window of your default
browser** (`--incognito` / `--inprivate` / `--private-window`, chosen by
detecting the system's default browser). The login never inherits the
claude.ai session already signed in in your normal browser — you pick the
right account every time, which is the whole point of multi-account.

Every other link claude opens keeps using your normal browser session.
A `BROWSER` you set yourself is respected; `HOUSTON_LOGIN_PRIVATE=off`
disables the isolation; an unrecognized default browser falls back to a
normal open. Manual use: `houston open-url <url>`.

## Self-management: `houston doctor`

The multi-account layout (shared store + one dir per account + links) can
drift over time: missing links, a real folder where a link should be, an
account without login. `houston doctor` **audits** it and `houston doctor
--fix` **repairs** it, idempotently and cross-platform (junctions on Windows
without admin, symlinks on macOS/Linux). It's the Go — and portable — version
of what `houston-setup-accounts.ps1` used to do.

It never clobbers data: if it finds a real folder **with contents** where a
link should go, it leaves it alone and tells you to merge it by hand. It
creates any missing dirs in the shared store and links the missing ones in
each account.

## Statusline (every account's quota inside Claude)

Houston renders a **usage bar per account** in Claude Code's status line (the
active one marked with `►`), colored green/amber/red by how full it is, so you
can see at a glance which account has headroom without leaving the session.
The bar tracks the **5h** window (the limit you hit first). Enable it in
`settings.json` (the shared one and/or each account's):

```json
{ "statusLine": { "type": "command", "command": "houston statusline" } }
```

It looks like this (colored in the terminal):

```
work ▕█░░░░░░░▏ 12% │ ►personal ▕████▋░░░▏ 58% │ side ▕███████▎▏ 91% │ Opus 4.8 · ctx 12%
```

Honors `NO_COLOR` (degrades to plain text). Accounts without login/usage show
as `off` with an empty bar.

The active account uses the `rate_limits` Claude pipes on stdin (live); the
others are probed and **cached ~60 s** (keeping the last good value across a
transient 429) — probes are single-flighted across concurrent sessions — so
the bar is instant and doesn't hammer the endpoint. No `jq`, no dependencies:
the binary parses the JSON itself.

## Updates

Houston checks GitHub Releases **at most once a day** (cached) and, if there's
a version newer than yours, says so on launch (`houston run`), in `houston
doctor` and in `houston version`. Local builds (`dev`) never nag.

To update, run **`houston update`**: it fetches the binary for your platform
from the latest GitHub Release, verifies its SHA-256 against the release's
`checksums.txt`, and swaps it in over the running one. It shows a pre-warning
first — **close any other terminals where `houston`/`claude` is open**
(sessions left open keep the old version until restarted) — and asks for
confirmation (`-y` skips it). On Windows the in-use binary can't be
overwritten, so it's moved aside as `houston.exe.old-<ts>` and cleaned up
automatically on a later run. Use `houston update --check` to only check, or
`--force` to reinstall the current version.

## Layout

```
houston/
├── .github/workflows/
│   └── release.yml        tag v* → cross-compiles + Release with checksums
├── packaging/
│   ├── Install.ps1        idempotent, cross-platform installer
│   ├── Uninstall.ps1      reverts the install (conversations untouched)
│   └── Build.ps1          (maintenance) cross-compiles + builds the zip
├── LICENSE                MIT
├── docs/
│   └── modules.md         module authoring guide (manifest, wire contract, security model)
├── examples/
│   └── module-hello/      example module touching every surface (pwsh + sh handlers)
├── internal/
│   ├── accounts/  usage/  launch/     accounts, quota probing, launching
│   ├── oauth/  flock/  browse/        token refresh, cross-process locking, private-window logins
│   ├── fleet/  jsonedit/              MCP/plugins/skills across accounts, surgical JSON patching
│   ├── provision/                     multi-account layout (doctor: audit/repair)
│   ├── statusline/                    status line: active account + everyone's quota
│   ├── update/                        new-version notices + self-update (GitHub Releases)
│   ├── scan/  model/  pathenc/        mission discovery and indexing (cached)
│   ├── config/  theme/  module/       user config, palette/layout theming, exec'd modules
│   ├── resume/  export/               resuming and exporting (balanced)
│   └── tui/                           interface (missions + accounts)
└── main.go · go.mod
```

## Uninstall

```powershell
pwsh packaging/Uninstall.ps1            # removes binary, profile blocks and PATH
pwsh packaging/Uninstall.ps1 -PurgeData # also deletes Houston's store
```

**Your conversations are untouched**: the shared data (`~/.claude-shared`),
the per-account logins (`~/.claude-accounts`) and the store
(`~/.claude/houston`) are kept. The script prints how to delete them by hand
if you want to.

## Build (maintenance)

```powershell
pwsh packaging/Build.ps1     # cross-compiles 6 platforms + local zip
go test ./...                # tests
```
Go ≥ 1.26, no cgo. Dependencies: Bubble Tea / Bubbles / Lip Gloss.

Publishing a version: push a `v*` tag (e.g. `git tag v0.5.0 && git push origin
v0.5.0`). The `release.yml` workflow cross-compiles the 6 platforms embedding
the tag as the version (`-X main.version=$tag`, which the update notice uses),
generates `checksums.txt` and creates the GitHub Release with everything
attached.

## License

[MIT](LICENSE) © 2026 Edgar Fernández Diéguez.
