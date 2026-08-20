//! Opens URLs on behalf of the `claude` child process. Houston injects itself
//! as the child's `$BROWSER` (see `launch`): Claude Code invokes `$BROWSER`
//! with the URL as its only argument for EVERY link it opens — the OAuth login
//! page included. Login URLs are opened in a PRIVATE window of the default
//! browser, so the login never inherits the signed-in claude.ai session of
//! your normal browsing (crucial when juggling several accounts); every other
//! URL opens normally.

use std::process::Command;

/// Whether `s` is a plain http(s) URL — the shape `$BROWSER` mode accepts.
/// Anything else (a directory path, a flag) is not for us. Parsed by hand to
/// keep the kernel dependency-free.
pub fn is_http(s: &str) -> bool {
    host_and_path(s).is_some()
}

/// Split an http(s) URL into (lowercased host, path). None if it isn't one.
///
/// The order here follows RFC 3986 and it MATTERS: the authority ends at the
/// first `/`, `?` or `#`, and userinfo lives inside that authority. Stripping
/// userinfo from the whole remainder instead would let an `@` anywhere in the
/// PATH decide the host — `https://real-host.example/x@claude.ai/oauth` would
/// report `claude.ai`, so a lookalike host could impersonate a login URL.
fn host_and_path(s: &str) -> Option<(String, String)> {
    // Case-insensitive scheme, without allocating for the whole URL.
    let rest = ["https://", "http://"].iter().find_map(|scheme| {
        let n = scheme.len();
        (s.len() >= n && s[..n].eq_ignore_ascii_case(scheme)).then(|| &s[n..])
    })?;
    // 1. The authority is everything up to the first path/query/fragment mark.
    let (authority, path) = match rest.find(['/', '?', '#']) {
        Some(i) => (&rest[..i], &rest[i..]),
        None => (rest, ""),
    };
    // 2. Userinfo is inside the authority only, before its LAST '@'.
    let hostport = authority.rsplit_once('@').map(|(_, h)| h).unwrap_or(authority);
    // 3. Drop the port. A bracketed IPv6 literal keeps its colons.
    let host = match hostport.strip_prefix('[') {
        Some(v6) => v6.split(']').next().unwrap_or(""),
        None => hostport.split(':').next().unwrap_or(""),
    };
    if host.is_empty() {
        return None;
    }
    let path = if path.starts_with('/') { path } else { "/" };
    Some((host.to_ascii_lowercase(), path.to_string()))
}

/// Strip whitespace and one level of surrounding quotes. Claude Code pre-quotes
/// the URL it hands to `$BROWSER` (`"https://…"`); depending on the platform's
/// argv round-trip the quotes may arrive as literal characters.
pub fn clean_url(raw: &str) -> String {
    let s = raw.trim();
    let bytes = s.as_bytes();
    if bytes.len() >= 2 && bytes[0] == b'"' && bytes[bytes.len() - 1] == b'"' {
        return s[1..s.len() - 1].to_string();
    }
    s.to_string()
}

/// Whether `s` is an Anthropic OAuth page — the URLs worth isolating in a
/// private window. Kept tight (host + /oauth path) so ordinary claude.ai links
/// keep opening in the normal browser session.
pub fn is_login(s: &str) -> bool {
    let Some((host, path)) = host_and_path(s) else { return false };
    let anthropic = host == "claude.ai"
        || host.ends_with(".claude.ai")
        || host == "console.anthropic.com"
        || host == "anthropic.com"
        || host.ends_with(".anthropic.com");
    anthropic && path.starts_with("/oauth")
}

/// The private-window switch per browser family — the same table Claude Code's
/// bundled opener uses.
fn private_flag(id: &str) -> &'static str {
    let id = id.to_ascii_lowercase();
    if id.contains("brave") || id.contains("chrom") {
        "--incognito"
    } else if id.contains("firefox") {
        "--private-window"
    } else if id.contains("edge") {
        "--inprivate"
    } else {
        ""
    }
}

/// Dispatch a URL coming from the claude child: login pages go to a private
/// window (falling back to a normal open if the default browser isn't
/// recognized), everything else to the default opener. Set
/// `HOUSTON_LOGIN_PRIVATE=off` to disable the private-window behavior.
pub fn open(raw: &str) -> std::io::Result<()> {
    let u = clean_url(raw);
    if !is_http(&u) {
        return Err(std::io::Error::other(format!("not an http(s) url: {raw:?}")));
    }
    let private_off = std::env::var("HOUSTON_LOGIN_PRIVATE").map(|v| v == "off").unwrap_or(false);
    if is_login(&u) && !private_off && open_private(&u).is_ok() {
        return Ok(());
    }
    // Unrecognized/undetectable default browser: a normal open still logs you
    // in — just without the isolation.
    open_default(&u)
}

// ------------------------------------------------------------------ windows --

#[cfg(windows)]
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

/// Open the URL with the system's default handler — the same mechanism Claude
/// Code itself uses when no `$BROWSER` is set.
#[cfg(windows)]
fn open_default(u: &str) -> std::io::Result<()> {
    use std::os::windows::process::CommandExt;
    Command::new("rundll32")
        .arg("url,OpenURL")
        .arg(u)
        .creation_flags(CREATE_NO_WINDOW)
        .spawn()
        .map(|_| ())
}

/// Open the URL in a private window of the DEFAULT browser: resolve the user's
/// https handler from the registry (UserChoice ProgId → its shell open
/// command), match the browser family and append its private-window switch. An
/// error means "couldn't do it safely" — the caller falls back to a normal open.
#[cfg(windows)]
fn open_private(u: &str) -> std::io::Result<()> {
    use std::os::windows::process::CommandExt;
    let prog_id = default_prog_id()?;
    let cmdline = prog_id_command(&prog_id)?;
    let exe = exe_from_command(&cmdline);
    let flag = private_flag(&format!("{prog_id} {exe}"));
    if exe.is_empty() || flag.is_empty() {
        return Err(std::io::Error::other(format!("default browser {prog_id:?} has no known private mode")));
    }
    Command::new(exe)
        .arg(flag)
        .arg(u)
        .creation_flags(CREATE_NO_WINDOW)
        .spawn()
        .map(|_| ())
}

/// Query one registry value with `reg query`, avoiding a registry crate. The
/// value is the tail of the `NAME  REG_SZ  VALUE` line.
#[cfg(windows)]
fn reg_query(key: &str, value_args: &[&str]) -> std::io::Result<String> {
    use std::os::windows::process::CommandExt;
    let out = Command::new("reg")
        .arg("query")
        .arg(key)
        .args(value_args)
        .creation_flags(CREATE_NO_WINDOW)
        .output()?;
    if !out.status.success() {
        return Err(std::io::Error::other(format!("reg query failed for {key}")));
    }
    parse_reg_value(&String::from_utf8_lossy(&out.stdout))
        .ok_or_else(|| std::io::Error::other(format!("no value in reg output for {key}")))
}

/// The ProgId of the user's default https handler.
#[cfg(windows)]
fn default_prog_id() -> std::io::Result<String> {
    reg_query(
        r"HKCU\Software\Microsoft\Windows\Shell\Associations\UrlAssociations\https\UserChoice",
        &["/v", "ProgId"],
    )
}

/// The shell open command registered for a ProgId, e.g.
/// `"C:\...\msedge.exe" --single-argument %1`.
#[cfg(windows)]
fn prog_id_command(prog_id: &str) -> std::io::Result<String> {
    reg_query(&format!(r"HKCR\{prog_id}\shell\open\command"), &["/ve"])
}

// --------------------------------------------------------------------- unix --

#[cfg(not(windows))]
fn open_default(u: &str) -> std::io::Result<()> {
    if cfg!(target_os = "macos") {
        return Command::new("open").arg(u).spawn().map(|_| ());
    }
    Command::new("xdg-open").arg(u).spawn().map(|_| ())
}

#[cfg(not(windows))]
fn open_private(u: &str) -> std::io::Result<()> {
    // Resolve the default browser's desktop id, then its private flag.
    let out = Command::new("xdg-settings").args(["get", "default-web-browser"]).output()?;
    if !out.status.success() {
        return Err(std::io::Error::other("could not read default browser"));
    }
    let id = String::from_utf8_lossy(&out.stdout).trim().to_string();
    let flag = private_flag(&id);
    if flag.is_empty() {
        return Err(std::io::Error::other(format!("default browser {id:?} has no known private mode")));
    }
    // The desktop id maps to a binary by convention (firefox.desktop → firefox).
    let bin = id.trim_end_matches(".desktop").split(['-', '_']).next().unwrap_or("").to_string();
    if bin.is_empty() {
        return Err(std::io::Error::other("no browser binary"));
    }
    Command::new(bin).arg(flag).arg(u).spawn().map(|_| ())
}

// ------------------------------------------------------------------ parsing --

/// Extract the executable path from a registry shell command: the quoted first
/// token, or the first whitespace-delimited token otherwise.
pub fn exe_from_command(cmd: &str) -> String {
    let cmd = cmd.trim();
    if let Some(rest) = cmd.strip_prefix('"') {
        return match rest.find('"') {
            Some(end) => rest[..end].to_string(),
            None => String::new(),
        };
    }
    match cmd.find(' ') {
        Some(i) => cmd[..i].to_string(),
        None => cmd.to_string(),
    }
}

/// Pull the value out of `reg query` output: the text after the type token
/// (REG_SZ / REG_EXPAND_SZ) on the first value line.
pub fn parse_reg_value(out: &str) -> Option<String> {
    for line in out.lines() {
        for ty in ["REG_SZ", "REG_EXPAND_SZ"] {
            if let Some(i) = line.find(ty) {
                let v = line[i + ty.len()..].trim();
                if !v.is_empty() {
                    return Some(v.to_string());
                }
            }
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn is_http_accepts_only_real_urls() {
        assert!(is_http("https://claude.ai/oauth/authorize?x=1"));
        assert!(is_http("http://localhost:3000/"));
        assert!(!is_http("C:\\Users\\me\\project"));
        assert!(!is_http("--flag"));
        assert!(!is_http("ftp://example.com"));
        assert!(!is_http("https://"), "no host");
        assert!(!is_http(""));
    }

    #[test]
    fn clean_url_strips_one_layer_of_quotes() {
        assert_eq!(clean_url("  \"https://x.dev/a\"  "), "https://x.dev/a");
        assert_eq!(clean_url("https://x.dev/a"), "https://x.dev/a");
        // Only ONE layer, and never a lone quote.
        assert_eq!(clean_url("\"\"https://x\"\""), "\"https://x\"");
        assert_eq!(clean_url("\""), "\"");
    }

    #[test]
    fn is_login_only_for_anthropic_oauth_paths() {
        assert!(is_login("https://claude.ai/oauth/authorize?code=1"));
        assert!(is_login("https://console.anthropic.com/oauth/token"));
        assert!(is_login("https://app.claude.ai/oauth"));
        assert!(is_login("HTTPS://CLAUDE.AI/oauth/x"), "host is case-insensitive");
        // Ordinary links keep the normal session.
        assert!(!is_login("https://claude.ai/chat/abc"));
        assert!(!is_login("https://docs.anthropic.com/en/docs"));
        // Lookalike hosts must not match.
        assert!(!is_login("https://claude.ai.evil.com/oauth"));
        assert!(!is_login("https://notclaude.ai/oauth"));
        assert!(!is_login("https://evil.com/oauth?claude.ai"));
    }

    #[test]
    fn the_authority_ends_before_the_path_so_an_at_sign_cannot_move_the_host() {
        // Userinfo belongs to the authority. If it were stripped from the whole
        // remainder, the '@' in these PATHS would hand the host to an attacker.
        assert_eq!(host_and_path("https://evil.example/x@claude.ai/oauth").unwrap().0, "evil.example");
        assert!(!is_login("https://evil.example/x@claude.ai/oauth"), "path @ must not fake a login host");
        assert!(!is_login("https://evil.example/?next=user@claude.ai/oauth"));
        // Real userinfo IS inside the authority and is still dropped.
        assert_eq!(host_and_path("https://user:pw@claude.ai/oauth").unwrap().0, "claude.ai");
        assert!(is_login("https://user:pw@claude.ai/oauth"));
        // The last '@' in the authority wins (a userinfo may itself contain one).
        assert_eq!(host_and_path("https://a@b@claude.ai/x").unwrap().0, "claude.ai");
    }

    #[test]
    fn scheme_is_case_insensitive_and_ports_and_ipv6_are_handled() {
        for u in ["HTTPS://claude.ai/oauth", "Https://claude.ai/oauth", "hTTp://claude.ai/oauth"] {
            assert!(is_http(u), "{u}");
            assert!(is_login(u), "{u}");
        }
        assert_eq!(host_and_path("https://claude.ai:8443/oauth").unwrap().0, "claude.ai");
        // A bracketed IPv6 literal keeps its colons but loses the brackets,
        // the same normalization Go's url.Hostname() applies.
        assert_eq!(host_and_path("http://[::1]:3000/x").unwrap().0, "::1");
        // A path-less URL still yields a root path, so prefix checks behave.
        assert_eq!(host_and_path("https://claude.ai").unwrap(), ("claude.ai".into(), "/".into()));
        assert_eq!(host_and_path("https://claude.ai?x=1").unwrap().1, "/");
    }

    #[test]
    fn private_flag_per_family() {
        assert_eq!(private_flag("MSEdgeHTM C:\\msedge.exe"), "--inprivate");
        assert_eq!(private_flag("ChromeHTML chrome.exe"), "--incognito");
        assert_eq!(private_flag("BraveHTML brave.exe"), "--incognito");
        assert_eq!(private_flag("FirefoxURL firefox.exe"), "--private-window");
        assert_eq!(private_flag("SomeBrowser weird.exe"), "");
    }

    #[test]
    fn exe_from_command_handles_quotes_and_bare_paths() {
        assert_eq!(
            exe_from_command(r#""C:\Program Files\Edge\msedge.exe" --single-argument %1"#),
            r"C:\Program Files\Edge\msedge.exe"
        );
        assert_eq!(exe_from_command(r"C:\bin\ff.exe -osint -url %1"), r"C:\bin\ff.exe");
        assert_eq!(exe_from_command(r"C:\bin\ff.exe"), r"C:\bin\ff.exe");
        assert_eq!(exe_from_command(r#""unterminated"#), "");
    }

    #[test]
    fn parse_reg_value_reads_the_value_column() {
        let out = "\r\nHKEY_CURRENT_USER\\...\\UserChoice\r\n    ProgId    REG_SZ    MSEdgeHTM\r\n\r\n";
        assert_eq!(parse_reg_value(out).unwrap(), "MSEdgeHTM");
        let cmd = "\r\nHKEY_CLASSES_ROOT\\MSEdgeHTM\\shell\\open\\command\r\n    (Default)    REG_SZ    \"C:\\msedge.exe\" --single-argument %1\r\n";
        assert_eq!(parse_reg_value(cmd).unwrap(), r#""C:\msedge.exe" --single-argument %1"#);
        assert!(parse_reg_value("ERROR: not found").is_none());
    }

    #[test]
    fn open_rejects_non_urls_without_spawning() {
        assert!(open("C:\\some\\path").is_err());
        assert!(open("").is_err());
    }
}
