# Houston modules — authoring guide

A module extends Houston without touching its code: a directory under
`<store>/modules/<name>/` holding a `module.json` manifest plus handler
programs in any language. Houston never loads module code in-process and
never executes anything at install time — every contribution is an exec of an
explicit argv array with a JSON envelope on stdin and a JSON reply on stdout,
under enforced timeouts, output caps and process-tree kill.

`<store>` is `~/.claude/houston` (override with `$HOUSTON_HOME`).

A module can contribute to four exec'd surfaces, plus one declarative one:

| Surface | Manifest key | Event | What it does |
|---|---|---|---|
| TUI action | `actions[]` | `action.invoke` | a key binding on the missions or accounts screen |
| Mission-list transform | `transforms.missions` | `missions.transform` | badge, retitle, hide or re-sort mission rows |
| Preview sections | `transforms.preview` | `preview.append` | append sections to the selected mission's preview pane |
| Statusline segment | `statusline` | `statusline.segment` | one cached text segment in `houston statusline` |
| Theme | `theme` | — | color/layout overrides, no handler involved |

## Quick start

The repo ships a hello-world module touching every surface
([examples/module-hello](../examples/module-hello)):

```
houston module add ./examples/module-hello   # snapshot install — lands DISABLED
houston module test module-hello             # run every handler against fixtures
houston module enable module-hello           # the consent point: code runs from here on
houston                                      # H says hello, `wip`-tagged missions get a badge
```

Read the [security model](#security-model) before enabling anything you did
not write yourself.

## Ordering

Enabled modules are always processed in **lexicographic name order**. For
mergeable contributions (theme fields, transform patches) the later-processed
module wins per field; for exclusive resources (action keys) the first
claimant wins and later claimants are dropped with a warning. The order is
reproducible from `houston module ls` alone — there is no hidden
enable-order state.

## module.json reference

A manifest using all four surfaces:

```json
{
  "api": 1,
  "name": "jira-git",
  "version": "1.2.0",
  "description": "Jira ticket badges, preview info, and a sync action",
  "actions": [
    { "id": "open-ticket", "key": "J", "title": "open Jira ticket", "screen": "missions",
      "command": ["pwsh", "-NoProfile", "-File", "open-ticket.ps1"] },
    { "id": "sync-worklog", "key": "ctrl+j", "title": "log time to Jira", "screen": "missions",
      "command": ["python", "sync_worklog.py"], "interactive": true, "refreshAfter": true }
  ],
  "transforms": {
    "missions": { "command": ["python", "badge_missions.py"], "timeoutMs": 1500 },
    "preview":  { "command": ["python", "preview_ticket.py"] }
  },
  "statusline": { "command": ["pwsh", "-NoProfile", "-File", "sprint_segment.ps1"], "ttlSeconds": 300 },
  "theme": { "colors": { "yellow": "214" } }
}
```

Unknown fields at any level are silently ignored (forward compatibility);
known fields with wrong types are hard errors.

| Field | Type | Rules |
|---|---|---|
| `api` | int | REQUIRED, must be `1`. Anything else loads as *unavailable* (`needs modules api N; try houston update`) and nothing executes |
| `name` | string | REQUIRED, must equal the directory name; see [name rules](#name-rules) |
| `version`, `description` | string | informational; description ≤ 200 runes |
| `timeoutMs` | int | optional module-wide **fallback** for handler timeouts; see [timeouts](#timeouts) |
| `actions[]` | array | ≤ 16 entries. Each: `id` (unique in the module, name charset), `key` (see [action keys](#action-keys)), `title` 1–40 runes, `screen` ∈ {`missions`, `accounts`}, `command` argv, `interactive` bool, `refreshAfter` bool, `timeoutMs` (ignored when interactive) |
| `transforms.missions`, `transforms.preview` | object | `command` argv + optional `timeoutMs` |
| `statusline` | object | one segment per module: `command`, `ttlSeconds` (default 60, clamped to [60, 3600]), `timeoutMs` (clamped to [500, 4000]) |
| `theme` | object | same shape as `config.json`'s `theme`; see [theme](#settings-and-theme) |

### Name rules

Module names (and action ids) match `^[a-z0-9][a-z0-9._-]{0,63}$`. Names
become directory names, so two extra Windows rules apply everywhere: no
trailing `.` (Windows silently drops it, so `foo.` collides with `foo`), and
the stem before the first `.` must not be a reserved device name
(`CON PRN AUX NUL COM1-9 LPT1-9` — `nul.sync` is as invalid as `NUL`). The
manifest's `name` must equal the directory it lives in.

### Action keys

`key` is what Bubble Tea's `msg.String()` delivers: a single printable rune
(case-sensitive — `J` and `j` are different keys) or `ctrl+<rune>`. Four
ctrl aliases are **rejected at validation** because the terminal normalizes
them into other keys and they could never fire: `ctrl+i` (arrives as `tab`),
`ctrl+m` (`enter`), `ctrl+[` (`esc`), `ctrl+h` (`backspace`).

Built-in keys always win. A module action whose key collides with a built-in
is dropped at startup with a warning (visible in `module ls` and `doctor`),
never intermittently routed. Reserved on the missions screen:
`q ctrl+c ? A tab up down k j left right h l / esc pgdown pgup f b enter * a
t n p P x e r` — and on the accounts screen: `q ctrl+c esc A tab up k down j
r d x enter`. Between modules, the first claimant in lexicographic name
order keeps the key.

### Command resolution

`command` is an argv array — no shell, no shebang. The elements are **never
rewritten** (a flag like `-NoProfile` is indistinguishable from a relative
path by shape). The only rule applies to `command[0]`:

- no path separator → resolved via `PATH` at exec time (`python`, `pwsh`, `sh`);
- contains a separator → must be **relative**; it is joined to the module
  directory and must stay inside it. Absolute paths and any `..` escape are
  rejected at manifest validation.

Elements `[1:]` pass through verbatim. Relative script references like
`-File open-ticket.ps1` work because the handler's working directory is
always the module directory. This rule is review hygiene — what you read in
`modules/<name>/` is what runs — **not** a security boundary: an enabled
module can exec anything at runtime.

### Timeouts

Effective timeout = the action/handler-level `timeoutMs` if set → else the
module-wide `timeoutMs` if set → else the per-surface default; the result is
then clamped to the surface's range:

| Surface | Default | Clamp |
|---|---|---|
| non-interactive action | 10 000 ms | [500, 30000] |
| missions transform | 2 000 ms | [200, 10000] |
| preview | 3 000 ms | [200, 10000] |
| statusline segment | 4 000 ms | [500, 4000] |
| interactive action | none — the user owns the terminal | n/a |

On timeout the whole process tree is killed (`taskkill /T /F` on Windows, a
process-group `SIGKILL` on POSIX) and the contribution is treated as failed.

## The wire contract

Every non-interactive exec receives one UTF-8 JSON object on stdin, then EOF:

```json
{
  "api": 1,
  "event": "action.invoke",
  "module": "jira-git",
  "houston": { "version": "v0.9.0", "os": "windows", "storeDir": "C:\\Users\\me\\.claude\\houston" },
  "settings": { "jiraUrl": "https://…" },
  "payload": { }
}
```

`settings` is the opaque passthrough of `config.json` →
`modules.<name>.settings`. Handlers must ignore unknown envelope fields;
Houston ignores unknown reply fields.

The handler's environment is the inherited one **minus `CLAUDE_CONFIG_DIR`**
(a handler spawning `claude` must not inherit the wrong account) plus:

| Variable | Value |
|---|---|
| `HOUSTON_API` | `1` |
| `HOUSTON_EVENT` | the event name — one script can serve every surface |
| `HOUSTON_MODULE` | the module name |
| `HOUSTON_MODULE_DIR` | the module directory (also the working directory) |
| `HOUSTON_STORE_DIR` | Houston's store |
| `HOUSTON_VERSION` | e.g. `v0.9.0`; `dev` on source builds |

Stdout is the reply channel and is capped — 1 MiB for actions, transforms
and previews, 8 KiB for segments; over cap is a hard failure (truncated JSON
must never half-parse). Stderr is yours for diagnostics: the last 32 KiB are
kept and logged to `modules.log` when the exec fails.

### Mission and account projections

Missions cross the wire as:

```json
{ "key": "C--Users-me-proj/0198f3…", "id": "0198f3…", "project": "C--Users-me-proj",
  "title": "Fix OAuth redirect", "cwd": "C:\\Users\\me\\proj", "gitBranch": "fix/oauth",
  "tags": ["auth"], "pinned": false, "archived": false,
  "lastTime": "2026-07-06T09:14:02Z", "userMsgs": 12, "assistantMsgs": 30 }
```

The projection excludes the conversation body and the `.jsonl` path — but
`title` falls back to the user's **first prompt** for unnamed sessions, so
mission payloads do carry conversation text (see the
[security model](#security-model)).

Accounts cross as:

```json
{ "id": "work", "label": "Work account", "configDir": "C:\\Users\\me\\.claude-accounts\\account-work",
  "lastUse": "2026-07-05T18:00:00Z" }
```

### `action.invoke`

Payload: `{ "screen": "missions", "action": "open-ticket", "mission": {…} }`
(or `"account": {…}` on the accounts screen). Reply (non-interactive):

```json
{ "status": "opened PROJ-142", "refresh": true }
```

`status` (≤ 120 runes, optional) lands in the TUI footer; `refresh` ORs with
the manifest's `refreshAfter` and triggers a rescan. `{}` and empty stdout
are valid ("done"). Interactive actions have **no reply protocol** — the
exit code is the result (see
[interactive actions](#interactive-actions-and-houston_event_file)).

### `missions.transform`

Payload is the full deduped scan, capped at the 2000 most-recent rows with
`"truncated": true` beyond:

```json
{ "generation": 17, "truncated": false, "missions": [ { "key": "…", "title": "…" }, … ] }
```

Reply — sparse patches keyed by mission `key`; unknown keys are ignored and
the first patch per key wins within one module:

```json
{ "patches": [
  { "key": "C--…/0198f3…", "badge": "PROJ-142", "title": "PROJ-142 · Fix OAuth redirect" },
  { "key": "C--…/01a002…", "hide": true },
  { "key": "C--…/019904…", "sortKey": "2026-07-09" } ] }
```

- `title` (≤ 200 runes) replaces the **display** title; search still matches
  the original — patches are presentation, not identity.
- `badge` (≤ 16 runes) renders as plain-text ` [PROJ-142]`; Houston owns the
  brackets and styling.
- `hide` removes the row from **all** list views, program views included.
- `sortKey` sorts ascending within the pinned and unpinned groups of the
  default views only — pinned-first is inviolable, and program views keep
  their curated membership order and ignore `sortKey`.

Transforms re-run when the scan set changes (startup, `r` rescan, an action
with `refresh`), not on every keystroke. On a handler failure Houston keeps
the module's previous patches for up to 3 consecutive failed generations,
then drops them.

### `preview.append`

Payload: `{ "mission": {…} }`. Reply:

```json
{ "sections": [ { "title": "Jira", "body": "PROJ-142 · In Review\nAssignee: efd\nDue: 2026-07-09" } ] }
```

At most 3 sections per module; `title` ≤ 40 runes; `body` ≤ 8 KiB of plain
text (control characters stripped, tabs → 2 spaces). Sections are fetched
when the selection settles on a mission (debounced — holding `j`/`k` does
not spawn a handler per row) and cached per mission until the next rescan.

Transform and preview replies may also carry `"notice": "≤ 120 runes"`,
shown once in the TUI footer as `[name] …`. Actions have `status` for that;
segment replies have no footer, so `notice` is ignored on both.

### `statusline.segment`

The segment surface is **machine-global by contract**: the reply is cached
in one machine-wide file (`<store>/modules-seg-cache.json`) and served to
every concurrent Claude session for up to `ttlSeconds`. The payload
therefore carries **no per-session fields** — no model, no context
percentage, no active account: whichever session refreshed the cache would
bake *its* values into every other session's statusline, the exact
cross-account confusion Houston exists to prevent. A per-session, uncached
segment variant is deferred to a future api revision.

Payload: `{}` — the envelope's `houston.storeDir` and `settings` are the
only inputs. Reply:

```json
{ "text": "sprint 12 · 3d left" }
```

`text` ≤ 80 runes, first line only, ANSI/control stripped. Empty text hides
the segment this cycle (valid). Segments render in `houston statusline`
only, appended after the account bars; the TUI does not show them. When a
refresh fails, the last good text is kept for up to 10 minutes, then the
segment silently disappears — an error string never reaches the line.

## Writing handlers

### Exit codes

`0` ok · `1` runtime failure · `2` bad input · `3` contract mismatch (e.g.
an event the handler does not know). The distinction is a documented
convention for humans reading `module test` output and logs — Houston only
enforces zero/nonzero. Any nonzero exit is a failure and **stdout is
discarded**, even if it parses: a half-run script may emit a half-truth.

### What the reply decoder tolerates

The decoder is hardened against the usual scripting traps:

- a UTF-8 BOM is stripped silently (PowerShell's default file encodings);
- UTF-16 is rejected with `reply is UTF-16, emit UTF-8` (the PowerShell 5.1
  `>` redirection trap);
- leading whitespace/CRLF is trimmed;
- **empty stdout is a valid no-op** (zero patches, no sections, hidden
  segment, generic "done");
- trailing bytes after the JSON object are tolerated (a stray `Write-Host`
  after the reply); leading garbage is rejected.

### PowerShell rules of thumb

- Always put `-NoProfile` in the command array: profiles are slow and print.
- Emit the reply with `ConvertTo-Json -Compress` (add `-Depth` for nested
  replies — the default depth mangles patch arrays) and nothing else on
  stdout.
- Never `Write-Host` on Windows PowerShell 5 — it pollutes the reply stream.
  Prefer `pwsh` (7+) and write diagnostics to stderr
  (`[Console]::Error.WriteLine(...)`).
- `pwsh` writes UTF-8 without BOM to a piped stdout by default; if you build
  the reply through files, avoid PS 5.1 `>` (UTF-16).
- Read the envelope with `[Console]::In.ReadToEnd() | ConvertFrom-Json`.

See [examples/module-hello/handler.ps1](../examples/module-hello/handler.ps1)
for a complete handler serving all four surfaces.

### Python

```python
import json, os, sys

envelope = json.load(sys.stdin)
event = os.environ["HOUSTON_EVENT"]

if event == "missions.transform":
    patches = [{"key": m["key"], "badge": "WIP"}
               for m in envelope["payload"]["missions"]
               if "wip" in (m.get("tags") or [])]
    json.dump({"patches": patches}, sys.stdout)
elif event == "statusline.segment":
    json.dump({"text": "hello"}, sys.stdout)
else:
    sys.exit(3)  # contract mismatch: an event this handler does not serve
```

Declare it as `"command": ["python", "handler.py"]` — the script path
resolves against the module directory because that is the working directory.

### Interactive actions and HOUSTON_EVENT_FILE

An action with `"interactive": true` gets the real terminal: Houston leaves
the alt-screen, hands the handler stdin/stdout/stderr and waits. Because
stdin belongs to the user, the envelope arrives in a file instead:
`$HOUSTON_EVENT_FILE` holds the absolute path of a 0600 JSON file (inside
the store, cleaned up after the run). There is no timeout, no output caps
and no reply protocol — the exit code is the result, and with
`refreshAfter` a zero exit triggers a rescan. Own the console: read input,
handle Ctrl-C, exit when done.

## The authoring loop: `houston module test`

```
houston module test <name> [--event <e>] [--live] [--mission <key>]
```

runs every contribution the manifest declares — through the exact runner
policy the real surfaces use (timeout chain, caps, hardened decoder) — and
prints, per contribution: the envelope it sent, the raw stdout/stderr, a
field-by-field verdict of the reply (unknown fields noted, caps checked,
clipping previewed) and the wall time against the resolved timeout. The
exit code is nonzero if anything fails, so module repos can run it in CI.

- Default input is a synthetic fixture: two missions (one pinned and tagged
  `wip`), one account. `--live` feeds the real scan; `--mission <key>`
  selects a specific mission (implies `--live`).
- `--event` filters to one surface: `action`, `transform`, `preview` or
  `segment` (full event names work too).
- The module only needs to exist under `modules/<name>/` — disabled and even
  unregistered modules are testable; running `module test` is itself the
  consent to execute that module's handlers once.
- Interactive actions run for real, attached to your terminal, envelope file
  included — that *is* the test.

The loop: edit the handler in `modules/<name>/` → `houston module test
<name>` → repeat → `houston module enable <name>` when it passes.

## Settings and theme

Users configure modules in `<store>/config.json` (hand-edited, read-only to
Houston; a malformed file is ignored, `houston doctor` warns):

```json
{
  "theme": { "colors": { "accent": "75" }, "layout": { "rightPercent": 45 } },
  "modules": { "jira-git": { "settings": { "jiraUrl": "https://…" } } }
}
```

`modules.<name>.settings` reaches that module's handlers verbatim in the
envelope — Houston never interprets it. Document the shape your module
expects; remember it may hold tokens (security model, point 3).

A module's `theme` contribution uses the same shape as `config.json`'s:
`colors` maps any of `accent grey dim green yellow selBg` (TUI) and
`slGreen slAmber slRed slDim slActive` (statusline) to ANSI-256 codes as
strings; `layout` takes `leftWidth`, `rightPercent`, `rightMin`. Precedence,
low to high: built-in defaults < enabled module themes in lexicographic name
order < `config.json` — installed code never overrides the user's explicit
taste. Invalid values are skipped field-wise; `houston doctor` prints the
resolved theme and which layer set each overridden field.

## When handlers fail

No module can crash the TUI, block its event loop, corrupt the terminal or
clobber a newer scan — these are robustness invariants against buggy
modules, not a sandbox (see below). Per surface:

| Failure | Effect |
|---|---|
| explicit action fails | footer: `[name] <action>: <err> (see houston module log)` |
| transform/preview fails | that module's contribution dropped (transform: previous patches kept ≤ 3 consecutive failures); other modules unaffected; footer warning once per module per session |
| segment fails | last good text kept ≤ 10 min, then the segment disappears; never an error in the line |
| broken manifest / wrong `api` / missing dir | module skipped at load with one startup warning; `ls`/doctor show the reason |
| key shadowed | action dropped, warned in `ls`/doctor |

Every failed exec appends a stanza to `<store>/logs/modules.log` — header
plus up to 8 KiB of stderr tail. Read it with `houston module log`
(`-f` follows, a name filters to one module). There is no auto-disable on
repeated failure: predictability over cleverness.

## Security model

Read this before enabling anything.

1. **Enabling a module means arbitrary code running as your user** — at TUI
   start, on every mission rescan, and on every statusline render. Review
   `modules/<name>/` before enabling; `module add` prints every declared
   command array for exactly this reason.
2. **Once any module is enabled, enablement state is advisory.** An enabled
   module can rewrite `modules.json`, other modules, `config.json`, caches,
   or the Houston binary itself. Timeouts, output caps, ANSI stripping and
   symlink rejection are robustness features, **not a sandbox**. `enable` is
   the consent point; nothing enforces it afterward.
3. **Payloads sent to modules include mission titles** — which fall back to
   the **first user prompt** for unnamed sessions — plus cwd paths and your
   `config.json` module settings (which may hold tokens). Treat module
   handlers accordingly.

Install-time hardening exists so that what you review is what runs: nothing
executes during `add`, manifests are validated before landing, git installs
are shallow hardened clones (`ext::`/`file://` transports and option
injection rejected, prompts disabled, `.git` deleted), symlinks and Windows
reparse points **fail** the install, and every install lands disabled —
`--enable` is refused for git sources.

## Managing modules

```
houston module ls                                # name, version, enabled, surfaces, shadowed keys, problems
houston module add <path|git-url> [--name n] [--enable]
houston module rm <name> [--yes]
houston module enable|disable <name>
houston module test <name> [--event e] [--live] [--mission <key>]
houston module log [-f] [<name>]
```

Installs are snapshots: `source` is recorded but never auto-refetched — to
update a module, `rm` it and `add` it again. `--name` installs under a
different name (the manifest's `name` is rewritten to match). The registry
lives in `<store>/modules.json`; a directory without an entry is
"unregistered" (flagged, never executed, never clobbered by installs), an
entry without a directory is "missing" (skipped at load). `houston doctor`
runs the full static audit: manifest health, command resolution, key
collisions, theme values, orphaned staging dirs and the resolved theme.
