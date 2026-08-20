//! Houston 2.0 — entrypoint. Routes CLI verbs; with no verb, opens the TUI.

mod cli;
mod fleetcmd;
mod provision;

use std::time::Instant;

/// ONE lock for every test in this binary that touches `HOUSTON_HOME`.
///
/// Environment variables are process-global and cargo runs tests in parallel, so
/// a per-module mutex is not isolation — it just means two modules can each be
/// confident while removing the other's variable. houston-core learned this the
/// same way; this is the same fix for the same reason.
#[cfg(test)]
pub(crate) static TEST_ENV_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

fn main() -> anyhow::Result<()> {
    // Collect any binary a previous Windows self-update had to leave behind
    // (it was still locked then; by now it usually isn't).
    if let Ok(exe) = std::env::current_exe() {
        houston_core::update::cleanup_stale(&exe);
    }
    let args: Vec<String> = std::env::args().skip(1).collect();
    match args.first().map(String::as_str) {
        Some("scan") => cmd_scan(args.iter().any(|a| a == "--json")),
        Some("doctor") => cli::doctor(&args[1..]),
        Some("accounts") | Some("account") => cli::accounts_cmd(&args[1..]),
        Some("run") => cli::run(&args[1..]),
        Some("statusline") => cli::statusline(),
        Some("usage") => cli::usage_cmd(&args[1..]),
        Some("journal") => cli::journal_cmd(&args[1..]),
        Some("live") | Some("agents") => cli::live_cmd(&args[1..]),
        // For a shell left in raw mode with mouse capture on by a TUI that was
        // killed rather than closed: until someone puts the console back, moving
        // the pointer types escape sequences.
        Some("reset-terminal") => {
            // Only touch a real terminal: piped or captured, the escape sequences
            // would just be bytes in somebody's log.
            use std::io::IsTerminal;
            if std::io::stdout().is_terminal() {
                houston_tui::restore_terminal();
                println!("terminal restored: cooked mode, no mouse capture, no alternate screen");
            } else {
                println!("stdout is not a terminal, so there is nothing to restore");
            }
            Ok(())
        }
        Some("segment") | Some("segments") => cli::segment_cmd(&args[1..]),
        Some("hooks") => cli::hooks_cmd(&args[1..]),
        Some("compat") => cli::compat_cmd(&args[1..]),
        Some("retention") => cli::retention_cmd(&args[1..]),
        // The receiver Claude Code invokes; not meant to be run by hand.
        Some("hook") => cli::hook_cmd(&args[1..]),
        Some("export") => cli::export(&args[1..]),
        Some("update") => cli::update(&args[1..]),
        Some("keygen") => cli::keygen(&args[1..]),
        Some("sign") => cli::sign(&args[1..]),
        Some("verify") => cli::verify(&args[1..]),
        // The child half of the isolated WASM plugin host: serves one JSON call
        // per stdin line. Spawned by the TUI, not meant to be run by hand.
        Some("wasm-host") => {
            let module = args.get(1).ok_or_else(|| anyhow::anyhow!("usage: houston wasm-host <module.wasm>"))?;
            houston_plugins::serve_stdio(std::path::Path::new(module))
        }
        Some("mcp") => fleetcmd::mcp(&args[1..]),
        Some("plugin") | Some("plugins") => fleetcmd::plugin(&args[1..]),
        Some("policy") => fleetcmd::policy(&args[1..]),
        Some("provision") => provision::cmd_provision(&args[1..]),
        Some("--version") | Some("-v") => {
            println!("houston {} (v2 rewrite)", env!("CARGO_PKG_VERSION"));
            Ok(())
        }
        Some("--help") | Some("-h") => {
            print_help();
            Ok(())
        }
        // $BROWSER mode: the claude child invokes us with a URL as the only
        // argument for every link it opens. Login pages get a private window so
        // they never inherit a signed-in claude.ai session; the rest open
        // normally. Never falls through to the TUI (that would hijack the
        // child's terminal).
        Some(arg) if args.len() == 1 && houston_core::browse::is_http(&houston_core::browse::clean_url(arg)) => {
            if let Err(e) = houston_core::browse::open(arg) {
                eprintln!("houston: could not open the link: {e}");
            }
            Ok(())
        }
        _ => {
            // Opening the TUI: provision houston-basics on first run (existing
            // users get it silently; fresh installs are asked once).
            provision::ensure_provisioned();
            // A newer release? One line before the UI takes the screen. The
            // lookup is cached for 24h, so this touches the network rarely.
            if let Some(n) =
                houston_core::update::notice(env!("CARGO_PKG_VERSION"), std::time::Duration::from_secs(3))
            {
                eprintln!("houston: {n}");
            }
            houston_tui::run()
        }
    }
}

/// The front door. Written as a raw string with real newlines rather than
/// escaped continuations: the old form needed a `\` at the end of every line and
/// an escaped `\\` to indent a wrapped description, which printed a literal
/// backslash down the left margin — it read as a typo in the first thing anyone
/// sees. Grouped, because twenty flat verbs is a list you scan rather than read.
fn print_help() {
    println!(
        "houston {} — multi-account Claude session manager (v2)

usage:
  houston                            open the TUI (missions)
  houston run [-a <id>] [-- <args>]  launch claude on the best account
  houston --version

chats
  houston live [--json]              sessions running right now (pid, busy/idle, cwd)
  houston reset-terminal             undo a killed TUI's raw mode / mouse capture
  houston journal [--tail N] [--session <id>] [--json]
                                     what Houston launched or resumed, and why
  houston export <id> [out.md]       render a transcript to Markdown
  houston scan [--json]              scan transcripts (diagnostics)

accounts
  houston accounts [ls]              list accounts
  houston accounts add <label>       register + provision a new account
  houston accounts rm <id>           unregister (the config dir is kept)
  houston usage [--refresh] [--json | --pick]
                                     per-account quota; --pick explains which one resume opens

across every account
  houston policy [ls | get <key> | sync <key> --from <id> [--apply]]
                                     settings that should match everywhere (permissions, env, …)
  houston mcp [ls | add <name> … | add-json <name> <json> | rm <name>]
  houston plugin [ls | install | enable | disable | uninstall | marketplace]
  houston hooks [status | install | uninstall]
                                     let Claude tell Houston about sessions and rate limits

the status line
  houston statusline                 render it (reads Claude's JSON on stdin)
  houston segment [ls | set <name> <text> | rm <name>]
                                     extra text you write and Houston reads

maintenance
  houston doctor [--fix] [--resync-settings]
                                     audit store, accounts, config, plugins, links, hooks
  houston compat [--json]            what Houston assumes about Claude Code, and whether the
                                     installed claude has moved past the last check
  houston retention [--keep <days> | --default]
                                     how long transcripts survive; reports unless told to write
  houston provision [--minimal]      (re)write the layout preset
  houston update [--check] [--yes]   check / install a newer release
  houston keygen [dir]               create the release-signing keypair
  houston sign <file> --tag <release>        write <file>.minisig
  houston verify <file> [--tag <release>]    check it against the key",
        env!("CARGO_PKG_VERSION")
    );
}

/// `houston scan [--json]` — run the full multi-root scan and report.
fn cmd_scan(json: bool) -> anyhow::Result<()> {
    let t0 = Instant::now();
    let missions = houston_core::scan::scan_all();
    let dt = t0.elapsed();
    if json {
        serde_json::to_writer(std::io::stdout().lock(), &missions)?;
        return Ok(());
    }
    let msgs: u32 = missions.iter().map(|m| m.message_count()).sum();
    let bytes: u64 = missions.iter().map(|m| m.size_bytes).sum();
    eprintln!(
        "{} missions · {} messages · {:.1} MB · scanned in {:.2?}",
        missions.len(),
        msgs,
        bytes as f64 / (1024.0 * 1024.0),
        dt
    );
    for m in missions.iter().take(8) {
        let when = m.last_time.map(|t| t.format("%m-%d %H:%M").to_string()).unwrap_or_else(|| "     ".into());
        eprintln!("  {}  {:8}  {}", when, &m.id[..8.min(m.id.len())], m.title.chars().take(60).collect::<String>());
    }
    Ok(())
}
