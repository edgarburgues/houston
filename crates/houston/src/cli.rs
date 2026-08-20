//! CLI subcommands for Houston 2.0: `doctor`, `accounts`, `run`, `statusline`.
//! These give parity with the v1 verbs that make Houston a daily driver. The
//! quota-aware account balancer and the multi-account live statusline bars
//! depend on the OAuth/usage subsystem (a later port); until then `run` picks
//! the least-recently-used logged-in account and `statusline` renders the
//! active account from the JSON Claude already pipes in.

use anyhow::{anyhow, Context};
use houston_core::{accounts, config::Config, paths, plugin, store::Store};
use std::io::Read;
use std::path::PathBuf;

// ------------------------------------------------------------------ doctor --

pub fn doctor(args: &[String]) -> anyhow::Result<()> {
    let do_fix = args.iter().any(|a| a == "--fix");
    let do_resync = args.iter().any(|a| a == "--resync-settings");
    println!("Houston 2.0 · doctor\n");
    println!("store: {}", paths::store_dir().display());

    let accs = accounts::load().unwrap_or_default();
    println!("\naccounts ({}):", accs.len());
    for a in &accs {
        // "logged in" only means the credential FILE exists — it says nothing
        // about whether the token still works, which is how three accounts read
        // as logged in while two of them could not authenticate at all. So the
        // age of the credential goes next to it: that is the number that predicts
        // the failure, and the failure is otherwise discovered at the worst moment.
        let logged = if a.logged_in() { "logged in" } else { "NOT logged in" };
        let age = a
            .credential_age_days()
            .map(|d| format!("cred {d}d"))
            .unwrap_or_else(|| "cred ?".into());
        let email = a.email().unwrap_or_default();
        println!("  {:<16} {:<14} {:<14} {:<9} {}", a.id, a.label, logged, age, email);
    }
    if !accs.is_empty() {
        println!("  (\"logged in\" = the credential file exists. `houston usage --refresh --force` is what proves it still works.)");
    }

    // Config: valid or falling back to default.
    let cfg_path = houston_core::config::config_path();
    match std::fs::read(&cfg_path) {
        Ok(b) => match serde_json::from_slice::<Config>(&b) {
            Ok(_) => println!("\nconfig: {} (valid)", cfg_path.display()),
            Err(e) => println!("\nconfig: {} — INVALID ({e}); using defaults", cfg_path.display()),
        },
        Err(_) => println!("\nconfig: none yet (built-in default)"),
    }

    let plugins = plugin::discover();
    println!("\nplugins ({}):", plugins.len());
    for p in &plugins {
        let rt = match &p.manifest.runtime {
            plugin::Runtime::Wasm { file } => format!("wasm:{file}"),
            plugin::Runtime::Exec { command } => format!("exec:{}", command.first().cloned().unwrap_or_default()),
        };
        let ids: Vec<&str> = p.manifest.widgets.iter().map(|w| w.id.as_str()).collect();
        // Flag what will NOT run, and why. Otherwise a plugin listed here looks
        // installed while its pane sits inert.
        let mut flags = Vec::new();
        if p.manifest.api != houston_api::API_VERSION {
            flags.push(format!("BLOCKED: targets api {}, this build implements {}", p.manifest.api, houston_api::API_VERSION));
        }
        if p.wants_removed_exec_runtime() {
            flags.push("BLOCKED: the `exec` runtime was removed — port it to wasm".into());
        }
        println!("  {:<16} api={} {:<12} widgets={:?}", p.manifest.name, p.manifest.api, rt, ids);
        if !p.manifest.capabilities.is_empty() {
            // Say plainly that this grants nothing, so nobody reads a manifest
            // as a security boundary it is not.
            println!("      declares caps={:?} (informational; nothing is granted by it)", p.manifest.capabilities);
        }
        if matches!(p.manifest.runtime, plugin::Runtime::Wasm { .. }) {
            println!("      sandboxed: no files, no network, own killable process");
        }
        for f in flags {
            println!("      {f}");
        }
    }

    // Store loads?
    match Store::load() {
        Ok(s) => println!("\nstore: {} metas · {} programs", s.meta.len(), s.programs.len()),
        Err(e) => println!("\nstore: ERROR {e}"),
    }

    // Shared data links: report every account's drift, then heal what's safe.
    use houston_core::heal;
    println!("\nshared store: {}", heal::shared_dir().display());
    let shared = heal::shared_dir();
    let mut drift = 0;
    for a in &accs {
        let Some(cd) = a.resolve_config_dir() else { continue };
        if !cd.is_dir() {
            continue;
        }
        for d in heal::SHARE_DIRS {
            let state = heal::classify(&cd.join(d), &shared.join(d));
            if state != heal::LinkState::Ok {
                println!("  account-{}/{d}: {state}", a.id);
                drift += 1;
            }
        }
    }
    if drift == 0 {
        println!("  all {} dirs linked in every account", heal::SHARE_DIRS.len());
    } else {
        let res = heal::heal(&accs);
        for line in res.relinked.iter().chain(res.notices().iter()) {
            println!("  heal: {line}");
        }
    }

    // What CLAUDE's own settings say about the two things Houston depends on:
    // the status line that displays quota, and the retention of the transcripts
    // that are the whole product.
    // Claude Code updates itself, and everything below this line reads a surface
    // it owns. Say where the binary stands relative to the last time those
    // surfaces were actually checked, because none of them fails loudly.
    let compat = houston_core::compat::check();
    println!("\nclaude code: {compat}");
    if compat.wants_attention() {
        println!("  `houston compat` lists what to re-verify");
    }
    // The one override that makes the store unreadable to Houston. Stripped from
    // every launch, but an account's own `env` block is applied by Claude and no
    // child environment can undo it — so it is reported instead.
    for (id, v) in houston_core::launch::project_dir_name_overrides(&accs) {
        println!(
            "  account-{id}: settings.json sets env.{}={v:?} — transcripts land under that name, \
             which Houston cannot decode back to a cwd, so those sessions cannot be resumed",
            houston_core::launch::PROJECT_DIR_NAME_ENV
        );
    }

    use houston_core::claude_settings as cs;
    println!("\nclaude settings:");
    for a in &accs {
        println!("  account-{:<12} {}", a.id, cs::statusline_state(a));
    }
    // Hooks: what Houston hears, and the one setting that silently stops it (and
    // the status line) without anything on screen saying so.
    for a in &accs {
        if houston_core::hooks::all_hooks_disabled(a) {
            println!("  account-{:<12} ! disableAllHooks is set — hooks AND the status line are off", a.id);
            continue;
        }
        let installed = houston_core::hooks::SPECS
            .iter()
            .filter(|s| houston_core::hooks::state_of(a, s) == houston_core::hooks::State::Ours)
            .count();
        let total = houston_core::hooks::SPECS.len();
        let note = match installed {
            0 => "no hooks (houston hooks install)".to_string(),
            n if n == total => "all hooks installed".to_string(),
            n => format!("{n}/{total} hooks installed (houston hooks status)"),
        };
        println!("  account-{:<12} {note}", a.id);
    }

    let ret = cs::retention(&accs);
    let hist = cs::history(ret.days);
    match cs::retention_notice(&ret, &hist) {
        Some(n) => println!("  {n}"),
        None if hist.files > 0 => println!(
            "  retention {} days · {} transcripts, oldest {} days — none at risk",
            ret.days, hist.files, hist.oldest_days
        ),
        None => println!("  no transcripts found to check retention against"),
    }

    // --fix builds anything missing (account dirs, seeded identity, links);
    // heal above only repairs drift in dirs that already exist.
    if do_fix {
        println!("\n--fix:");
        match heal::fix(&accs) {
            Ok(res) => {
                if res.created.is_empty() && res.skipped.is_empty() {
                    println!("  nothing to build");
                }
                for l in &res.created {
                    println!("  + {l}");
                }
                for l in &res.skipped {
                    println!("  ! {l}");
                }
            }
            Err(e) => println!("  ERROR {e}"),
        }
        // Status-line settings are part of --fix; retention deliberately is not
        // (see claude_settings: keeping or dropping history is the user's call).
        for a in &accs {
            match cs::ensure_statusline(a) {
                Ok(Some(what)) => println!("  + account-{}: {what}", a.id),
                Ok(None) => {}
                Err(e) => println!("  ! account-{}: settings.json not patched: {e}", a.id),
            }
        }
    }

    // --resync-settings propagates the shared settings/mcp into every account,
    // overwriting per-account copies (previous kept as .bak).
    if do_resync {
        println!("\n--resync-settings:");
        match heal::resync_settings(&accs) {
            Ok(res) => {
                if res.created.is_empty() && res.skipped.is_empty() {
                    println!("  every account already matches the shared seed");
                }
                for l in &res.created {
                    println!("  + {l}");
                }
                for l in &res.skipped {
                    println!("  ! {l}");
                }
            }
            Err(e) => println!("  ERROR {e}"),
        }
    }

    println!("\nAll checks ran.");
    Ok(())
}

// ---------------------------------------------------------------- accounts --

pub fn accounts_ls() -> anyhow::Result<()> {
    let accs = accounts::load().context("reading accounts")?;
    if accs.is_empty() {
        println!("(no accounts registered)");
        return Ok(());
    }
    println!("{:<16} {:<16} {:<12} LAST USE", "ID", "LABEL", "STATE");
    for a in &accs {
        let state = if a.logged_in() { "logged in" } else { "-" };
        let last = if a.last_use.is_empty() { "-" } else { &a.last_use };
        println!("{:<16} {:<16} {:<12} {}", a.id, a.label, state, last);
    }
    Ok(())
}

/// `houston accounts <add|rm|ls>` — registry mutations plus the provisioning
/// that makes a new account usable (its own config dir + seeded identity +
/// linked data dirs). Registering alone would leave an account nobody can log
/// into.
pub fn accounts_cmd(args: &[String]) -> anyhow::Result<()> {
    match args.first().map(String::as_str) {
        None | Some("ls") | Some("list") => accounts_ls(),
        Some("add") => {
            let label = args.get(1).ok_or_else(|| anyhow!("usage: houston accounts add <label>"))?;
            let a = accounts::add(label, &now_stamp()).context("registering the account")?;
            println!("added account '{}' (label: {})", a.id, a.label);
            // Build its dir, seed .claude.json with the identity stripped, copy
            // settings/mcp, and link the shared data dirs.
            let res = houston_core::heal::fix(std::slice::from_ref(&a)).context("provisioning the account dir")?;
            for line in &res.created {
                println!("  {line}");
            }
            for line in &res.skipped {
                println!("  ! {line}");
            }
            println!("\nnext: log it in once with  houston run -a {}  (then /login inside claude)", a.id);
            Ok(())
        }
        Some("rm") | Some("remove") => {
            let id = args.get(1).ok_or_else(|| anyhow!("usage: houston accounts rm <id>"))?;
            let accs = accounts::load().unwrap_or_default();
            let Some(a) = accs.iter().find(|a| &a.id == id) else {
                return Err(anyhow!("no such account: {id}"));
            };
            let dir = a.resolve_config_dir();
            accounts::remove(id).context("removing the account")?;
            println!("removed account '{id}' from the registry");
            // The config dir (its login and any real data) is deliberately left
            // on disk: deleting a credential store is not something to do as a
            // side effect. Say where it is so the user can decide.
            if let Some(d) = dir {
                if d.is_dir() {
                    println!("its config dir is left on disk: {}", d.display());
                }
            }
            Ok(())
        }
        Some(other) => Err(anyhow!("unknown accounts subcommand: {other} (ls | add <label> | rm <id>)")),
    }
}

/// `houston export <id-prefix> [out.md]` — render a transcript to Markdown.
/// The id may be any unique prefix (the short form the UI shows is enough).
pub fn export(args: &[String]) -> anyhow::Result<()> {
    let needle = args.first().ok_or_else(|| anyhow!("usage: houston export <mission-id> [out.md]"))?;
    let missions = houston_core::scan::scan_all();
    let hits: Vec<_> = missions.iter().filter(|m| m.id.starts_with(needle.as_str())).collect();
    let m = match hits.len() {
        0 => return Err(anyhow!("no mission whose id starts with {needle:?}")),
        1 => hits[0],
        n => {
            eprintln!("{n} missions match {needle:?}:");
            for m in hits.iter().take(10) {
                eprintln!("  {}  {}", &m.id[..12.min(m.id.len())], m.title);
            }
            return Err(anyhow!("ambiguous id — use a longer prefix"));
        }
    };
    let out = match args.get(1) {
        Some(p) => PathBuf::from(p),
        None => paths::store_dir().join("exports").join(houston_core::export::default_name(m)),
    };
    let p = houston_core::export::mission(m, &out).context("writing the export")?;
    // stdout stays the path alone, so it can be piped; the warning goes to
    // stderr where it can't corrupt a `$(houston export …)`.
    println!("{}", p.display());
    eprintln!("warning: {}", houston_core::export::secrets_warning(&p));
    Ok(())
}

/// `houston update [--check] [--yes]` — check GitHub Releases and, unless
/// `--check`, download the verified binary for this platform and swap it in.
pub fn update(args: &[String]) -> anyhow::Result<()> {
    use houston_core::update as up;
    let check_only = args.iter().any(|a| a == "--check" || a == "-n");
    let assume_yes = args.iter().any(|a| a == "--yes" || a == "-y");
    let allow_unsigned = args.iter().any(|a| a == "--allow-unsigned");
    let current = env!("CARGO_PKG_VERSION");

    println!("current: {current}   repo: {}", up::repo());
    if up::signing_key_configured() {
        println!("signing: releases are verified against the configured key");
    } else {
        println!("signing: NO key configured — a download can be checksum-verified but not proven genuine");
    }
    let Some(latest) = up::fetch_latest(std::time::Duration::from_secs(8)) else {
        return Err(anyhow!("could not reach GitHub Releases (offline, or no release published yet)"));
    };
    println!("latest:  {latest}");
    if !up::newer(&latest, current) {
        println!("\nalready up to date.");
        return Ok(());
    }
    if check_only {
        println!("\n{latest} is newer — run `houston update` to install it.");
        return Ok(());
    }
    // Replacing the binary the user runs daily is not something to do on a
    // guess: confirm unless they passed --yes.
    if !assume_yes {
        use std::io::{IsTerminal, Write};
        if !std::io::stdin().is_terminal() {
            return Err(anyhow!("refusing to self-update without a terminal — re-run with --yes"));
        }
        // Installing an UNSIGNED binary over the daily executable is a bigger
        // decision than a routine update; say so in the prompt itself.
        if !up::signing_key_configured() && allow_unsigned {
            println!("\nWARNING: this install cannot be proven genuine — only its checksum is verified,");
            println!("and that checksum comes from the same release as the binary.");
        }
        print!("\ninstall {latest} over {}? [y/N] ", std::env::current_exe()?.display());
        std::io::stdout().flush().ok();
        let mut line = String::new();
        std::io::stdin().read_line(&mut line)?;
        if !matches!(line.trim().to_ascii_lowercase().as_str(), "y" | "yes") {
            println!("cancelled.");
            return Ok(());
        }
    }
    println!("downloading {} …", up::asset_name());
    let (bin, file) = up::download_verified(&latest, std::time::Duration::from_secs(90), allow_unsigned)?;
    let how = if up::signing_key_configured() { "signature + checksum" } else { "checksum only" };
    println!("verified {file} ({:.1} MB) — {how}", bin.len() as f64 / (1024.0 * 1024.0));
    let exe = std::env::current_exe().context("locating the running binary")?;
    match up::swap(&exe, &bin).context("installing the new binary")? {
        Some(left) => println!("installed {latest}. A previous copy is still locked: {}", left.display()),
        None => println!("installed {latest}."),
    }
    Ok(())
}

// ------------------------------------------------------- release signing ----

/// Where the release-signing keys live by default: outside the repo, so a
/// secret key is never one `git add -A` away from being published.
fn default_key_dir() -> PathBuf {
    paths::home().join(".houston-keys")
}

/// `houston keygen [dir]` — create the release-signing keypair.
///
/// The secret key is written to disk owner-only and is NEVER printed: a key
/// echoed to a terminal ends up in scrollback, logs and transcripts. Only the
/// public half is shown, because that is the half meant to be published.
pub fn keygen(args: &[String]) -> anyhow::Result<()> {
    use houston_core::update as up;
    let dir = args.first().map(PathBuf::from).unwrap_or_else(default_key_dir);
    let (pub_path, sec_path) = (dir.join("houston.pub"), dir.join("houston.key"));

    // Never clobber an existing key: that would invalidate every release signed
    // with it and strand anyone who trusts the old public half.
    if sec_path.exists() {
        return Err(anyhow!(
            "a secret key already exists at {} — move it aside deliberately if you really want a new one \
             (every release signed with the old key stops verifying)",
            sec_path.display()
        ));
    }

    let kp = up::generate_keypair().context("generating the keypair")?;
    houston_core::export::ensure_export_dir(&dir).context("creating the key directory")?;
    write_private(&sec_path, kp.secret_key_file.as_bytes()).context("writing the secret key")?;
    std::fs::write(&pub_path, &kp.public_key_file).context("writing the public key")?;

    println!("secret key: {}   (owner-only, DO NOT commit or share)", sec_path.display());
    println!("public key: {}", pub_path.display());
    println!("\npublic key line — paste this into UPDATE_PUBKEY in crates/houston-core/src/update.rs:\n");
    println!("{}", kp.public_key_line);
    println!("\nThen sign each release's checksums.txt:");
    println!("  houston sign <path-to-checksums.txt>");
    println!("\nFor CI, put the SECRET key file's contents in a repository secret and expose it");
    println!("as $HOUSTON_SIGNING_KEY, so it is never written to a runner's disk.");
    Ok(())
}

/// `houston sign <file> --tag <release> [--key <path>]` — write `<file>.minisig`
/// next to it. The key comes from $HOUSTON_SIGNING_KEY when set (CI), else the
/// key file.
///
/// `--tag` is REQUIRED because the tag goes into the signed comment: without it
/// a signed checksum file could be lifted into a different release, and its
/// signature would still verify. The installer refuses a signature that does not
/// name the release being installed, so an untagged one is useless anyway —
/// better to fail here than at every user's machine.
pub fn sign(args: &[String]) -> anyhow::Result<()> {
    use houston_core::update as up;
    let flags = ["--key", "--tag"];
    let positional: Vec<&String> = args
        .iter()
        .enumerate()
        .filter(|(i, a)| {
            !a.starts_with("--") && !args.get(i.wrapping_sub(1)).is_some_and(|p| flags.contains(&p.as_str()))
        })
        .map(|(_, a)| a)
        .collect();
    let file = positional
        .first()
        .ok_or_else(|| anyhow!("usage: houston sign <file> --tag <release> [--key <secret-key-path>]"))?;
    let tag = args
        .iter()
        .position(|a| a == "--tag")
        .and_then(|i| args.get(i + 1))
        .ok_or_else(|| anyhow!("--tag <release> is required: the signature must name the release it authorises"))?;
    let key_arg = args.iter().position(|a| a == "--key").and_then(|i| args.get(i + 1));

    let secret = match std::env::var("HOUSTON_SIGNING_KEY") {
        Ok(v) if !v.trim().is_empty() => v,
        _ => {
            let p = key_arg.map(PathBuf::from).unwrap_or_else(|| default_key_dir().join("houston.key"));
            std::fs::read_to_string(&p)
                .with_context(|| format!("reading the secret key at {} (run `houston keygen` first)", p.display()))?
        }
    };

    let path = PathBuf::from(file);
    let bytes = std::fs::read(&path).with_context(|| format!("reading {}", path.display()))?;
    // The trusted comment records WHAT was signed and WHEN, and is itself
    // covered by the global signature.
    let stamp = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    let name = path.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
    let sig = up::sign_detached(&secret, &bytes, &format!("timestamp:{stamp} file:{name} tag:{tag}"))
        .context("signing")?;

    let out = PathBuf::from(format!("{}.minisig", path.display()));
    std::fs::write(&out, sig).with_context(|| format!("writing {}", out.display()))?;
    println!("{}", out.display());
    Ok(())
}

/// `houston verify <file> [sig] [--pubkey <line-or-file>]` — check a detached
/// signature against the trusted key. Useful before publishing a release, and
/// for a user checking a download by hand.
pub fn verify(args: &[String]) -> anyhow::Result<()> {
    use houston_core::update as up;
    let flags = ["--pubkey", "--tag"];
    let positional: Vec<&String> = args
        .iter()
        .enumerate()
        .filter(|(i, a)| {
            !a.starts_with("--") && !args.get(i.wrapping_sub(1)).is_some_and(|p| flags.contains(&p.as_str()))
        })
        .map(|(_, a)| a)
        .collect();
    let file = positional.first().ok_or_else(|| {
        anyhow!("usage: houston verify <file> [<file>.minisig] [--tag <release>] [--pubkey <line|path>]")
    })?;
    let sig_path = positional
        .get(1)
        .map(|s| PathBuf::from(s.as_str()))
        .unwrap_or_else(|| PathBuf::from(format!("{file}.minisig")));

    // A pubkey argument may be the base64 line itself or a path to a .pub file.
    let pubkey = match args.iter().position(|a| a == "--pubkey").and_then(|i| args.get(i + 1)) {
        Some(v) => std::fs::read_to_string(v).unwrap_or_else(|_| v.clone()),
        None => up::trusted_pubkey(),
    };
    if pubkey.trim().is_empty() {
        return Err(anyhow!("no trusted key: pass --pubkey, or bake UPDATE_PUBKEY in"));
    }

    let message = std::fs::read(file).with_context(|| format!("reading {file}"))?;
    let sig = std::fs::read_to_string(&sig_path).with_context(|| format!("reading {}", sig_path.display()))?;
    let comment = up::verify_minisign(&pubkey, &sig, &message)?;

    // Nothing is called OK until EVERY check has passed. This printed the OK line
    // straight after the cryptographic check and then failed on the tag below, so
    // a mismatched release read as "OK — …" followed by a contradiction. In a
    // verification command that is worse than terse: a skimmed OK is the whole
    // reason someone runs it.
    let want_tag = args.iter().position(|a| a == "--tag").and_then(|i| args.get(i + 1));
    let tag_line = match (want_tag, up::signed_tag(&comment)) {
        (Some(want), Some(got)) if &got == want => format!("tag: {got} (matches)"),
        (Some(want), Some(got)) => return Err(anyhow!("signature names release {got}, expected {want}")),
        (Some(want), None) => {
            return Err(anyhow!("signature names no release, so it cannot authorise {want} — re-sign with --tag"))
        }
        (None, Some(got)) => format!("tag: {got}"),
        (None, None) => {
            // Not fatal — verifying an unbound signature is a legitimate thing to
            // do — but `update` will refuse it, so say so where it cannot be missed.
            eprintln!("warning: this signature names no release, so `houston update` will refuse it");
            String::new()
        }
    };
    println!("OK — {file} matches {} and the trusted key", sig_path.display());
    println!("signed: {comment}");
    if !tag_line.is_empty() {
        println!("{tag_line}");
    }
    Ok(())
}

/// Write a file that only its owner can read (see export's rationale).
fn write_private(path: &std::path::Path, bytes: &[u8]) -> std::io::Result<()> {
    let mut opts = std::fs::OpenOptions::new();
    // create_new: refuse to overwrite, so a race can't clobber a key.
    opts.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    {
        use std::io::Write;
        let mut f = opts.open(path)?;
        f.write_all(bytes)?;
        f.flush()?;
    }
    #[cfg(windows)]
    {
        // Strip inherited ACLs and grant only the current user.
        use std::os::windows::process::CommandExt;
        if let Ok(user) = std::env::var("USERNAME") {
            let _ = std::process::Command::new("icacls")
                .arg(path)
                .args(["/inheritance:r", "/grant:r", &format!("{user}:F"), "/q"])
                .creation_flags(0x0800_0000)
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null())
                .status();
        }
    }
    Ok(())
}

/// A monotonic unix-seconds stamp for last-use ordering (matches the kernel's).
fn now_stamp() -> String {
    use std::time::{SystemTime, UNIX_EPOCH};
    let secs = SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0);
    format!("@{secs}")
}

// --------------------------------------------------------------------- run --

/// Pick the account to launch: the forced id if given and known; else the
/// lowest-quota-pressure logged-in account (usage::best, which falls back to the
/// cache and only then to least-recently-used). The `Pick` comes back with it so
/// the journal records WHY, in the same words `usage --pick` prints.
fn choose_account(
    accs: &[accounts::Account],
    forced: Option<&str>,
) -> Option<(accounts::Account, houston_core::usage::Pick)> {
    if let Some(id) = forced {
        return accs.iter().find(|a| a.id == id).cloned().map(|a| (a, houston_core::usage::Pick::Forced));
    }
    let logged: Vec<accounts::Account> = accs.iter().filter(|a| a.logged_in()).cloned().collect();
    // A launch can afford a live probe: unlike a resume keystroke, nothing is
    // rendering and the user is already waiting for claude to start.
    houston_core::usage::best(&logged, std::time::Duration::from_secs(8)).map(|d| (d.account, d.how))
}

pub fn run(args: &[String]) -> anyhow::Result<()> {
    // Split our flags from claude passthrough args at `--`.
    let mut forced: Option<String> = None;
    let mut passthrough: Vec<String> = Vec::new();
    let mut it = args.iter();
    while let Some(a) = it.next() {
        match a.as_str() {
            "--account" | "-a" => forced = it.next().cloned(),
            "--" => {
                passthrough.extend(it.by_ref().cloned());
                break;
            }
            other => passthrough.push(other.to_string()),
        }
    }

    let accs = accounts::load().unwrap_or_default();
    // Repair drifted data links before handing the terminal over, so the
    // session about to start sees the shared store (v1 healed at every launch).
    for n in houston_core::heal::heal(&accs).notices() {
        eprintln!("houston: {n}");
    }
    let chosen = choose_account(&accs, forced.as_deref());
    let config_dir = chosen.as_ref().and_then(|(a, _)| a.resolve_config_dir());

    match (&chosen, &forced) {
        (Some((a, how)), _) => eprintln!("houston: launching account '{}' ({})", a.id, how.explain()),
        (None, Some(id)) => return Err(anyhow!("no such account: {id}")),
        (None, None) => eprintln!("houston: no logged-in account — using Claude's default"),
    }

    // One code path for launching: it strips an inherited CLAUDE_CONFIG_DIR,
    // points it at the chosen account, stamps last-use, and injects Houston as
    // $BROWSER so OAuth logins land in a private window.
    let id = chosen.as_ref().map(|(a, _)| a.id.as_str()).unwrap_or_default();
    if let Some((a, how)) = &chosen {
        let cwd = std::env::current_dir().map(|p| p.to_string_lossy().into_owned()).unwrap_or_default();
        houston_core::launch::record_launch(&a.id, *how, &cwd);
    }
    let mut cmd = houston_core::launch::launch_command(id, config_dir.as_deref());
    cmd.args(&passthrough);
    let status = cmd.status().context("launching claude (is it on PATH?)")?;
    std::process::exit(status.code().unwrap_or(1));
}

// -------------------------------------------------------------- statusline --

pub fn statusline() -> anyhow::Result<()> {
    let mut raw = String::new();
    let _ = std::io::stdin().read_to_string(&mut raw);
    let v: serde_json::Value = serde_json::from_str(&raw).unwrap_or(serde_json::Value::Null);

    let model = v.get("model").and_then(|m| m.get("display_name")).and_then(|s| s.as_str()).unwrap_or("claude");
    let ctx = v.get("context_window").and_then(|c| c.get("used_percentage")).and_then(|n| n.as_f64());
    // Both windows and both reset times, for the account THIS session is signed
    // in as — free, exact, and newer than any cache a render is allowed to read.
    let live = houston_core::usage::LiveWindows::from_statusline_json(&v);

    let accs = accounts::load().unwrap_or_default();
    // Piggyback the render cadence to self-heal the shared data links: the
    // statusline runs every few hundred ms while ANY session is open, so a link
    // that drifts mid-session is repaired within seconds — before the running
    // claude writes a plan into a trapped real dir. The healthy path is a
    // handful of lstats per account; notices are doctor's business, not the
    // line's, so the result is dropped.
    let _ = houston_core::heal::heal(&accs);
    let active = active_account_id(&accs);

    // No Houston accounts: just the active session's basic line.
    if accs.is_empty() {
        let mut parts = vec![model.to_string()];
        if let Some(c) = ctx {
            parts.push(format!("ctx {c:.0}%"));
        }
        if let Some(p) = live.u5 {
            parts.push(format!("5h {}", bar(p)));
        }
        println!("{}", parts.join(" · "));
        return Ok(());
    }

    // ONE line (v1's layout): a usage bar per account joined by " │ ", the
    // active one marked ▸ and overridden with the live 5h figure Claude pipes
    // in, then model · ctx. Colored bars/markers only under a styled theme
    // (basics); the plain core theme stays monochrome. Never stacked.
    let theme_cfg = Config::load();
    let theme = &theme_cfg.theme;
    let styled = std::env::var_os("NO_COLOR").is_none() && theme.accent.bytes().all(|b| b.is_ascii_digit()) && !theme.accent.is_empty();
    let sep = if styled { format!("{} │ {}", sl_fg("240"), SL_RESET) } else { " │ ".into() };

    // A PURE READ (never a probe). Claude debounces the status line at 300 ms and
    // cancels a script that is still running when the next update arrives, so the
    // one render where the cache expires is exactly the render that would be
    // killed — the line would blink out precisely when the number changes.
    // Reading can't block; keeping the number current is the detached
    // refresher's job below, and `refreshInterval` guarantees a later render
    // arrives to display it even in a session nobody is typing in.
    let util = houston_core::usage::read_utilization(&accs);
    let mut segs: Vec<String> = Vec::new();
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
    for mut u in util {
        let id = u.id.clone();
        let is_active = active.as_deref() == Some(id.as_str());
        // Only the active account can be told about from stdin — those figures
        // are the session's own. Every other account is knowable only from the
        // cache, which is exactly why the refresher below exists.
        if is_active {
            u.overlay(&live);
        }
        let (u5, u7, ok) = (u.u5, u.u7, u.ok);
        // The 5h window is the one you work against, so it is the default. The
        // weekly takes over only once it is SATURATED — then IT is why the
        // account cannot serve and why the launcher skips it, and a "0%" 5h bar
        // would read as "totally free" for the one account that is out. Same
        // threshold as the launcher, so display and behaviour cannot disagree.
        let weekly = u7 >= houston_core::usage::SATURATED;
        let (pct, tag) = if weekly { (u7, "7d") } else { (u5, "") };
        let label = if is_active && styled {
            format!("{}{}▸{id}{}", sl_fg(&theme.accent), SL_BOLD, SL_RESET)
        } else if is_active {
            format!("▸{id}")
        } else {
            id.clone()
        };
        // "When does it come back?" is the only useful thing to say about an
        // account with no room, so the countdown is shown exactly there.
        let renew = if ok && pct >= houston_core::usage::SATURATED {
            houston_core::usage::resets_in_label(u.resets_in(weekly, now))
        } else {
            None
        };
        segs.push(format!("{label} {}", account_value(pct, tag, renew.as_deref(), ok, u.needs_login(), styled)));
    }

    // Segments other processes wrote (Decision 6). Read and sanitized, never
    // executed: a render that ran a plugin is a render Claude can cancel, and a
    // segment that could emit ANSI could forge the fields above it.
    for s in houston_core::segments::read_all(&theme_cfg.segment_specs()) {
        segs.push(if styled { format!("{}{s}{}", sl_fg("245"), SL_RESET) } else { s });
    }

    let mut meta = model.to_string();
    if let Some(c) = ctx {
        meta.push_str(&format!(" · ctx {c:.0}%"));
    }
    if styled {
        meta = format!("{}{meta}{}", sl_fg("240"), SL_RESET);
    }
    segs.push(meta);

    println!("{}", segs.join(&sep));

    // Printed first, refreshed after: whatever this costs happens with the line
    // already on Claude's screen. Throttled across processes so a stale cache
    // costs one refresher, not one per render per session.
    if houston_core::usage::claim_background_refresh(&accs, STATUSLINE_TTL) {
        houston_core::usage::spawn_background_refresh(false);
    }
    Ok(())
}

/// How old a cached quota value may get before a refresh is started. Matched by
/// the `refreshInterval` Houston writes into `statusLine` (see
/// `claude_settings`): the cache expiring with no render due would leave the
/// number stale until the user typed.
const STATUSLINE_TTL: std::time::Duration = std::time::Duration::from_secs(60);

// ------------------------------------------------------------------- usage --

/// `houston usage [--refresh] [--json] [--pick]` — the quota the statusline
/// draws, as text, plus the refresh half that the statusline itself must not
/// run. `--refresh` is what the detached refresher invokes; `--pick` answers
/// "why did it send me to that account?" without reading the source.
pub fn usage_cmd(args: &[String]) -> anyhow::Result<()> {
    // --force ignores the cache TTL. The rate-limit hook uses it: the cache may
    // be seconds old and is now known to be wrong. It IMPLIES --refresh, because
    // "force" with nothing to force is a silent no-op nobody expects.
    let force = args.iter().any(|a| a == "--force");
    let do_refresh = force || args.iter().any(|a| a == "--refresh");
    let as_json = args.iter().any(|a| a == "--json");
    let do_pick = args.iter().any(|a| a == "--pick");
    let accs = accounts::load().unwrap_or_default();
    if accs.is_empty() {
        return Err(anyhow!("no accounts registered"));
    }
    if do_pick {
        return explain_pick(&accs);
    }
    if do_refresh {
        // 6s: nothing is waiting on this, and a probe that gives up too early
        // leaves the account reading as "off" for a whole interval.
        let ttl = if force { std::time::Duration::ZERO } else { STATUSLINE_TTL };
        houston_core::usage::refresh(&accs, ttl, std::time::Duration::from_secs(6));
    }
    let util = houston_core::usage::read_utilization(&accs);
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
    if as_json {
        let rows: Vec<serde_json::Value> = util
            .iter()
            .map(|u| {
                serde_json::json!({
                    "id": u.id, "ok": u.ok, "u5": u.u5, "u7": u.u7,
                    "reset5": u.reset5, "reset7": u.reset7,
                })
            })
            .collect();
        println!("{}", serde_json::to_string_pretty(&rows)?);
        return Ok(());
    }
    println!("{:<16} {:>6} {:>6}  RESETS", "ID", "5H", "7D");
    for u in &util {
        if !u.ok {
            // The REASON, not just the absence. "logged out or unreachable" made
            // the reader guess between two very different remedies; the probe
            // knew which and the cache was throwing it away.
            let why = if u.err.is_empty() { "no reading yet — run `houston usage --refresh`".into() } else { u.err.clone() };
            println!("{:<16} {:>6} {:>6}  {why}", u.id, "-", "-");
            if u.err.contains("re-login") || u.err.contains("not logged in") {
                println!("{:<16} {:>14}  → houston run -a {} …then /login inside claude", "", "", u.id);
            }
            continue;
        }
        let weekly = u.u7 >= houston_core::usage::SATURATED;
        let renew = houston_core::usage::resets_in_label(u.resets_in(weekly, now))
            .map(|s| format!("{s} ({})", if weekly { "7d" } else { "5h" }))
            .unwrap_or_else(|| "-".into());
        println!("{:<16} {:>5.0}% {:>5.0}%  {renew}", u.id, u.u5, u.u7);
    }
    if !do_refresh && houston_core::usage::is_stale(&accs, STATUSLINE_TTL) {
        println!("\n(cached values are past their TTL; `houston usage --refresh` re-probes)");
    }
    Ok(())
}

// ------------------------------------------------------------------- hooks --

/// `houston hook <verb>` — the receiver Claude Code invokes.
///
/// Runs INSIDE somebody's session, which sets three hard rules:
///
/// - **Print nothing.** A hook's stdout is fed back to Claude for some events
///   (`SessionStart` can inject context), so a stray line would end up in the
///   conversation.
/// - **Always succeed.** A non-zero exit is a signal to Claude, not just to us; a
///   Houston that cannot journal must not become the user's problem mid-session.
/// - **Be quick.** One append, then out.
pub fn hook_cmd(args: &[String]) -> anyhow::Result<()> {
    let verb = args.first().cloned().unwrap_or_default();
    if verb.is_empty() {
        // Journalling a nameless event would litter an append-only file with a
        // line `read_all` then filters out forever.
        return Ok(());
    }
    let mut raw = String::new();
    let _ = std::io::stdin().read_to_string(&mut raw);
    let v: serde_json::Value = serde_json::from_str(&raw).unwrap_or(serde_json::Value::Null);
    let get = |k: &str| v.get(k).and_then(|x| x.as_str()).unwrap_or_default().to_string();

    // Whichever of these the event carries: SessionStart's source, SessionEnd's
    // reason, StopFailure's error. Capped, because `error` can be long and the
    // journal is a log of facts rather than of prose.
    let detail: String = [get("source"), get("reason"), get("error")]
        .into_iter()
        .find(|s| !s.is_empty())
        .unwrap_or_default()
        .chars()
        .take(120)
        .collect();

    houston_core::journal::append(&houston_core::journal::Entry {
        session: get("session_id"),
        cwd: get("cwd"),
        // The hook runs with CLAUDE_CONFIG_DIR set, so the event knows whose it is.
        account: houston_core::accounts::active_id().unwrap_or_default(),
        detail,
        ..houston_core::journal::Entry::now(&verb)
    });

    // The one event that changes behaviour rather than just history: getting
    // rate-limited means the cached quota is now wrong, and waiting up to 60s for
    // the next poll is how an exhausted account keeps looking free. Re-probe for
    // the TRUTH rather than inventing a 100% — detached, so this hook still
    // returns immediately.
    // Two events make the cached quota known-wrong rather than merely old, and
    // both are worth a forced re-probe: hitting a rate limit (the account is out,
    // and a 60s-stale cache would keep offering it) and finishing a login (the
    // account was reading as off and is now usable).
    if verb == houston_core::journal::EVENT_RATE_LIMIT || verb == houston_core::journal::EVENT_AUTH_OK {
        houston_core::usage::spawn_background_refresh(true);
    }
    // A chat just opened, so the live snapshot is known to be out of date. This is
    // the honest use of the event: it says "measure again", not "here is the new
    // state" — the payload has no pid, and there is no matching end event, so
    // deriving liveness from events alone would mark every chat ever opened as
    // live, forever.
    if verb == houston_core::journal::EVENT_SESSION_START {
        houston_core::agents::spawn_background_refresh();
    }
    Ok(())
}

// -------------------------------------------------------------- retention --

/// `houston retention [--keep <days> | --default]` — how long the history
/// survives, and the one place Houston changes it from.
///
/// With no arguments it **only reports**. Writing requires an explicit `--keep`
/// or `--default`, because lowering retention deletes conversations and that must
/// never be the side effect of a half-typed verb (Phase 0.3).
///
/// Exists besides the Settings tab's row because that row cycles between "keep
/// forever" and Claude's default, and this needs to take any number.
pub fn retention_cmd(args: &[String]) -> anyhow::Result<()> {
    use houston_core::claude_settings as cs;
    let accs = accounts::load().unwrap_or_default();

    let keep = args.iter().position(|a| a == "--keep").and_then(|i| args.get(i + 1));
    let to_default = args.iter().any(|a| a == "--default");

    // Report first, always: see what is being acted on before acting.
    let before = cs::retention(&accs);
    let hist = cs::history(before.days);
    println!("now: {} days · {} conversations, oldest {}", before.days, hist.files, hist.oldest_days);
    for root in &hist.roots {
        println!(
            "  {:<60} {} conversations · oldest {}d · newest {}d · {} past the limit",
            root.path, root.files, root.oldest_days, root.newest_days, root.at_risk
        );
    }
    if let Some(n) = cs::retention_notice(&before, &hist) {
        println!("\n{n}");
    }
    if keep.is_none() && !to_default {
        println!("\nNothing changed. `--keep <days>` sets it, `--default` returns to Claude's {}.", cs::DEFAULT_CLEANUP_DAYS);
        return Ok(());
    }
    if keep.is_some() && to_default {
        return Err(anyhow!("--keep and --default ask for opposite things"));
    }

    let target = match keep {
        Some(s) => Some(s.parse::<i64>().map_err(|_| anyhow!("--keep wants a number of days, not {s:?}"))?),
        None => None,
    };
    println!("\nwriting every config dir (accounts + ~/.claude when it holds transcripts):");
    for (place, res) in cs::set_cleanup_period(&accs, target) {
        match res {
            Ok(what) => println!("  {place:<24} {what}"),
            Err(e) => println!("  {place:<24} ERROR {e}"),
        }
    }

    // Re-read, never assume: the after-report comes from disk, so a write that
    // did not stick shows up here instead of in the next session.
    let after = cs::retention(&accs);
    let hist = cs::history(after.days);
    println!("\nnow: {} days · {} of {} conversations past the limit", after.days, hist.at_risk, hist.files);
    if after.days != target.unwrap_or(cs::DEFAULT_CLEANUP_DAYS) {
        println!("NOTE: the effective period is NOT what was asked — the strictest config dir governs.");
    }
    Ok(())
}

// ---------------------------------------------------------------- compat --

/// `houston compat [--json]` — the assumptions Houston makes about Claude Code,
/// and whether the installed binary has moved past the release they were checked
/// on.
///
/// Exists as its own verb, not just a `doctor` section, because the useful
/// output is the LIST: after an update you want the walkable set of things to
/// re-verify, and doctor's job is to tell you that you need it.
pub fn compat_cmd(args: &[String]) -> anyhow::Result<()> {
    use houston_core::compat;
    let json = args.iter().any(|a| a == "--json");
    let rep = compat::check();

    if json {
        let out = serde_json::json!({
            "verifiedAgainst": compat::VERIFIED_AGAINST,
            "installed": rep.installed.map(|v| v.to_string()),
            "drift": format!("{:?}", rep.drift).to_lowercase(),
            "wantsAttention": rep.wants_attention(),
            "assumptions": compat::ASSUMPTIONS
                .iter()
                .map(|a| serde_json::json!({ "surface": a.surface, "assumption": a.assumption, "recheck": a.recheck }))
                .collect::<Vec<_>>(),
        });
        serde_json::to_writer_pretty(std::io::stdout().lock(), &out)?;
        println!();
        return Ok(());
    }

    println!("{rep}\n");
    for a in compat::ASSUMPTIONS {
        println!("  {}", a.surface);
        println!("      assumes:  {}", a.assumption);
        println!("      recheck:  {}", a.recheck);
    }
    if rep.wants_attention() {
        // The point of the whole module, said where it lands: these do not fail
        // loudly, so nothing on screen will prompt you to look.
        println!("\nNone of these surfaces errors when it changes — they degrade quietly.");
        println!("Re-walk the list, then bump compat::VERIFIED_AGAINST to record that you did.");
    }
    Ok(())
}

/// `houston hooks [status | install | uninstall]` — Houston's hooks across ALL
/// accounts, like `mcp` and `plugin`.
pub fn hooks_cmd(args: &[String]) -> anyhow::Result<()> {
    use houston_core::hooks;
    let accs = accounts::load().unwrap_or_default();
    if accs.is_empty() {
        return Err(anyhow!("no accounts registered"));
    }
    match args.first().map(String::as_str) {
        Some("install") => {
            let rep = hooks::install(&accs);
            report(&rep);
            println!("\nHouston now hears about chats opening, accounts running out, and logins.");
            // Say how to confirm it, because a print-mode session will NOT fire
            // these — only an interactive one does, and that is worth knowing
            // before concluding it is broken.
            println!("Open a chat, then `houston journal --tail 5` — a `session-start` line means it works.");
            println!("(`claude -p …` does not run these hooks at all, so it proves nothing either way.)");
            println!("`houston hooks uninstall` removes every hook Houston ever wrote.");
            Ok(())
        }
        Some("uninstall") | Some("remove") => {
            let rep = hooks::uninstall(&accs);
            report(&rep);
            Ok(())
        }
        None | Some("status") | Some("ls") => {
            for a in &accs {
                println!("account-{}:", a.id);
                if hooks::all_hooks_disabled(a) {
                    // Loud, because it also kills the status line and nothing on
                    // screen would explain either absence.
                    println!("  ! disableAllHooks is set — hooks AND the custom status line are off");
                }
                for spec in hooks::SPECS {
                    let name = format!("{}{}", spec.event, spec.matcher.map(|m| format!("/{m}")).unwrap_or_default());
                    let state = match hooks::state_of(a, spec) {
                        hooks::State::Ours => "installed".to_string(),
                        hooks::State::Stale(c) => format!("older command `{c}` (install rewrites it)"),
                        hooks::State::Missing => "not installed".to_string(),
                        hooks::State::Foreign(n) => format!("{n} hook(s) here, none of them Houston's"),
                    };
                    println!("  {name:<38} {state}");
                }
            }
            println!("\nwhat each is for:");
            for spec in hooks::SPECS {
                println!("  {:<20} {}", spec.verb, spec.why);
            }
            println!("\nnot subscribed on purpose: PreToolUse / PostToolUse / PermissionRequest —");
            println!("they fire per tool call, which would mean a Houston process per tool call.");
            Ok(())
        }
        Some(other) => Err(anyhow!("unknown: houston hooks {other} (try status | install | uninstall)")),
    }
}

fn report(rep: &houston_core::hooks::Report) {
    if rep.changed.is_empty() && rep.left_alone.is_empty() {
        println!("nothing to change");
    }
    for l in &rep.changed {
        println!("+ {l}");
    }
    for l in &rep.left_alone {
        println!("! {l}");
    }
}

// ---------------------------------------------------------------- segments --

/// `houston segment [ls | set <name> <text> | rm <name>]` — the producer side of
/// Decision 6, so a shell script or a bus server can contribute to the status
/// line without Houston ever executing anything at render time.
pub fn segment_cmd(args: &[String]) -> anyhow::Result<()> {
    use houston_core::segments;
    match args.first().map(String::as_str) {
        Some("set") => {
            let name = args.get(1).ok_or_else(|| anyhow!("usage: houston segment set <name> <text>"))?;
            // Everything after the name is the text, so it needs no quoting.
            let text = args[2..].join(" ");
            if text.trim().is_empty() {
                // An empty set would write an empty file, which reads as "present
                // but says nothing" — indistinguishable from a broken producer.
                return Err(anyhow!("nothing to set. To remove it: houston segment rm {name}"));
            }
            segments::write(name, &text).with_context(|| format!("writing segment {name}"))?;
            let shown = segments::read(&houston_core::segments::Spec {
                name: name.clone(),
                ttl: None,
                max_chars: segments::DEFAULT_MAX_CHARS,
            })
            .unwrap_or_default();
            println!("{name} = {shown:?}");
            if shown != text.trim() {
                // Say it plainly rather than letting a silently-cleaned value
                // look like a Houston bug later.
                println!("(sanitized: escape sequences, control characters and overlong text are removed)");
            }
            print_segment_config_hint(name);
            Ok(())
        }
        Some("rm") | Some("remove") => {
            let name = args.get(1).ok_or_else(|| anyhow!("usage: houston segment rm <name>"))?;
            let path = segments::path_of(name);
            match std::fs::remove_file(&path) {
                Ok(()) => println!("removed {}", path.display()),
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => println!("no such segment: {name}"),
                Err(e) => return Err(e).context("removing the segment"),
            }
            Ok(())
        }
        None | Some("ls") | Some("list") => {
            let cfg = Config::load();
            let specs = cfg.segment_specs();
            println!("segments dir: {}", segments::dir().display());
            if specs.is_empty() {
                println!("\n(no segments configured — nothing extra is read)");
            } else {
                println!("\nconfigured, in the order they render:");
                for s in &specs {
                    let state = match segments::read(s) {
                        Some(text) => format!("{text:?}"),
                        None if segments::path_of(&s.name).exists() => "(present but empty or expired)".into(),
                        None => "(no file yet)".into(),
                    };
                    let ttl = s.ttl.map(|t| format!("{}s", t.as_secs())).unwrap_or_else(|| "no ttl".into());
                    println!("  {:<16} {ttl:<8} {state}", s.name);
                }
            }
            // Files nobody asked for are easy to forget about, so name them.
            let mut orphans: Vec<String> = Vec::new();
            if let Ok(rd) = std::fs::read_dir(segments::dir()) {
                for e in rd.flatten() {
                    let p = e.path();
                    if p.extension().and_then(|x| x.to_str()) != Some("txt") {
                        continue;
                    }
                    let stem = p.file_stem().unwrap_or_default().to_string_lossy().into_owned();
                    if !specs.iter().any(|s| s.name == stem) {
                        orphans.push(stem);
                    }
                }
            }
            if !orphans.is_empty() {
                println!("\nwritten but NOT configured (so not shown): {}", orphans.join(", "));
                print_segment_config_hint(&orphans[0]);
            }
            Ok(())
        }
        Some(other) => Err(anyhow!("unknown: houston segment {other} (try ls | set | rm)")),
    }
}

fn print_segment_config_hint(name: &str) {
    // snake_case, matching the rest of config-v2.json (`sel_fg`, `border_focus`).
    println!(
        "\nto show it, add to {}:\n  \"segments\": [ {{ \"name\": \"{name}\", \"ttl_secs\": 120 }} ]",
        houston_core::config::config_path().display()
    );
}

// -------------------------------------------------------------------- live --

/// `houston live [--json] [--refresh]` — the sessions running right now.
///
/// Reads the cached snapshot by default (instant) and says how old it is; asking
/// claude costs ~1.2 s, so it happens on `--refresh`, when there is no cache yet,
/// or in the background. `--refresh` is also what the `session-start` hook spawns.
pub fn live_cmd(args: &[String]) -> anyhow::Result<()> {
    let as_json = args.iter().any(|a| a == "--json");
    let force = args.iter().any(|a| a == "--refresh");
    let (cached, age) = houston_core::agents::read_cached();
    // No cache yet means a first run, and answering "nothing is live" from an
    // absent cache would be a lie rather than a stale truth.
    let measured = force || age.is_none();
    let live = if measured { houston_core::agents::refresh(houston_core::agents::DEFAULT_TIMEOUT) } else { cached };
    if as_json {
        println!("{}", serde_json::to_string_pretty(&live)?);
        return Ok(());
    }
    if live.is_empty() {
        // Distinguish "nothing running" from "could not tell", because the
        // second one is a Houston problem and the first one is not.
        println!("(no live sessions — or claude could not be asked; `houston live --json` shows the raw answer)");
        return Ok(());
    }
    if !measured {
        println!("as of {}s ago · --refresh re-asks claude (~1.2s)\n", age.unwrap_or(0));
    }
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
    println!("{:<8} {:<7} {:<13} {:>7}  NAME / SESSION", "PID", "STATUS", "KIND", "UPTIME");
    for l in &live {
        let up = houston_core::usage::resets_in_label(Some(l.uptime_secs(now))).unwrap_or_else(|| "0m".into());
        let who = if l.name.is_empty() { l.session_id.clone() } else { format!("{} · {}", l.name, l.session_id) };
        println!("{:<8} {:<7} {:<13} {up:>7}  {who}", l.pid, l.status, l.kind);
        if !l.cwd.is_empty() {
            println!("{:38}  {}", "", l.cwd);
        }
    }
    Ok(())
}

// ----------------------------------------------------------------- journal --

/// `houston journal [--tail N] [--session <id>] [--json]` — what Houston did and
/// when. The point of the plain output is that "which account opened this, and
/// why" is readable at a glance; `--json` is for anything that wants to process
/// it.
pub fn journal_cmd(args: &[String]) -> anyhow::Result<()> {
    let mut n = 20usize;
    let mut session: Option<String> = None;
    let as_json = args.iter().any(|a| a == "--json");
    let mut it = args.iter();
    while let Some(a) = it.next() {
        match a.as_str() {
            "--tail" | "-n" => {
                if let Some(v) = it.next() {
                    n = v.parse().map_err(|_| anyhow!("--tail wants a number, got {v:?}"))?;
                }
            }
            "--session" | "-s" => session = it.next().cloned(),
            _ => {}
        }
    }

    let mut rows = houston_core::journal::read_all();
    if let Some(s) = &session {
        rows.retain(|e| e.session == *s);
    }
    let start = rows.len().saturating_sub(n);
    let rows = &rows[start..];

    if as_json {
        println!("{}", serde_json::to_string_pretty(rows)?);
        return Ok(());
    }
    if rows.is_empty() {
        println!("(nothing recorded yet — the journal fills as Houston launches and resumes sessions)");
        return Ok(());
    }
    println!("{:<19} {:<14} {:<16} {:<18} SESSION", "WHEN", "EVENT", "ACCOUNT", "WHY");
    for e in rows {
        let when = e.when_local();
        let session = if e.session.is_empty() {
            // Say why it is missing rather than leaving a hole: a new session has
            // no id until claude creates it.
            "(new)".to_string()
        } else {
            e.session.clone()
        };
        let acc = if e.account.is_empty() { "-" } else { &e.account };
        // `reason` (why THIS ACCOUNT) and `detail` (what the event itself said) are
        // separate fields for a reason, but they never both apply to one line, so
        // one column shows whichever there is.
        let why = match (e.reason.is_empty(), e.detail.is_empty()) {
            (false, _) => e.reason.clone(),
            (true, false) => e.detail.clone(),
            _ => "-".to_string(),
        };
        println!("{when:<19} {:<14} {acc:<16} {why:<18} {session}", e.event);
    }
    Ok(())
}

/// Show which account resume would open, and the numbers behind it. Reads the
/// cache exactly as resume does — no probe — so what it prints is the decision
/// itself and not a second opinion.
fn explain_pick(accs: &[accounts::Account]) -> anyhow::Result<()> {
    let logged: Vec<accounts::Account> = accs.iter().filter(|a| a.logged_in()).cloned().collect();
    let out = accs.len() - logged.len();
    if out > 0 {
        println!("{out} account(s) not logged in — they cannot be picked at all\n");
    }
    match houston_core::usage::best_cached(&logged) {
        Some(houston_core::usage::Decision { account: chosen, probes, how }) => {
            println!("{:<16} {:>6} {:>6} {:>9}  STATE", "ID", "5H", "7D", "PRESSURE");
            for p in &probes {
                let state = if !p.ok {
                    "no cached reading"
                } else if p.u5 >= houston_core::usage::SATURATED || p.u7 >= houston_core::usage::SATURATED {
                    "saturated — skipped unless every account is"
                } else {
                    "usable"
                };
                let mark = if p.account.id == chosen.id { "→" } else { " " };
                println!("{mark}{:<15} {:>5.0}% {:>5.0}% {:>9.1}  {state}", p.account.id, p.u5, p.u7, p.pressure, );
            }
            println!("\nresume would open: {} — {}", chosen.id, how.explain());
            println!("(ties go to the least recently used; the journal records this as \"{}\")", how.label());
        }
        None => {
            println!("no account has a cached reading, so resume would probe the network once");
            println!("and, if that fails too, fall back to the least recently used account.");
            println!("run `houston usage --refresh` to fill the cache.");
        }
    }
    Ok(())
}

// ---- statusline styling (v1 parity): block bars + level colors ------------

const SL_RESET: &str = "\x1b[0m";
const SL_BOLD: &str = "\x1b[1m";

/// A 38;5 foreground escape for a numeric ANSI-256 code; empty for non-numeric.
fn sl_fg(code: &str) -> String {
    if !code.is_empty() && code.bytes().all(|b| b.is_ascii_digit()) {
        format!("\x1b[38;5;{code}m")
    } else {
        String::new()
    }
}

/// One account's value on the status line: `7d 100% ↻ 3h`, `▕██▏░░░░░▏ 27%`, `off`.
///
/// Extracted from the render loop so its shape can be pinned by a test — the line
/// is the most-looked-at surface Houston has, and it is edited by eye.
///
/// Two deliberate details:
///
/// - **A full bar is dropped.** Eight identical blocks repeat what "100%" already
///   says, for ten columns on a line that three accounts share. The number is red
///   at that point anyway, and the countdown is what you actually want there.
///   (The quota *pane* keeps its bar: there the bars align into a column, and
///   removing one would misalign every percentage beside it.)
/// - **A space after `↻`.** `↻3h` reads as one crowded token; `↻ 3h` reads as
///   "returns in 3h".
/// - **`login`, not `off`, when a login would fix it.** "off" answers *what* and
///   leaves *what do I do* to a guess between two very different remedies; the
///   cache knows which one applies, and the word IS the remedy. Amber rather than
///   grey because it is actionable — grey `off` stays for the failures waiting
///   won't cure but a login won't either.
fn account_value(pct: f64, tag: &str, renew: Option<&str>, ok: bool, needs_login: bool, styled: bool) -> String {
    if !ok {
        let (word, color) = if needs_login { ("login", "214") } else { ("off", "240") };
        return if styled { format!("{}{word}{}", sl_fg(color), SL_RESET) } else { word.into() };
    }
    let renew = renew.map(|s| format!(" ↻ {s}")).unwrap_or_default();
    let bar = if pct.round() >= 100.0 { String::new() } else { block_bar(pct, true, styled) };
    if styled {
        let t = if tag.is_empty() { String::new() } else { format!("{}{tag}{}", sl_fg("240"), SL_RESET) };
        let r = if renew.is_empty() { String::new() } else { format!("{}{renew}{}", sl_fg("240"), SL_RESET) };
        format!("{t}{bar} {}{pct:.0}%{}{r}", sl_fg(level_code(pct)), SL_RESET)
    } else {
        format!("{tag}{bar} {pct:.0}%{renew}")
    }
}

/// Usage level → color code: green < 50 ≤ amber < 80 ≤ red.
fn level_code(pct: f64) -> &'static str {
    if pct >= 80.0 {
        "203"
    } else if pct >= 50.0 {
        "214"
    } else {
        "42"
    }
}

/// Eighth-block bar split into filled/empty runs (v1's smooth bar).
fn bar_cells(pct: f64, width: usize) -> (String, String) {
    let pct = pct.clamp(0.0, 100.0);
    let eighths = (pct / 100.0 * (width * 8) as f64).round() as usize;
    let (full, rem) = (eighths / 8, eighths % 8);
    let parts = [' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉'];
    let mut f = String::new();
    let mut e = String::new();
    for i in 0..width {
        if i < full {
            f.push('█');
        } else if i == full && rem > 0 {
            f.push(parts[rem]);
        } else {
            e.push('░');
        }
    }
    (f, e)
}

/// "▕███▌░░░▏" — track dim, fill in the level color when styled.
fn block_bar(pct: f64, ok: bool, styled: bool) -> String {
    let width = 8;
    if !ok {
        let empty = "░".repeat(width);
        return if styled { format!("{}▕{empty}▏{}", sl_fg("240"), SL_RESET) } else { format!("▕{empty}▏") };
    }
    let (filled, empty) = bar_cells(pct, width);
    if !styled {
        format!("▕{filled}{empty}▏")
    } else {
        format!("{d}▕{lvl}{filled}{d}{empty}▏{r}", d = sl_fg("240"), lvl = sl_fg(level_code(pct)), r = SL_RESET)
    }
}

/// The active account's id, from CLAUDE_CONFIG_DIR matched against the
/// registry. None when running Claude's default config.
///
/// Takes the already-loaded registry: the status line reads it once per render,
/// and `accounts::active_id()` (the shared version, used by hooks) would read it
/// a second time.
fn active_account_id(accs: &[accounts::Account]) -> Option<String> {
    let cd = PathBuf::from(std::env::var_os("CLAUDE_CONFIG_DIR")?);
    accs.iter()
        .find(|a| a.resolve_config_dir().is_some_and(|d| d == cd || std::fs::canonicalize(&d).ok() == std::fs::canonicalize(&cd).ok()))
        .map(|a| a.id.clone())
}

/// A tiny 10-cell usage bar with a percentage.
fn bar(pct: f64) -> String {
    let cells = 10;
    let fill = ((pct / 100.0) * cells as f64).round().clamp(0.0, cells as f64) as usize;
    format!("[{}{}] {pct:.0}%", "#".repeat(fill), "-".repeat(cells - fill))
}

#[cfg(test)]
mod tests {
    use super::*;
    use houston_core::accounts::Account;

    fn acc(id: &str, last: &str, cfg: &str) -> Account {
        Account { id: id.into(), last_use: last.into(), config_dir: cfg.into(), ..Default::default() }
    }

    #[test]
    fn choose_forced_wins_when_known() {
        let accs = vec![acc("a", "@5", ""), acc("b", "@1", "")];
        // config_dir empty → logged_in() false; forced bypasses the login filter.
        let (c, how) = choose_account(&accs, Some("b")).unwrap();
        assert_eq!(c.id, "b");
        assert_eq!(how, houston_core::usage::Pick::Forced, "the reason recorded is the user's choice");
        assert!(choose_account(&accs, Some("zzz")).is_none());
    }

    #[test]
    fn choose_falls_back_to_least_recently_used_and_says_so() {
        // Point config_dir at real temp dirs with a .credentials.json so
        // logged_in() is true, and vary last_use. The credentials are empty, so
        // every probe fails fast without touching the network.
        // Crate-wide env lock: this test sets and clears HOUSTON_HOME, and
        // provision.rs's tests depend on it at the same time.
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        // Point the store at the temp dir too: without this the cache fallback
        // would read the developer's REAL usage cache mid-test.
        unsafe { std::env::set_var("HOUSTON_HOME", tmp.path()) };
        let mk = |id: &str, last: &str| {
            let d = tmp.path().join(id);
            std::fs::create_dir_all(&d).unwrap();
            std::fs::write(d.join(".credentials.json"), "{}").unwrap();
            acc(id, last, d.to_str().unwrap())
        };
        let accs = vec![mk("recent", "@9"), mk("old", "@2"), mk("mid", "@5")];
        let (c, how) = choose_account(&accs, None).unwrap();
        assert_eq!(c.id, "old", "LRU account is launched");
        assert_eq!(
            how,
            houston_core::usage::Pick::LruFallback,
            "with no probe and no cached reading, the decision must admit it is blind"
        );
        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }

    /// The status line is edited by eye, so its shape is pinned here.
    #[test]
    fn a_full_bar_is_dropped_and_the_countdown_breathes() {
        // Exhausted weekly: tag, no bar, the number, the countdown with its space.
        assert_eq!(account_value(100.0, "7d", Some("3h"), true, false, false), "7d 100% ↻ 3h");
        // 99.6 PRINTS as 100%, so its bar would be full too — the two must agree,
        // or the line would show a full bar next to "100%" on some renders only.
        assert_eq!(account_value(99.6, "7d", None, true, false, false), "7d 100%");
        // Anything that is not full keeps its bar.
        let normal = account_value(27.0, "", None, true, false, false);
        assert!(normal.starts_with('▕') && normal.ends_with(" 27%"), "{normal}");
        assert!(!account_value(94.0, "7d", Some("2d"), true, false, false).contains("7d 94%"), "94% still has a bar");
        // No reading is not 0%.
        assert_eq!(account_value(0.0, "", None, false, false, false), "off");
        // A failure a login fixes SAYS so: the word is the remedy, and "off" was
        // how two dead accounts sat unnoticed for weeks.
        assert_eq!(account_value(0.0, "", None, false, true, false), "login");
        // Styled: the escapes wrap the number, and the bar is still absent at 100%.
        let styled = account_value(100.0, "7d", Some("3h"), true, false, true);
        assert!(!styled.contains('█') && !styled.contains('░'), "{styled}");
        assert!(styled.contains(&format!("{}100%", sl_fg(level_code(100.0)))), "the number carries the colour");
        assert!(styled.contains("↻ 3h"));
    }

    #[test]
    fn bar_scales_and_clamps() {
        assert_eq!(bar(0.0), "[----------] 0%");
        assert_eq!(bar(50.0), "[#####-----] 50%");
        assert_eq!(bar(100.0), "[##########] 100%");
        assert_eq!(bar(140.0), "[##########] 140%"); // clamps fill, shows real pct
    }
}
