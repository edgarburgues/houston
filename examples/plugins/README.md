# Houston 2.0 plugins

A plugin extends Houston through the **component API** (`houston-api`) — the
same contract the built-in widgets use. A plugin is a directory with a
`plugin.json` manifest that declares the widgets it provides, its runtime, and
the capabilities it needs.

Drop a plugin dir under `<store>/houston2/plugins/`, then reference its widget
id from your layout in `config-v2.json`:

```json
{ "type": "split", "dir": "col", "children": [
  { "size": "1fr", "type": "pane", "widget": "missions" },
  { "size": "6",   "type": "pane", "widget": "clock" }
] }
```

## The runtime

There is one: **`wasm`**. Write code in any language that targets WebAssembly,
compile it, ship the `.wasm`. Cross-platform, no ABI hell.

The isolation is a property rather than a setting: your module gets **no WASI**,
so it has no files, no network and no syscalls — it computes on its own memory and
answers the host. It also runs in its **own process**, which the host kills if it
stops answering, so an accidental infinite loop costs nothing.

A `capabilities` field exists in the manifest and is **informational**: there is
nothing to grant.

### Why there is no script runtime

An `exec` runtime existed (a script invoked per call over JSON stdin/stdout, for
migrating v1 modules) and was removed. Houston manages OAuth credentials for
several accounts, and a script runs with your full privileges: it can read every
file you can, tokens included. No manifest field changes that, and the only real
containment is OS-level sandboxing — a large per-platform project, unavailable on
native Windows. Deleting the runtime was less code than sandboxing it.

If your extension genuinely needs to run commands or touch the system, it belongs
in **Claude Code's own** plugin system (`claude plugin`), which Houston propagates
across your accounts with `houston plugin`. A manifest still asking for `exec`
loads as a visible, inert pane explaining this — never silently.

## The contract (`houston-api`)

The host sends one `Call` on the boundary and expects one `Response`:

- **Call** — `{ "call": "render", "req": {...} }` or
  `{ "call": "event", "event": {...}, "req": {...} }`.
- **RenderRequest** — `{ api, width, height, focused, selection?, settings }`
  (`selection` is the selected mission; `settings` is your config block).
- **Response** — `{ "title?": "...", "lines": [ { "spans": [ { "text": "...",
  "fg?": "cyan", "bold?": true, "dim?": true } ] } ], "effects?": [...] }`.
  `fg` is a color name or ANSI-256 index, mapped through the theme.

`wasm-clock/` is a complete minimal example. If a layout references a widget id
no plugin provides, Houston shows a visible placeholder — never a crash.

## Compiling a WASM plugin (the ABI)

A `wasm` plugin is any `wasm32` module exporting three symbols:

- `memory`
- `houston_alloc(len: i32) -> i32` — return a writable region of `len` bytes.
- `houston_call(ptr: i32, len: i32) -> i64` — read the JSON `Call` at
  `[ptr, ptr+len)`, and return a packed `(out_ptr << 32) | out_len` pointing
  at the JSON `Response` in memory.

No imports are allowed — the module is a pure sandbox (no fs/net/syscalls).
Execution is bounded by a per-call timeout, so an infinite loop can't hang
Houston. `wasm-clock/` is a complete `no_std`, dependency-free Rust example:

```
cd wasm-clock
cargo build --release --target wasm32-unknown-unknown
# → target/wasm32-unknown-unknown/release/wasm_clock.wasm
```

Then drop the `.wasm` beside a `plugin.json` with
`"runtime": { "kind": "wasm", "file": "wasm_clock.wasm" }` in the plugins dir.
