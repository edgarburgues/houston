//! houston-plugins — the WASM host for Houston 2.0.
//!
//! A plugin is a plain `wasm32` module (any source language) implementing a
//! tiny ABI over linear memory — no component model, no ABI hell:
//!
//! - export `memory`
//! - export `houston_alloc(len: i32) -> i32` — a writable region for the host
//!   to put the request in.
//! - export `houston_call(ptr: i32, len: i32) -> i64` — handle the JSON
//!   `houston_api::Call` at `[ptr, ptr+len)` and return a packed
//!   `(out_ptr as u64) << 32 | out_len` pointing at the JSON `Response`.
//!
//! ## Sandbox & robustness
//! No WASI is provided, so a plugin can do NOTHING but compute on its own
//! memory — no fs, no net, no syscalls (capability grants via WASI preview2
//! are a future extension; today "no capability" holds by construction).
//!
//! Each plugin runs on its **own thread**, owning its wasmtime Store; the UI
//! talks to it over a channel and waits with a timeout. Two failure modes,
//! both survivable by the UI:
//!   - the plugin TRAPS (illegal op, OOB) → a clean `Err` from the call;
//!   - the plugin HANGS (infinite loop) → the call times out and returns
//!     `Err`; the runaway thread is abandoned but the UI never blocks.
//! We deliberately avoid wasm fuel/epoch async-interruption: its out-of-budget
//! trap aborts (non-unwinding) instead of returning cleanly on some Windows
//! toolchains. The thread+timeout model is portable and keeps the UI alive.

#![allow(clippy::doc_lazy_continuation)]
use anyhow::{anyhow, Context};
use houston_api as api;
use std::path::Path;
use std::sync::mpsc;
use std::time::Duration;
use wasmtime::{Engine, Instance, Memory, Module, Store, TypedFunc};

pub const DEFAULT_TIMEOUT: Duration = Duration::from_millis(1500);

/// Cap on ONE reply from an isolated plugin. A pane's worth of text is
/// kilobytes; this leaves room for a generous preview while making it
/// impossible for a guest to exhaust the parent's memory with one line.
const MAX_REPLY_BYTES: u64 = 4 << 20;

/// Environment variables a plugin process must NOT inherit.
///
/// Houston runs with `CLAUDE_CONFIG_DIR` pointing at an account directory —
/// which is the directory holding `.credentials.json`. Passing that down hands
/// every plugin the exact path to the OAuth tokens. It could find a conventional
/// path anyway, but there is no reason to point at it, and none of these belong
/// to a plugin in the first place.
///
/// This is a DENY list, not an allowlist, on purpose. A plugin legitimately needs
/// PATH, TEMP, locale and terminal variables; a strict allowlist would break
/// working plugins while giving false confidence, because an `exec` plugin can
/// read the filesystem regardless of its environment. What this actually buys:
/// Houston stops handing out the credential path, stops copying a release-signing
/// key into N child processes, and stops leaking whatever secrets happen to be in
/// the user's shell.
const ENV_DENY_EXACT: &[&str] = &[
    // Points at the account dir containing .credentials.json.
    "CLAUDE_CONFIG_DIR",
    // Houston's own secrets and store overrides.
    "HOUSTON_SIGNING_KEY",
    "HOUSTON_UPDATE_PUBKEY",
    "HOUSTON_HOME",
    "HOUSTON_ACCOUNTS_DIR",
    "HOUSTON_SHARED_DIR",
    // Anthropic credentials a user may have exported.
    "ANTHROPIC_API_KEY",
    "ANTHROPIC_AUTH_TOKEN",
];

/// Substrings that mark a variable as secret-shaped. Matched case-insensitively
/// against the NAME only — a plugin has no business inheriting the user's
/// unrelated API keys either.
const ENV_DENY_CONTAINS: &[&str] = &["SECRET", "PASSWORD", "PASSWD", "_TOKEN", "TOKEN_", "APIKEY", "API_KEY"];

/// Whether a variable name should be withheld from plugin processes.
pub fn env_is_denied(name: &str) -> bool {
    let upper = name.to_ascii_uppercase();
    ENV_DENY_EXACT.iter().any(|d| upper == *d) || ENV_DENY_CONTAINS.iter().any(|d| upper.contains(d))
}

/// Strip everything a plugin has no business seeing from a child's environment.
/// Used for BOTH plugin backends: the wasm host child (defence in depth — its
/// guest has no WASI and cannot read env at all) and exec plugins, where it is
/// the only thing standing between a plugin and the credential path.
pub fn scrub_env(cmd: &mut std::process::Command) {
    for (name, _) in std::env::vars_os() {
        let n = name.to_string_lossy();
        if env_is_denied(&n) {
            cmd.env_remove(&name);
        }
    }
}

/// A handle to a plugin running on its own thread. `call` is cheap and never
/// blocks longer than the timeout.
pub struct WasmHost {
    req: mpsc::Sender<api::Call>,
    resp: mpsc::Receiver<Result<api::Response, String>>,
    timeout: Duration,
}

impl WasmHost {
    /// Load a `.wasm`/`.wat` file and start its plugin thread.
    pub fn load(path: &Path, timeout: Duration) -> anyhow::Result<Self> {
        let bytes = std::fs::read(path).with_context(|| format!("reading {}", path.display()))?;
        Self::from_bytes(bytes, timeout)
    }

    /// Start a plugin from module bytes (`.wasm` binary or `.wat` text).
    pub fn from_bytes(bytes: impl Into<Vec<u8>>, timeout: Duration) -> anyhow::Result<Self> {
        let bytes = bytes.into();
        let (ready_tx, ready_rx) = mpsc::channel::<Result<(), String>>();
        let (req_tx, req_rx) = mpsc::channel::<api::Call>();
        let (resp_tx, resp_rx) = mpsc::channel::<Result<api::Response, String>>();

        std::thread::spawn(move || {
            // Build the engine/store/instance ON this thread (wasmtime types
            // are not Send, and never leave here).
            let init = (|| -> anyhow::Result<Ctx> {
                let engine = Engine::default();
                let module = Module::new(&engine, &bytes)?;
                let mut store = Store::new(&engine, ());
                let instance = Instance::new(&mut store, &module, &[])
                    .context("instantiating plugin (it must import nothing)")?;
                let memory = instance
                    .get_memory(&mut store, "memory")
                    .ok_or_else(|| anyhow!("plugin exports no `memory`"))?;
                let alloc = instance
                    .get_typed_func::<u32, u32>(&mut store, "houston_alloc")
                    .context("plugin exports no `houston_alloc(i32)->i32`")?;
                let call = instance
                    .get_typed_func::<(u32, u32), u64>(&mut store, "houston_call")
                    .context("plugin exports no `houston_call(i32,i32)->i64`")?;
                Ok(Ctx { store, memory, alloc, call })
            })();

            let mut ctx = match init {
                Ok(c) => {
                    let _ = ready_tx.send(Ok(()));
                    c
                }
                Err(e) => {
                    let _ = ready_tx.send(Err(format!("{e:#}")));
                    return;
                }
            };
            // Serve calls until the handle drops (req channel closes).
            while let Ok(call) = req_rx.recv() {
                let r = ctx.run(&call).map_err(|e| format!("{e:#}"));
                if resp_tx.send(r).is_err() {
                    break;
                }
            }
        });

        match ready_rx.recv() {
            Ok(Ok(())) => Ok(WasmHost { req: req_tx, resp: resp_rx, timeout }),
            Ok(Err(s)) => Err(anyhow!(s)),
            Err(_) => Err(anyhow!("plugin thread exited during startup")),
        }
    }

    /// Run one call, bounded by the timeout. A trap comes back as `Err`; a
    /// hang returns a timeout `Err` while the plugin thread is left behind.
    pub fn call(&self, c: &api::Call) -> anyhow::Result<api::Response> {
        // Discard any late reply from a previously timed-out call so requests
        // and responses never desync.
        while self.resp.try_recv().is_ok() {}
        self.req.send(c.clone()).map_err(|_| anyhow!("plugin thread is gone"))?;
        match self.resp.recv_timeout(self.timeout) {
            Ok(Ok(r)) => Ok(r),
            Ok(Err(s)) => Err(anyhow!(s)),
            Err(mpsc::RecvTimeoutError::Timeout) => Err(anyhow!("plugin timed out")),
            Err(mpsc::RecvTimeoutError::Disconnected) => Err(anyhow!("plugin thread is gone")),
        }
    }
}

// ------------------------------------------------- process-isolated host ----

/// A plugin running in its OWN PROCESS, one JSON message per line over
/// stdin/stdout.
///
/// Why a process and not just a thread: a guest that loops forever cannot be
/// stopped from inside. wasmtime's fuel/epoch interruption is the intended
/// answer, but on this toolchain its out-of-budget trap hits a non-unwinding
/// frame and ABORTS the whole process (verified: STATUS_STACK_BUFFER_OVERRUN),
/// which would take the UI down with it. With a thread, the call times out but
/// the runaway thread keeps a CPU core pegged for the rest of the session. A
/// child process can simply be KILLED, which is the only way to actually bound
/// a plugin's CPU. The child is persistent, so plugins keep their internal
/// state across calls, and is respawned on the next call after a kill.
pub struct ProcHost {
    inner: std::sync::Mutex<Proc>,
}

struct Proc {
    /// argv[0] and the module path used to (re)spawn the child.
    exe: std::path::PathBuf,
    module: std::path::PathBuf,
    timeout: Duration,
    live: Option<Live>,
}

struct Live {
    child: std::process::Child,
    stdin: std::process::ChildStdin,
    lines: mpsc::Receiver<String>,
}

impl ProcHost {
    /// Prepare an isolated host. `exe` is the Houston binary, re-invoked as
    /// `<exe> wasm-host <module>`. Nothing is spawned until the first call, so
    /// mounting a pane is free.
    pub fn new(exe: std::path::PathBuf, module: std::path::PathBuf, timeout: Duration) -> Self {
        ProcHost { inner: std::sync::Mutex::new(Proc { exe, module, timeout, live: None }) }
    }

    /// Run one call. A hung guest is killed (not merely abandoned) and the next
    /// call starts a fresh child.
    pub fn call(&self, c: &api::Call) -> anyhow::Result<api::Response> {
        let mut p = self.inner.lock().map_err(|_| anyhow!("plugin host poisoned"))?;
        p.call(c)
    }

    /// Whether a child is currently alive. False before the first call and
    /// after a timeout killed one — which is how "the runaway was actually
    /// stopped" becomes observable rather than assumed.
    pub fn is_running(&self) -> bool {
        self.inner.lock().map(|p| p.live.is_some()).unwrap_or(false)
    }

    /// The OS pid of the live child, if any (diagnostics).
    pub fn pid(&self) -> Option<u32> {
        self.inner.lock().ok()?.live.as_ref().map(|l| l.child.id())
    }
}

impl Proc {
    fn spawn(&mut self) -> anyhow::Result<()> {
        use std::io::{BufRead, BufReader, Read};
        use std::process::{Command, Stdio};
        let mut cmd = Command::new(&self.exe);
        cmd.arg("wasm-host")
            .arg(&self.module)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null());
        scrub_env(&mut cmd);
        let mut child =
            cmd.spawn().with_context(|| format!("spawning the plugin host for {}", self.module.display()))?;
        let stdin = child.stdin.take().ok_or_else(|| anyhow!("no stdin for the plugin host"))?;
        let stdout = child.stdout.take().ok_or_else(|| anyhow!("no stdout for the plugin host"))?;
        // A reader thread turns blocking line reads into something we can wait
        // on with a timeout. It ends when the child's stdout closes.
        let (tx, rx) = mpsc::channel::<String>();
        std::thread::spawn(move || {
            // Bounded read: a plugin's reply is a pane's worth of text, so an
            // unbounded `lines()` would let a buggy or hostile guest exhaust
            // memory in the PARENT — the one process that must survive.
            let mut r = BufReader::new(stdout).take(MAX_REPLY_BYTES);
            loop {
                let mut line = String::new();
                match r.read_line(&mut line) {
                    Ok(0) => break, // child closed stdout
                    Ok(_) => {
                        if tx.send(line.trim_end().to_string()).is_err() {
                            break;
                        }
                        // Refill the allowance for the NEXT reply; the cap is
                        // per-message, not for the child's whole lifetime.
                        r.set_limit(MAX_REPLY_BYTES);
                    }
                    Err(_) => break,
                }
            }
        });
        self.live = Some(Live { child, stdin, lines: rx });
        Ok(())
    }

    /// Kill the child and forget it; the next call respawns.
    fn kill(&mut self) {
        if let Some(mut l) = self.live.take() {
            let _ = l.child.kill();
            let _ = l.child.wait(); // reap, so no zombie is left behind
        }
    }

    fn call(&mut self, c: &api::Call) -> anyhow::Result<api::Response> {
        if self.live.is_none() {
            self.spawn()?;
        }
        let timeout = self.timeout;
        let result = (|| -> anyhow::Result<api::Response> {
            use std::io::Write;
            let live = self.live.as_mut().ok_or_else(|| anyhow!("plugin host is gone"))?;
            // Drain any late reply from a previously timed-out call so requests
            // and responses can never desync.
            while live.lines.try_recv().is_ok() {}
            let mut msg = serde_json::to_vec(c)?;
            msg.push(b'\n');
            live.stdin.write_all(&msg).context("writing to the plugin host")?;
            live.stdin.flush().context("flushing to the plugin host")?;
            match live.lines.recv_timeout(timeout) {
                Ok(line) => {
                    // The child replies either a Response or {"error": "..."};
                    // checking for the error field first keeps a plugin that
                    // legitimately has no lines from being read as a failure.
                    let v: serde_json::Value = serde_json::from_str(&line)
                        .map_err(|e| anyhow!("plugin host sent invalid JSON: {e}"))?;
                    if let Some(err) = v.get("error").and_then(|e| e.as_str()) {
                        return Err(anyhow!(err.to_string()));
                    }
                    serde_json::from_value(v).context("plugin reply was not valid houston-api JSON")
                }
                Err(mpsc::RecvTimeoutError::Timeout) => Err(anyhow!("plugin timed out")),
                Err(mpsc::RecvTimeoutError::Disconnected) => Err(anyhow!("plugin host exited")),
            }
        })();
        // A timeout means the guest is spinning: kill it so it stops burning a
        // core. A dead pipe means the child is already gone. Either way, drop it.
        if result.is_err() {
            self.kill();
        }
        result
    }
}

impl Drop for Proc {
    fn drop(&mut self) {
        self.kill();
    }
}

/// The child side of `ProcHost`: load the module and serve one JSON `Call` per
/// input line, writing one JSON reply per line. Errors are replied as
/// `{"error": "..."}` rather than exiting, so a bad call doesn't cost a respawn.
/// Returns when stdin closes.
pub fn serve_stdio(module: &Path) -> anyhow::Result<()> {
    use std::io::{BufRead, Write};
    // Reuse the in-process host: inside this child a runaway only wedges THIS
    // process, which the parent kills.
    let host = WasmHost::load(module, Duration::from_secs(3600))?;
    let stdin = std::io::stdin();
    let mut out = std::io::stdout();
    for line in stdin.lock().lines().map_while(Result::ok) {
        if line.trim().is_empty() {
            continue;
        }
        let reply = match serde_json::from_str::<api::Call>(&line) {
            Ok(call) => match host.call(&call) {
                Ok(r) => serde_json::to_string(&r)?,
                Err(e) => serde_json::to_string(&serde_json::json!({"error": format!("{e:#}")}))?,
            },
            Err(e) => serde_json::to_string(&serde_json::json!({"error": format!("bad call: {e}")}))?,
        };
        out.write_all(reply.as_bytes())?;
        out.write_all(b"\n")?;
        out.flush()?;
    }
    Ok(())
}

/// The wasmtime state, owned by the plugin thread.
struct Ctx {
    store: Store<()>,
    memory: Memory,
    alloc: TypedFunc<u32, u32>,
    call: TypedFunc<(u32, u32), u64>,
}

impl Ctx {
    fn run(&mut self, c: &api::Call) -> anyhow::Result<api::Response> {
        let bytes = serde_json::to_vec(c)?;
        let len = u32::try_from(bytes.len()).map_err(|_| anyhow!("request too large"))?;
        let ptr = self.alloc.call(&mut self.store, len).context("houston_alloc trapped")?;
        self.memory
            .write(&mut self.store, ptr as usize, &bytes)
            .context("writing request into plugin memory")?;
        let packed = self.call.call(&mut self.store, (ptr, len)).context("houston_call trapped")?;
        let out_ptr = (packed >> 32) as usize;
        let out_len = (packed & 0xffff_ffff) as usize;
        let data = self.memory.data(&self.store);
        let slice = data
            .get(out_ptr..out_ptr.saturating_add(out_len))
            .ok_or_else(|| anyhow!("plugin returned an out-of-bounds response"))?;
        serde_json::from_slice(slice).context("plugin response was not valid houston-api JSON")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn wat_guest(json: &str) -> String {
        let off = 1024u64;
        let len = json.len() as u64;
        let packed = (off << 32) | len;
        let escaped: String = json.bytes().map(|b| format!("\\{b:02x}")).collect();
        format!(
            r#"(module
  (memory (export "memory") 1)
  (global $bump (mut i32) (i32.const 8192))
  (data (i32.const {off}) "{escaped}")
  (func (export "houston_alloc") (param $len i32) (result i32)
    (local $p i32)
    global.get $bump local.set $p
    global.get $bump local.get $len i32.add global.set $bump
    local.get $p)
  (func (export "houston_call") (param $ptr i32) (param $len i32) (result i64)
    (i64.const {packed}))
)"#
        )
    }

    /// Houston runs with CLAUDE_CONFIG_DIR pointing at the directory that holds
    /// .credentials.json. Handing that to a plugin points it straight at the
    /// OAuth tokens, and the release-signing key must not be copied into every
    /// child either. This pins WHAT is withheld, because the list only protects
    /// anything as long as it is complete.
    #[test]
    fn credential_bearing_variables_are_withheld_from_plugins() {
        // The concrete exposure: the path to the credential store.
        assert!(env_is_denied("CLAUDE_CONFIG_DIR"));
        // Houston's own secrets and store overrides.
        for n in ["HOUSTON_SIGNING_KEY", "HOUSTON_HOME", "HOUSTON_ACCOUNTS_DIR", "HOUSTON_SHARED_DIR"] {
            assert!(env_is_denied(n), "{n} must not reach a plugin");
        }
        // Anthropic credentials a user may have exported.
        assert!(env_is_denied("ANTHROPIC_API_KEY"));
        assert!(env_is_denied("ANTHROPIC_AUTH_TOKEN"));
        // Secret-shaped names in general, case-insensitively.
        for n in ["GITHUB_TOKEN", "aws_secret_access_key", "MyApiKey", "DB_PASSWORD", "TOKEN_FOR_X"] {
            assert!(env_is_denied(n), "{n} looks secret and must not reach a plugin");
        }
        // And what a plugin legitimately needs is NOT withheld — an allowlist
        // here would break working plugins while buying nothing, since an exec
        // plugin can read the filesystem whatever its environment says.
        for n in ["PATH", "TEMP", "TMPDIR", "HOME", "USERPROFILE", "LANG", "TERM", "PATHEXT", "SystemRoot"] {
            assert!(!env_is_denied(n), "{n} is ordinary and should pass through");
        }
    }

    #[test]
    fn scrub_env_removes_them_from_a_real_command() {
        let mut cmd = std::process::Command::new("cmd");
        // Command::get_envs only reports EXPLICIT changes, so a removal shows up
        // as (name, None) — which is exactly what the child will not inherit.
        unsafe { std::env::set_var("HOUSTON_TEST_SECRET_TOKEN", "sensitive") };
        unsafe { std::env::set_var("CLAUDE_CONFIG_DIR", "/somewhere/with/credentials") };
        scrub_env(&mut cmd);
        let removed: Vec<String> = cmd
            .get_envs()
            .filter(|(_, v)| v.is_none())
            .map(|(k, _)| k.to_string_lossy().to_ascii_uppercase())
            .collect();
        assert!(removed.iter().any(|k| k == "CLAUDE_CONFIG_DIR"), "got {removed:?}");
        assert!(removed.iter().any(|k| k == "HOUSTON_TEST_SECRET_TOKEN"), "got {removed:?}");
        unsafe { std::env::remove_var("HOUSTON_TEST_SECRET_TOKEN") };
    }

    #[test]
    fn wasm_host_roundtrips_a_response() {
        let json = r#"{"title":"WASM!","lines":[{"spans":[{"text":"hi from wasm","bold":true}]}]}"#;
        let host = WasmHost::from_bytes(wat_guest(json), DEFAULT_TIMEOUT).unwrap();
        let resp = host
            .call(&api::Call::Render { req: api::RenderRequest { api: api::API_VERSION, ..Default::default() } })
            .unwrap();
        assert_eq!(resp.title.as_deref(), Some("WASM!"));
        assert_eq!(resp.lines[0].spans[0].text, "hi from wasm");
        assert!(resp.lines[0].spans[0].style.bold);
        // A second call on the same instance still works (thread persists).
        assert!(host.call(&api::Call::Render { req: api::RenderRequest::default() }).is_ok());
    }

    #[test]
    fn a_trap_is_an_error_not_a_crash() {
        let wat = r#"(module
  (memory (export "memory") 1)
  (func (export "houston_alloc") (param i32) (result i32) (i32.const 0))
  (func (export "houston_call") (param i32) (param i32) (result i64) (unreachable))
)"#;
        let host = WasmHost::from_bytes(wat.as_bytes().to_vec(), DEFAULT_TIMEOUT).unwrap();
        assert!(host.call(&api::Call::Render { req: api::RenderRequest::default() }).is_err());
    }

    #[test]
    fn infinite_loop_times_out_without_hanging_the_host() {
        let wat = r#"(module
  (memory (export "memory") 1)
  (func (export "houston_alloc") (param i32) (result i32) (i32.const 0))
  (func (export "houston_call") (param i32) (param i32) (result i64)
    (loop $l br $l) (i64.const 0))
)"#;
        let host = WasmHost::from_bytes(wat.as_bytes().to_vec(), Duration::from_millis(200)).unwrap();
        let t = std::time::Instant::now();
        let res = host.call(&api::Call::Render { req: api::RenderRequest::default() });
        assert!(res.is_err(), "an infinite loop must time out");
        assert!(t.elapsed() < Duration::from_secs(2), "the call must return near the timeout, not hang");
    }

    // End-to-end with the REAL compiled example plugin. Ignored by default
    // (needs the example built): run with
    //   cargo test -p houston-plugins --ignored real_compiled_plugin
    // after `cargo build --release --target wasm32-unknown-unknown` in
    // examples/plugins/wasm-clock.
    #[test]
    #[ignore]
    fn real_compiled_plugin_loads_and_renders() {
        let wasm = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../examples/plugins/wasm-clock/target/wasm32-unknown-unknown/release/wasm_clock.wasm");
        let host = WasmHost::load(&wasm, DEFAULT_TIMEOUT).expect("load compiled plugin");
        let resp = host
            .call(&api::Call::Render { req: api::RenderRequest { api: api::API_VERSION, ..Default::default() } })
            .expect("render");
        assert_eq!(resp.title.as_deref(), Some("wasm-clock"));
        assert!(resp.lines[0].spans[0].text.contains("compiled plugin"));
        assert!(resp.lines[0].spans[0].style.bold);
    }

    #[test]
    fn missing_exports_are_a_clear_error() {
        let wat = r#"(module (memory (export "memory") 1))"#;
        let res = WasmHost::from_bytes(wat.as_bytes().to_vec(), DEFAULT_TIMEOUT);
        assert!(res.is_err());
        assert!(format!("{:#}", res.err().unwrap()).contains("houston_alloc"));
    }
}
