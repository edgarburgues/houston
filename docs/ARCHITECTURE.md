# Houston 2.0 — architecture

Houston is the **Rust implementation**. The local tree also retains Go v1
for historical reference; Cargo builds the five Rust crates only. The goal, in one line:

> Be to a session manager what **Neovim** is to an editor — a small, fast,
> stable core, with an interface that is **absurdly customizable out of the
> box**: dynamic containers, movable and themeable, defined in config.

The difference from vim: you should not need a plugin ecosystem to rearrange
the UI. The layout is a first-class, config-driven tree from day one.

## Why Rust
Speed and stability: instant startup, no GC pauses on a tool that renders on
every keystroke and runs statusline probes constantly; a single static binary;
a strong type system for the concurrency (async module execs, OAuth refresh
under a lock, filesystem scans).

## The model: containers + widgets
The screen is a **tree**:

```
Node = Split { dir, children: [(Constraint, Node)] }   // a division
     | Pane(Widget)                                      // a leaf
```

- The **engine** owns the chrome: borders, titles, focus ring, resize, focus
  movement. It hands each widget only its inner rectangle.
- A **Widget** produces content for that rectangle. Built-in widgets
  (filters, missions, preview, accounts, probe, settings) and plugin-backed ones
  implement the SAME trait. There is no privileged "column" — everything is a
  pane in the tree.
- **A pane carries its own settings.** `Pane { widget, settings }` in the config,
  handed to the widget through `configure` and handed back by `settings` — which
  is not symmetry for its own sake: `to_layout` rebuilds the layout from the live
  tree on every save, including the save a border drag triggers, so a widget that
  did not return its settings would have them erased the first time the user
  resized anything. It is what makes `probe` two panes watching two hosts rather
  than a widget with a hardcoded one.
- **A pane can claim keys the router cannot carry.** `on_key` only ever receives
  printable characters, so a pane that edits text or means something by `Enter`
  says so — `editing()`, `on_enter()`, and the three edit hooks — and the app
  offers those keys to the focused pane *before* its own defaults. Without it a
  pane could advertise `↵` and never receive it, which is exactly what the
  Settings tab did until the trait grew the method.
- **The interface is clickable — mouse is first-class input.** Click focuses
  the pane under the cursor (borders included); the wheel scrolls the HOVERED
  pane, not the focused one; row-clicks select once widgets are real
  (phase 2); border-drag resize joins the config-layout work (phase 3).
  Hit-testing is just a walk of the layout tree — the engine and the renderer
  share one geometry, so they can never disagree about where a pane is.
- The **layout tree lives in config** (declarative, serde). The default
  reproduces v1's three columns in black & white; the user rewrites it freely.
  "Quota bottom-left" is a node, not a core change.

Async work (scan, module execs, OAuth, self-update) runs off the render thread
and lands as messages; a slow handler never freezes the UI (same guarantee v1
gives, enforced by the type system here).

## The component API (modules ARE the product)
Modules don't decorate Houston — they extend it through the **same component
API the built-ins use**. There is exactly one set of extension points, and the
core consumes its own API (missions list, preview, filters are "modules that
ship in the box"). The API surface a module can claim:

| Component | What it contributes |
|---|---|
| **Widget** | a pane mountable at ANY node of the layout tree (render + events + actions) |
| **Command** | a palette entry / CLI verb with its own handler |
| **Keybind** | keys, global or per-widget, bound to commands |
| **Transform** | patches over core data streams (mission rows, preview blocks) |
| **Segment** | statusline contribution — nothing may execute during a render, so a segment is a file the render reads, never a call it makes |
| **Gate** | pre-launch hook (allow/deny/ask) |
| **Theme** | colors + layout defaults |

Two panes are worth naming because they are the answer to "how do I show X":

- **`probe`** — a pane whose content is a command you declare or a file something
  else writes (`run` / `read` in its pane settings, refreshed off the render
  thread, output sanitized, age shown). "Show me the server" needs no code, and
  a command from the user's own config is admissible where one from a plugin
  manifest is not: the user already controls their configuration, while a
  manifest arrives with the plugin.
- **`settings`** — Houston's own configuration screen, on the synthetic tab reached
  with `0`. Driven by `houston-core::settings_schema`, which names, per row, WHO
  writes it: values go through `policy`, the status line through
  `claude_settings`, hooks through `hooks`, Houston's config through a request the
  app applies. One owner per key, or two subsystems fight over one file.

**Expansion is code, neovim-style.** The API is not a decoration protocol: a
plugin is a program that links against `houston-api` and drives the same
machinery the built-ins use. Bindings, in order of power:

1. **In-tree Rust** — built-ins implement the traits directly. The core is
   its own first plugin consumer; if the API can't express a built-in, the
   API is wrong.
2. **Compiled plugins (WASM)** — the neovim story, and the ONLY executable
   plugin runtime: write code (Rust, Go, C, Zig, anything that targets wasm),
   compile it, drop the `.wasm` in the plugins dir. No ABI hell, no
   recompiling Houston, cross-platform artifacts.

   The sandbox is not a policy but a property: the guest gets **no WASI**, so
   it has no files, no network and no syscalls, and therefore cannot reach the
   OAuth tokens Houston manages. It runs in its own **killable process**, so a
   runaway guest is stopped rather than left burning a core. The `capabilities`
   field in a manifest is informational — there is nothing to grant, and a field
   that looks like a permission system while enforcing nothing is worse than no
   field at all.

An `exec` runtime (JSON over stdin/stdout, for v1 script modules) was part of
this design and **was removed**. It could not be made safe: an arbitrary command
carries the user's full privileges and can read every file the user can, tokens
included. The only real containment was OS-level sandboxing — a large
per-platform project, and absent on native Windows. Deleting the runtime is less
code than sandboxing it, so nothing in Houston spawns a command from a manifest.
Scripts that genuinely need system access belong in **Claude Code's own** plugin
system, which Houston propagates across accounts (`fleet`).

Rule of thumb: **if a built-in can do it, a plugin can do it** — placement,
keys, commands, theme, data access. The config decides where everything
mounts. (`houston-api` ships as a versioned crate + wasm interface so plugin
authors build against a contract, not against Houston's internals.)

## The surface Houston does not own

Houston is not a client of a versioned API. It parses `claude agents --json`,
consumes the status line's stdin payload, installs hooks by event name and
matcher, writes keys into Claude's own `settings.json`, passes launch flags, and
decodes the `projects/<encoded-cwd>` naming scheme. None of that is a contract,
and **Claude Code updates itself in the background** — so any of it can stop
being true without anyone here acting.

What makes that a design problem rather than a maintenance chore is the failure
mode: these surfaces degrade **silently**. A renamed field under `rate_limits`
falls back to a cached number and looks like a working line. A retired hook event
installs fine and never fires, while `hooks status` still calls it ours. A changed
key in `agents --json` turns every live session into no live session, which is
indistinguishable from "nothing is running".

Two rules follow, and both are load-bearing:

1. **Take Claude's vocabularies verbatim.** `kind` and `status` are `String`, not
   enums; unknown fields are ignored, never rejected. When 2.1.229 added `cloud`
   and `offline`, that was new information rather than a parse failure. Where a
   value must be *checked*, check something Houston can verify itself — a session
   is local because it has a pid, not because its `kind` is on a list.
2. **Write down what was verified, and against which release.**
   `houston-core::compat` is that record: each assumption, how to re-check it, and
   `VERIFIED_AGAINST`. `houston compat` prints the list; `doctor` says when the
   installed binary has moved past it, distinguishing *ahead*, *behind* and *could
   not tell*. It deliberately does not probe the surfaces — a probe that guesses
   wrong is worse than a dated list, because it invites trust.

## Crates
| Crate | Role |
|---|---|
| `houston-core` | kernel: model, scan, store, accounts, config, launch, resume — pure logic, no UI |
| `houston-api` | the versioned component contract plugins build against (traits + wasm interface) |
| `houston-tui` | the layout engine: `Node`/`Widget`, focus, render; built-in widgets |
| `houston` (bin) | wires core + tui, CLI subcommands, plugin host |
| `houston-plugins` | the plugin runtime: the wasm host, in its own killable process |

## Shipped boundary

The CLI includes fleet configuration, provisioning and export alongside session
scanning, account selection, authentication, statusline and signed self-update.
Built-in and WASM widgets share the configurable container layout. WASM calls
are submitted to a bounded worker queue; rendering and input do not wait for a
guest. Geometry and selection changes request a new snapshot.

Non-interactive probes have a deadline and capped stdout/stderr. Timeout and
output overflow terminate the command tree on a best-effort basis and reap the
direct child. Interactive Claude sessions keep inherited terminal I/O.

Persistent metadata and programs use read-modify-write under filesystem locks.
An unreadable metadata file is an error, never an empty store to overwrite.
Atomic writes use exclusive, per-write temporary files. Failed layout saves
remain pending and are reported in the UI.

## Development

Run `pwsh -File packaging/Test.ps1` to isolate tests from real application and
Claude directories. `try-v2.ps1` opens a fresh disposable home without copying
accounts or credentials. The Go v1 implementation, kept in this
repository's history at tag `v1.2.1`, had an executable module system; those
modules are not supported by the Rust runtime.
