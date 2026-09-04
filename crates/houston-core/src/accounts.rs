//! Claude accounts. Each account is a separate CLAUDE_CONFIG_DIR
//! (~/.claude-accounts/account-<id>) with its own /login and onboarding, so
//! Claude shows the real account identity and different terminals can run
//! different accounts concurrently. The bulk data (projects, sessions,
//! plugins, plans, todos) is shared across accounts via junction/symlink.
//! Registry mutations are serialized with the flock advisory lock — two
//! `houston run` in parallel must not lose each other's updates.

use crate::flock;
use crate::paths::{home, store_dir};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::fs;
use std::io;
use std::path::PathBuf;
use std::time::Duration;

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Account {
    /// Short stable id.
    pub id: String,
    /// Human label (e.g. email / "work").
    pub label: String,
    /// RFC3339.
    pub added_at: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub last_use: String,
    /// Per-account CLAUDE_CONFIG_DIR (isolated login/onboarding).
    #[serde(skip_serializing_if = "String::is_empty")]
    pub config_dir: String,
}

/// The claudeAiOauth block of .credentials.json Houston needs: the accessToken
/// to probe usage, plus the refreshToken/expiry to renew it when it lapses.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Credential {
    pub access_token: String,
    pub refresh_token: String,
    /// Unix milliseconds.
    pub expires_at: i64,
}

/// Where per-account Claude config dirs live. Override with
/// $HOUSTON_ACCOUNTS_DIR.
pub fn accounts_dir() -> PathBuf {
    if let Some(d) = std::env::var_os("HOUSTON_ACCOUNTS_DIR") {
        if !d.is_empty() {
            return PathBuf::from(d);
        }
    }
    home().join(".claude-accounts")
}

impl Account {
    /// The account's CLAUDE_CONFIG_DIR: the stored config_dir if set, else the
    /// conventional <accounts_dir>/account-<id>. None means "use Claude's
    /// default" (no account configured).
    pub fn resolve_config_dir(&self) -> Option<PathBuf> {
        if !self.config_dir.trim().is_empty() {
            return Some(PathBuf::from(&self.config_dir));
        }
        if self.id.is_empty() {
            return None;
        }
        Some(accounts_dir().join(format!("account-{}", self.id)))
    }

    fn credentials_path(&self) -> Option<PathBuf> {
        Some(self.resolve_config_dir()?.join(".credentials.json"))
    }

    /// Days since this account's credential was last written, i.e. since its
    /// refresh token was last successfully rotated.
    ///
    /// Why this is worth reporting: the refresh token dies of **inactivity**, and
    /// nothing announces it. It is only discovered on the next use, which is
    /// always a worse moment than being told in advance. This is the one number
    /// that predicts it, and Houston already had it sitting in the filesystem.
    ///
    /// It is the file's mtime and nothing cleverer, so it reads a *rotation*, not
    /// a check: a probe that reuses a still-valid access token does not touch the
    /// file, so a few days here is normal and healthy. What matters is the trend,
    /// which is why no threshold is baked in — an inactivity window Anthropic does
    /// not document is not something to hardcode a verdict against.
    pub fn credential_age_days(&self) -> Option<i64> {
        let m = fs::metadata(self.credentials_path()?).ok()?.modified().ok()?;
        let secs = std::time::SystemTime::now().duration_since(m).ok()?.as_secs();
        Some((secs / 86_400) as i64)
    }

    /// Whether this account's config dir has its own credentials (the user has
    /// done a one-time `/login` in it).
    pub fn logged_in(&self) -> bool {
        self.credentials_path().is_some_and(|p| p.exists())
    }

    /// The logged-in account email recorded in this account's config dir
    /// (.claude.json → oauthAccount), or None if never logged in.
    pub fn email(&self) -> Option<String> {
        let p = self.resolve_config_dir()?.join(".claude.json");
        let v: Value = serde_json::from_slice(&fs::read(p).ok()?).ok()?;
        let email = v.get("oauthAccount")?.get("emailAddress")?.as_str()?;
        if email.is_empty() {
            None
        } else {
            Some(email.to_string())
        }
    }

    /// The account's stored OAuth credential; None when the dir was never
    /// logged in (no file or no accessToken).
    pub fn credential(&self) -> Option<Credential> {
        #[derive(Deserialize)]
        struct File {
            #[serde(rename = "claudeAiOauth", default)]
            oauth: Credential,
        }
        let p = self.credentials_path()?;
        let f: File = serde_json::from_slice(&fs::read(p).ok()?).ok()?;
        if f.oauth.access_token.is_empty() {
            None
        } else {
            Some(f.oauth)
        }
    }

    /// Write refreshed OAuth tokens back into .credentials.json, preserving
    /// every other field (top level and inside claudeAiOauth — scopes,
    /// subscriptionType, etc. survive untouched). Atomic write: it's the very
    /// file Claude Code reads, so the next launch picks up the fresh token.
    pub fn save_tokens(&self, access: &str, refresh: &str, expires_at: i64) -> io::Result<()> {
        let p = self
            .credentials_path()
            .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "no config dir"))?;
        let mut top: Value = serde_json::from_slice(&fs::read(&p)?)?;
        let obj = top
            .as_object_mut()
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "credentials not an object"))?;
        let oauth = obj
            .entry("claudeAiOauth")
            .or_insert_with(|| Value::Object(Default::default()));
        let oa = oauth
            .as_object_mut()
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "claudeAiOauth not an object"))?;
        oa.insert("accessToken".into(), Value::String(access.to_string()));
        oa.insert("refreshToken".into(), Value::String(refresh.to_string()));
        oa.insert("expiresAt".into(), Value::from(expires_at));
        write_atomic(&p, serde_json::to_vec(&top)?.as_slice())
    }

    /// The advisory lock that serializes refreshes of this account's
    /// credential across processes.
    pub fn lock_path(&self) -> Option<PathBuf> {
        let p = self.credentials_path()?;
        Some(p.with_extension("json.lock"))
    }
}

fn registry_path() -> PathBuf {
    store_dir().join("accounts.json")
}

/// How long a registry mutation waits for the lock; these are read-modify-
/// write cycles over a small JSON file, so contention is brief.
const REGISTRY_LOCK_WAIT: Duration = Duration::from_secs(3);

/// The stored accounts (empty if none yet).
pub fn load() -> io::Result<Vec<Account>> {
    match fs::read(registry_path()) {
        Ok(b) => serde_json::from_slice(&b)
            .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e)),
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(Vec::new()),
        Err(e) => Err(e),
    }
}

/// The account this process is running under, from `CLAUDE_CONFIG_DIR` matched
/// against the registry. `None` when the variable is unset (Claude's own default
/// config) or points somewhere Houston does not manage.
///
/// Used by the status line and by hooks, which run INSIDE a session and therefore
/// have the variable set — it is how an event knows which account it belongs to.
pub fn active_id() -> Option<String> {
    let cd = PathBuf::from(std::env::var_os("CLAUDE_CONFIG_DIR")?);
    let accs = load().ok()?;
    accs.iter().find(|a| a.resolve_config_dir().is_some_and(|d| same_path(&d, &cd))).map(|a| a.id.clone())
}

/// Path equality that tolerates the two spellings of the same directory: exact
/// first, then canonicalized (which resolves the junctions Houston creates).
///
/// The `zip` is what makes it correct and is easy to lose: comparing
/// `canonicalize(a).ok() == canonicalize(b).ok()` reports EQUAL when both
/// fail, because `None == None`. Two paths that do not exist are not the same
/// directory. A copy in the statusline had exactly that bug.
pub fn same_path(a: &std::path::Path, b: &std::path::Path) -> bool {
    a == b
        || fs::canonicalize(a).ok().zip(fs::canonicalize(b).ok()).map(|(x, y)| x == y).unwrap_or(false)
}

/// Write accounts atomically.
pub fn save(list: &[Account]) -> io::Result<()> {
    let p = registry_path();
    if let Some(dir) = p.parent() {
        fs::create_dir_all(dir)?;
    }
    write_atomic(&p, serde_json::to_vec_pretty(list)?.as_slice())
}

fn lock_registry() -> io::Result<flock::Lock> {
    let _ = fs::create_dir_all(store_dir());
    flock::acquire(&registry_path().with_extension("json.lock"), REGISTRY_LOCK_WAIT)
}

fn slug(label: &str) -> String {
    let s: String = label
        .trim()
        .to_lowercase()
        .chars()
        .map(|r| if r.is_ascii_alphanumeric() { r } else { '-' })
        .collect();
    let s = s.trim_matches('-').to_string();
    if s.is_empty() {
        "acct".to_string()
    } else {
        s
    }
}

/// Store a new account. `now` is passed in (callers stamp time) to keep this
/// testable. Returns the created account.
pub fn add(label: &str, now: &str) -> io::Result<Account> {
    let _lk = lock_registry()?;
    let mut list = load()?;
    let base = slug(label);
    let mut id = base.clone();
    let mut i = 2;
    while list.iter().any(|a| a.id == id) {
        id = format!("{base}-{i}");
        i += 1;
    }
    let acc = Account {
        id,
        label: label.to_string(),
        added_at: now.to_string(),
        ..Default::default()
    };
    list.push(acc.clone());
    save(&list)?;
    Ok(acc)
}

/// Delete the account with the given id.
pub fn remove(id: &str) -> io::Result<()> {
    let _lk = lock_registry()?;
    let mut list = load()?;
    list.retain(|a| a.id != id);
    save(&list)
}

/// Stamp last_use for an account id (best-effort registry touch).
pub fn touch_last_use(id: &str, now: &str) -> io::Result<()> {
    let _lk = lock_registry()?;
    let mut list = load()?;
    if let Some(a) = list.iter_mut().find(|a| a.id == id) {
        a.last_use = now.to_string();
    }
    save(&list)
}

/// Atomic write via uniquely-named same-dir temp + rename. The unique name
/// matters as much as the rename: two processes writing the same fixed .tmp
/// path can interleave and rename corrupted bytes into place.
/// Atomic write for the files in this module — the account registry and
/// `.credentials.json`.
///
/// This used to be its own implementation, and it was the one that got the
/// permissions right: it created the temp WITH the destination's mode instead
/// of chmod'ing it afterwards. That is now what `atomic::write` does for
/// everybody, so this is a call rather than a copy — the point of folding them
/// together being that the lesson was learned here and nowhere else.
fn write_atomic(path: &std::path::Path, b: &[u8]) -> io::Result<()> {
    crate::atomic::write(path, b)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Serialize the HOUSTON_HOME-dependent tests: cargo runs tests in
    /// parallel and env vars are process-global.
    use crate::TEST_ENV_LOCK as ENV_LOCK;

    #[test]
    fn add_slugs_dedups_and_removes() {
        let _g = ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("HOUSTON_HOME", tmp.path()) };

        let a = add("Trabajo STIC!", "2026-01-01T00:00:00Z").unwrap();
        assert_eq!(a.id, "trabajo-stic");
        let b = add("trabajo stic", "2026-01-01T00:00:00Z").unwrap();
        assert_eq!(b.id, "trabajo-stic-2"); // collision → suffixed

        let list = load().unwrap();
        assert_eq!(list.len(), 2);
        remove("trabajo-stic").unwrap();
        let list = load().unwrap();
        assert_eq!(list.len(), 1);
        assert_eq!(list[0].id, "trabajo-stic-2");

        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }

    /// A credential file must never widen its permissions when Houston
    /// refreshes it: the rename installs the TEMP file's mode, so writing the
    /// temp with the default umask would expose tokens to every local user.
    #[cfg(unix)]
    #[test]
    fn refreshing_tokens_keeps_the_credential_file_owner_only() {
        use std::os::unix::fs::PermissionsExt;
        let _g = ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        let cfg = tmp.path().join("acct");
        fs::create_dir_all(&cfg).unwrap();
        let creds = cfg.join(".credentials.json");
        fs::write(&creds, r#"{"claudeAiOauth":{"accessToken":"old","refreshToken":"r0","expiresAt":1}}"#).unwrap();
        fs::set_permissions(&creds, fs::Permissions::from_mode(0o600)).unwrap();

        let a = Account { id: "x".into(), config_dir: cfg.to_string_lossy().into_owned(), ..Default::default() };
        a.save_tokens("new", "r1", 42).unwrap();

        let mode = fs::metadata(&creds).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "tokens must stay owner-only after a refresh");
    }

    #[test]
    fn save_tokens_preserves_unknown_fields() {
        let _g = ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        let cfg = tmp.path().join("acct");
        fs::create_dir_all(&cfg).unwrap();
        // The EXACT field set Claude Code writes in practice: Houston models
        // only accessToken/refreshToken/expiresAt, so the other four are
        // "unknown" to it and a refresh must not drop them — Claude Code reads
        // this same file, and losing refreshTokenExpiresAt or scopes would
        // strand the account.
        fs::write(
            cfg.join(".credentials.json"),
            r#"{"other":"keep","claudeAiOauth":{"accessToken":"old","expiresAt":1,"rateLimitTier":"default_claude_max_20x",
                "refreshToken":"r0","refreshTokenExpiresAt":1799999999999,"scopes":["user:inference","user:profile"],
                "subscriptionType":"max"}}"#,
        )
        .unwrap();
        let a = Account {
            id: "x".into(),
            config_dir: cfg.to_string_lossy().into_owned(),
            ..Default::default()
        };

        a.save_tokens("new", "r1", 42).unwrap();
        let v: Value =
            serde_json::from_slice(&fs::read(cfg.join(".credentials.json")).unwrap()).unwrap();
        assert_eq!(v["other"], "keep"); // top-level survives
        assert_eq!(v["claudeAiOauth"]["accessToken"], "new");
        assert_eq!(v["claudeAiOauth"]["scopes"][0], "user:inference"); // nested survives
        assert_eq!(v["claudeAiOauth"]["scopes"][1], "user:profile");
        assert_eq!(v["claudeAiOauth"]["subscriptionType"], "max");
        assert_eq!(v["claudeAiOauth"]["expiresAt"], 42);
        // The two fields the old test never covered:
        assert_eq!(v["claudeAiOauth"]["rateLimitTier"], "default_claude_max_20x");
        assert_eq!(v["claudeAiOauth"]["refreshTokenExpiresAt"], 1799999999999u64);
        // Exactly the 7 oauth keys went in and 7 came out — nothing added or lost.
        assert_eq!(v["claudeAiOauth"].as_object().unwrap().len(), 7);

        let c = a.credential().unwrap();
        assert_eq!(c.access_token, "new");
        assert_eq!(c.expires_at, 42);
    }

    #[test]
    fn resolve_config_dir_conventional_and_explicit() {
        let a = Account { id: "work".into(), ..Default::default() };
        let d = a.resolve_config_dir().unwrap();
        assert!(d.ends_with("account-work"));
        let b = Account {
            id: "work".into(),
            config_dir: "C:/elsewhere".into(),
            ..Default::default()
        };
        assert_eq!(b.resolve_config_dir().unwrap(), PathBuf::from("C:/elsewhere"));
        assert!(Account::default().resolve_config_dir().is_none());
    }
}
