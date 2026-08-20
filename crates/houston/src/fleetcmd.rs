//! `houston mcp` and `houston plugin` — configuration applied across EVERY
//! account. Add-operations are passthrough-then-propagate: the real `claude`
//! CLI runs ONCE against a source account (full flag parity, real validation
//! and downloads), then the resulting config diff is copied into every other
//! account. See houston_core::fleet.

use anyhow::{anyhow, Context};
use houston_core::accounts::{self, Account};
use houston_core::fleet::{self, Obj};

/// The accounts to apply to. At least one is required — the source account is
/// the one the real `claude` runs against.
fn load_fleet() -> anyhow::Result<Vec<Account>> {
    let accs = accounts::load().unwrap_or_default();
    if accs.is_empty() {
        return Err(anyhow!("no accounts registered — add one with: houston accounts add <label>"));
    }
    Ok(accs)
}

/// A per-account presence table, the v1 layout: one row per config key, one
/// column per account.
///
/// Cells are rendered ONCE up front and each column is sized to what it actually
/// holds. A fixed width worked while every cell was "on"/"off"/"—", and broke the
/// moment `policy` put a settings value in one: the table reserved 12 columns for
/// a 28-character value and every row after it was ragged.
fn print_table(header: &str, rows: &[String], accs: &[Account], cell: impl Fn(&str, usize) -> String) {
    let cells: Vec<Vec<String>> = rows.iter().map(|r| (0..accs.len()).map(|i| cell(r, i)).collect()).collect();
    let w = rows.iter().map(|r| r.chars().count()).max().unwrap_or(0).max(header.len());
    let col: Vec<usize> = accs
        .iter()
        .enumerate()
        .map(|(i, a)| {
            let widest = cells.iter().map(|row| row[i].chars().count()).max().unwrap_or(0);
            widest.max(a.id.chars().count()).max(3)
        })
        .collect();
    print!("{header:<w$}");
    for (i, a) in accs.iter().enumerate() {
        print!("  {:<width$}", a.id, width = col[i]);
    }
    println!();
    for (r, row) in rows.iter().zip(&cells) {
        print!("{r:<w$}");
        for (i, c) in row.iter().enumerate() {
            print!("  {:<width$}", c, width = col[i]);
        }
        println!();
    }
}

// ------------------------------------------------------------------ policy --

/// ```text
/// houston policy [ls]                        drift table across accounts
/// houston policy get <key>                   the value in each account
/// houston policy sync <key|--all> --from <id> [--apply]
/// ```
///
/// Dry run by default. These keys decide what Claude does *without asking* —
/// `permissions` most of all — so nothing is written until `--apply` says so.
pub fn policy(args: &[String]) -> anyhow::Result<()> {
    use houston_core::policy;
    let accs = load_fleet()?;
    match args.first().map(String::as_str) {
        Some("get") => {
            let key = args.get(1).ok_or_else(|| anyhow!("usage: houston policy get <key>"))?;
            if !policy::is_known(key) {
                return Err(unknown_key(key));
            }
            for a in &accs {
                match policy::get(a, key) {
                    Some(v) => println!("account-{}:\n{}", a.id, serde_json::to_string_pretty(&v)?),
                    None => println!("account-{}: (unset)", a.id),
                }
            }
            Ok(())
        }
        Some("sync") => {
            let key = args.get(1).filter(|k| !k.starts_with("--")).cloned();
            let all = args.iter().any(|a| a == "--all");
            let apply = args.iter().any(|a| a == "--apply");
            let from = args
                .iter()
                .position(|a| a == "--from")
                .and_then(|i| args.get(i + 1))
                .ok_or_else(|| anyhow!("say which account is the source: --from <id>"))?;
            let source = accs
                .iter()
                .find(|a| &a.id == from)
                .ok_or_else(|| anyhow!("no such account: {from}"))?
                .clone();
            let keys: Vec<&str> = match (&key, all) {
                (Some(k), _) if policy::is_known(k) => vec![k.as_str()],
                (Some(k), _) => return Err(unknown_key(k)),
                (None, true) => policy::KEYS.iter().map(|k| k.name).collect(),
                (None, false) => return Err(anyhow!("name a key, or --all (try: houston policy ls)")),
            };

            let mut any = false;
            for key in keys {
                // The plan is printed either way: with --apply it is the record of
                // what happened, without it the proposal.
                let rows = if apply {
                    policy::sync(&accs, &source, key)
                } else {
                    policy::plan(&accs, &source, key).into_iter().map(|(id, c)| (id, Ok(c))).collect()
                };
                for (id, res) in rows {
                    match res {
                        Ok(policy::Change::Same) => {}
                        Ok(policy::Change::SourceUnset) => {}
                        Ok(policy::Change::Add) => {
                            any = true;
                            println!("{} {key} → account-{id} (was unset)", mark(apply));
                        }
                        Ok(policy::Change::Replace(old)) => {
                            any = true;
                            println!("{} {key} → account-{id} (replaces {old})", mark(apply));
                        }
                        Err(e) => {
                            any = true;
                            println!("! {key} account-{id}: {e}");
                        }
                    }
                }
            }
            if !any {
                println!("every account already matches account-{}", source.id);
            } else if !apply {
                println!("\nnothing was written. Add --apply to do it.");
            }
            Ok(())
        }
        None | Some("ls") | Some("list") | Some("diff") => {
            let names: Vec<String> = policy::KEYS.iter().map(|k| k.name.to_string()).collect();
            let vals: Vec<Vec<Option<serde_json::Value>>> =
                policy::KEYS.iter().map(|k| accs.iter().map(|a| policy::get(a, k.name)).collect()).collect();
            print_table("SETTING", &names, &accs, |name, i| {
                let row = policy::KEYS.iter().position(|k| k.name == name).unwrap_or(0);
                policy::summarize(vals[row][i].as_ref())
            });
            // Naming the drifted keys is the point of the table; a wall of values
            // where two cells differ by one character is not readable on its own.
            let drift: Vec<&str> =
                policy::KEYS.iter().map(|k| k.name).filter(|k| !policy::agrees(&accs, k)).collect();
            if drift.is_empty() {
                println!("\nevery account agrees on all {} keys.", policy::KEYS.len());
            } else {
                println!("\ndrifted: {}", drift.join(", "));
                println!("to make them match:  houston policy sync {} --from <account>", drift[0]);
            }
            println!("\nHouston writes each account's USER scope only — the one scope that means");
            println!("the same thing everywhere. Project and local settings are left alone.");
            Ok(())
        }
        Some(other) => Err(anyhow!("unknown policy subcommand: {other} (ls | get | sync)")),
    }
}

fn mark(applied: bool) -> &'static str {
    if applied { "+" } else { "would set" }
}

fn unknown_key(key: &str) -> anyhow::Error {
    let known: Vec<&str> = houston_core::policy::KEYS.iter().map(|k| k.name).collect();
    anyhow!(
        "Houston does not propagate {key:?}.\nIt owns: {}\n\
         (statusLine, hooks, enabledPlugins, mcpServers and cleanupPeriodDays have their own commands.)",
        known.join(", ")
    )
}

// --------------------------------------------------------------------- mcp --

/// ```text
/// houston mcp add <name> [claude mcp add flags/args...]
/// houston mcp add-json <name> '<json>'
/// houston mcp rm <name>
/// houston mcp ls
/// ```
pub fn mcp(args: &[String]) -> anyhow::Result<()> {
    let accs = load_fleet()?;
    match args.first().map(String::as_str) {
        Some(sub @ ("add" | "add-json")) => {
            // Only the USER scope propagates: local/project scopes are
            // per-directory and mean nothing in another account.
            for (i, a) in args.iter().enumerate() {
                if (a == "-s" || a == "--scope") && args.get(i + 1).map(String::as_str) != Some("user") {
                    return Err(anyhow!(
                        "only user-scope servers propagate across accounts; for local/project scope use claude directly"
                    ));
                }
            }
            let src = &accs[0];
            let before = fleet::mcp_servers(src);
            let mut claude_args: Vec<String> =
                vec!["mcp".into(), sub.into(), "--scope".into(), "user".into()];
            claude_args.extend(args[1..].iter().cloned());
            let status = fleet::run_claude(src, &claude_args).context("running claude mcp")?;
            if !status.success() {
                // claude already printed why.
                std::process::exit(status.code().unwrap_or(1));
            }
            let after = fleet::mcp_servers(src);
            let (changed, removed) = fleet::diff(&before, &after);
            if changed.is_empty() && removed.is_empty() {
                println!("houston: claude made no user-scope change; nothing to propagate");
                return Ok(());
            }
            for a in &accs[1..] {
                fleet::patch_mcp(a, &changed, &removed).with_context(|| format!("account {}", a.id))?;
            }
            for k in fleet::keys(&[changed]) {
                println!("✓ mcp server {k:?} propagated to all {} accounts", accs.len());
            }
            Ok(())
        }
        Some("rm") | Some("remove") => {
            let name = args.get(1).ok_or_else(|| anyhow!("usage: houston mcp rm <name>"))?;
            for a in &accs {
                fleet::patch_mcp(a, &Obj::new(), std::slice::from_ref(name))
                    .with_context(|| format!("account {}", a.id))?;
            }
            println!("✓ mcp server {name:?} removed from all {} accounts", accs.len());
            Ok(())
        }
        None | Some("ls") | Some("list") => {
            let per: Vec<Obj> = accs.iter().map(fleet::mcp_servers).collect();
            let names = fleet::keys(&per);
            if names.is_empty() {
                println!("no user-scope MCP servers. Add one:  houston mcp add <name> -- <command>");
                return Ok(());
            }
            print_table("MCP SERVER", &names, &accs, |name, i| {
                if per[i].contains_key(name) { "✓".into() } else { "—".into() }
            });
            Ok(())
        }
        Some(other) => Err(anyhow!(
            "unknown mcp subcommand: {other} (add <name> … | add-json <name> <json> | rm <name> | ls)"
        )),
    }
}

// ------------------------------------------------------------------ plugin --

/// Plugin FILES already land in the shared store (the plugins dir is
/// junctioned), so install/uninstall runs ONCE via the real `claude plugin`;
/// what propagates is the ENABLEMENT (settings.json → enabledPlugins).
///
/// ```text
/// houston plugin install <spec>
/// houston plugin enable|disable <spec>
/// houston plugin uninstall <spec>
/// houston plugin ls
/// houston plugin marketplace <claude plugin marketplace args...>
/// ```
pub fn plugin(args: &[String]) -> anyhow::Result<()> {
    let accs = load_fleet()?;
    match args.first().map(String::as_str) {
        Some("install") => {
            let spec = args.get(1).ok_or_else(|| anyhow!("usage: houston plugin install <spec>"))?;
            let src = &accs[0];
            let before = fleet::enabled_plugins(src);
            let status = fleet::run_claude(src, &["plugin".into(), "install".into(), spec.clone()])
                .context("running claude plugin install")?;
            if !status.success() {
                std::process::exit(status.code().unwrap_or(1));
            }
            let after = fleet::enabled_plugins(src);
            let (mut changed, removed) = fleet::diff(&before, &after);
            // claude may install without touching enablement; enable the
            // matching keys ourselves so the fleet ends up consistent.
            if changed.is_empty() {
                for k in fleet::match_plugin_keys(spec, &fleet::keys(&[after])) {
                    changed.insert(k, serde_json::json!(true));
                }
            }
            if changed.is_empty() && removed.is_empty() {
                println!("houston: nothing to propagate");
                return Ok(());
            }
            for a in &accs[1..] {
                fleet::patch_plugins(a, &changed, &removed).with_context(|| format!("account {}", a.id))?;
            }
            for k in fleet::keys(&[changed]) {
                println!("✓ plugin {k:?} enabled in all {} accounts", accs.len());
            }
            Ok(())
        }
        Some(verb @ ("enable" | "disable")) => {
            let spec = args.get(1).ok_or_else(|| anyhow!("usage: houston plugin {verb} <spec>"))?;
            let per: Vec<Obj> = accs.iter().map(fleet::enabled_plugins).collect();
            let matched = fleet::match_plugin_keys(spec, &fleet::keys(&per));
            if matched.is_empty() {
                return Err(anyhow!("no installed plugin matches {spec:?} (see: houston plugin ls)"));
            }
            let on = verb == "enable";
            let set: Obj = matched.iter().map(|k| (k.clone(), serde_json::json!(on))).collect();
            for a in &accs {
                fleet::patch_plugins(a, &set, &[]).with_context(|| format!("account {}", a.id))?;
            }
            for k in &matched {
                println!("✓ plugin {k:?} {verb}d in all {} accounts", accs.len());
            }
            Ok(())
        }
        Some("uninstall") | Some("rm") => {
            let spec = args.get(1).ok_or_else(|| anyhow!("usage: houston plugin uninstall <spec>"))?;
            let per: Vec<Obj> = accs.iter().map(fleet::enabled_plugins).collect();
            let keys = fleet::match_plugin_keys(spec, &fleet::keys(&per));
            // The files are shared, so one uninstall is enough; ignore its exit
            // status (the plugin may already be gone) and clean enablement.
            let _ = fleet::run_claude(&accs[0], &["plugin".into(), "uninstall".into(), spec.clone()]);
            for a in &accs {
                fleet::patch_plugins(a, &Obj::new(), &keys).with_context(|| format!("account {}", a.id))?;
            }
            println!("✓ plugin {spec:?} removed from all {} accounts", accs.len());
            Ok(())
        }
        Some("marketplace") => {
            let mut a: Vec<String> = vec!["plugin".into(), "marketplace".into()];
            a.extend(args[1..].iter().cloned());
            let status = fleet::run_claude(&accs[0], &a).context("running claude plugin marketplace")?;
            if !status.success() {
                std::process::exit(status.code().unwrap_or(1));
            }
            Ok(())
        }
        None | Some("ls") | Some("list") => {
            let per: Vec<Obj> = accs.iter().map(fleet::enabled_plugins).collect();
            let names = fleet::keys(&per);
            if names.is_empty() {
                println!("no plugins enabled. Install one:  houston plugin install <spec>");
                return Ok(());
            }
            print_table("PLUGIN", &names, &accs, |name, i| match per[i].get(name) {
                Some(v) if fleet::is_enabled(v) => "on".into(),
                Some(_) => "off".into(),
                None => "—".into(),
            });
            Ok(())
        }
        Some(other) => Err(anyhow!(
            "unknown plugin subcommand: {other} \
             (install | enable | disable | uninstall | marketplace | ls)"
        )),
    }
}
