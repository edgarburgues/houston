//! Probes Anthropic's OAuth usage endpoint to balance accounts by quota
//! pressure. Pressure is the effective load of the bottleneck window: each
//! window (5h, 7d) contributes its utilization attenuated by how much of its
//! period is left before the reset — a window about to renew barely counts,
//! one far from reset counts fully — and the account's pressure is the worse
//! of the two. A saturated window that isn't about to reset is a hard blocker.
//! If probing fails, ranking degrades gracefully to least-recently-used.

use crate::accounts::Account;
use crate::flock;
use crate::oauth;
use crate::paths::store_dir;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::time::Duration;

const USAGE_URL: &str = "https://api.anthropic.com/api/oauth/usage";
const USAGE_BETA: &str = "oauth-2025-04-20";

const WIN5H_SECS: f64 = 5.0 * 3600.0;
const WIN7D_SECS: f64 = 7.0 * 24.0 * 3600.0;

/// Utilization (%) at or above which a window blocks the account right now.
///
/// Public because the UI must decide "is this account out?" with the SAME number
/// the launcher uses. Two thresholds would let the display disagree with the
/// behaviour — an account shown as fine that never gets picked, or the reverse.
pub const SATURATED: f64 = 90.0;
/// A saturated window resetting within this is "about to free up"; further out
/// is a hard blocker now.
const SATURATION_GRACE_SECS: i64 = 15 * 60;
/// Probes within this many pressure points count as tied (ties → LRU).
const TIE_BAND: f64 = 2.0;
/// How long a probe waits for another process's refresh of the same account.
const REFRESH_LOCK_WAIT: Duration = Duration::from_secs(10);

/// One account's usage result.
#[derive(Debug, Clone, Default)]
pub struct Probe {
    pub account: Account,
    pub u5: f64,
    pub u7: f64,
    pub reset5: Option<DateTime<Utc>>,
    pub reset7: Option<DateTime<Utc>>,
    pub pressure: f64,
    pub ok: bool,
    pub err: String,
}

enum ProbeErr {
    Unauthorized,
    Msg(String),
}

struct Windows {
    u5: f64,
    u7: f64,
    r5: Option<DateTime<Utc>>,
    r7: Option<DateTime<Utc>>,
}

/// Query usage for a single token. An empty token fails fast.
fn probe_token(token: &str, timeout: Duration) -> Result<Windows, ProbeErr> {
    if token.is_empty() {
        return Err(ProbeErr::Msg("no credential (account not logged in)".into()));
    }
    let resp = ureq::get(USAGE_URL)
        .timeout(timeout)
        .set("Authorization", &format!("Bearer {token}"))
        .set("anthropic-beta", USAGE_BETA)
        .set("User-Agent", "houston/2.0")
        .call();
    let resp = match resp {
        Ok(r) => r,
        Err(ureq::Error::Status(401, _)) => return Err(ProbeErr::Unauthorized),
        Err(ureq::Error::Status(code, _)) => return Err(ProbeErr::Msg(format!("usage HTTP {code}"))),
        Err(e) => return Err(ProbeErr::Msg(format!("usage request failed: {e}"))),
    };
    let text = resp.into_string().map_err(|e| ProbeErr::Msg(format!("reading usage: {e}")))?;
    parse_usage(&text).map_err(ProbeErr::Msg)
}

/// Decode the usage endpoint's JSON into the two windows.
///
/// Every field is an Option ON PURPOSE: the endpoint sends an explicit `null`
/// for an untouched window's `resets_at` (and may for `utilization`). Go's json
/// decoder silently treats null as the zero value, so v1 tolerated it; serde's
/// `default` only covers ABSENT fields, so a bare `String` here REJECTS null and
/// fails the whole probe — which is what made healthy accounts read as "off".
/// Pure, so the shape is unit-tested without the network.
fn parse_usage(text: &str) -> Result<Windows, String> {
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Win {
        utilization: Option<f64>,
        resets_at: Option<String>,
    }
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Resp {
        five_hour: Win,
        seven_day: Win,
    }
    let r: Resp = serde_json::from_str(text).map_err(|e| format!("decoding usage: {e}"))?;
    Ok(Windows {
        u5: r.five_hour.utilization.unwrap_or(0.0),
        u7: r.seven_day.utilization.unwrap_or(0.0),
        r5: parse_reset(r.five_hour.resets_at.as_deref().unwrap_or_default()),
        r7: parse_reset(r.seven_day.resets_at.as_deref().unwrap_or_default()),
    })
}

fn parse_reset(s: &str) -> Option<DateTime<Utc>> {
    if s.is_empty() {
        return None;
    }
    DateTime::parse_from_rfc3339(s).ok().map(|t| t.with_timezone(&Utc))
}

/// How much of a window's period remains before it resets, clamped to [0,1].
/// Unknown reset yields 0 weight.
fn remaining_fraction(reset: Option<DateTime<Utc>>, now: DateTime<Utc>, period_secs: f64) -> f64 {
    let Some(reset) = reset else { return 0.0 };
    let frac = (reset - now).num_seconds() as f64 / period_secs;
    frac.clamp(0.0, 1.0)
}

/// A saturated window keeps the account unusable beyond the grace period.
fn blocked_for(reset: Option<DateTime<Utc>>, now: DateTime<Utc>) -> bool {
    matches!(reset, Some(r) if (r - now).num_seconds() > SATURATION_GRACE_SECS)
}

/// One window's effective load: utilization attenuated by remaining period.
/// Unknown reset → an untouched window (0%) contributes nothing; a used one
/// counts in full (conservative — we can't know how soon it frees up).
fn eff_load(u: f64, reset: Option<DateTime<Utc>>, now: DateTime<Utc>, period_secs: f64) -> f64 {
    if reset.is_none() {
        return u;
    }
    remaining_fraction(reset, now, period_secs) * u
}

/// Effective load of the bottleneck window, with saturated-and-not-resetting
/// windows floored at their own value (a real block can't be discounted away).
fn weighted_pressure(w: &Windows, now: DateTime<Utc>) -> f64 {
    let mut p = eff_load(w.u5, w.r5, now, WIN5H_SECS).max(eff_load(w.u7, w.r7, now, WIN7D_SECS));
    if w.u5 >= SATURATED && blocked_for(w.r5, now) {
        p = p.max(w.u5);
    }
    if w.u7 >= SATURATED && blocked_for(w.r7, now) {
        p = p.max(w.u7);
    }
    p
}

/// Whether a unix-ms expiry has passed, with a margin so a token about to die
/// mid-probe already counts as expired. Zero (unknown) is not expired.
fn expired_ms(ms: i64, now: DateTime<Utc>) -> bool {
    const MARGIN: i64 = 60_000;
    ms != 0 && now.timestamp_millis() > ms - MARGIN
}

/// Renew an account's access token and PERSIST it before returning, serialized
/// across processes by the per-account lock, re-reading the credential under
/// the lock so a token another process just minted is reused instead of
/// burning the rotating refresh token again.
fn refresh_and_save(a: &Account, stale: &str, timeout: Duration, now: DateTime<Utc>) -> Result<String, String> {
    let lock_path = a.lock_path().ok_or_else(|| "account has no config dir".to_string())?;
    let _lk = flock::acquire(&lock_path, REFRESH_LOCK_WAIT)
        .map_err(|e| format!("credential busy in another process: {e}"))?;
    if let Some(c) = a.credential() {
        if !c.access_token.is_empty() && c.access_token != stale && !expired_ms(c.expires_at, now) {
            return Ok(c.access_token); // freshly refreshed elsewhere: reuse
        }
        if c.refresh_token.is_empty() {
            return Err(format!("token expired and no refresh token — re-login: houston run -a {}", a.id));
        }
        let t = oauth::refresh(&c.refresh_token, timeout)
            .map_err(|e| format!("refresh rejected (re-login: houston run -a {}): {e}", a.id))?;
        persist_tokens(a, &t)?;
        Ok(t.access_token)
    } else {
        Err("no credential (account not logged in)".into())
    }
}

/// How many times to try writing a freshly refreshed credential.
const PERSIST_TRIES: u32 = 3;

/// Write refreshed tokens, retrying briefly.
///
/// This is the most consequential write in Houston. A refresh **rotates** the
/// token server-side: the old refresh token is spent the instant the new one is
/// issued, so a refresh that succeeds and then fails to persist strands the
/// account until somebody logs in again by hand. The realistic causes of a failed
/// write here are transient — an antivirus holding the file open for a few
/// milliseconds, a momentary sharing violation on Windows — so giving up on the
/// first error is throwing away an account over something that passes on its own.
///
/// If it still fails, the error says exactly what happened and what to run, which
/// is the least a message can do when the state is unrecoverable.
fn persist_tokens(a: &Account, t: &oauth::Tokens) -> Result<(), String> {
    let mut last = String::new();
    for attempt in 0..PERSIST_TRIES {
        match a.save_tokens(&t.access_token, &t.refresh_token, t.expires_at) {
            Ok(()) => return Ok(()),
            Err(e) => {
                last = e.to_string();
                if attempt + 1 < PERSIST_TRIES {
                    std::thread::sleep(Duration::from_millis(50 * (attempt as u64 + 1)));
                }
            }
        }
    }
    Err(format!(
        "the token was refreshed but could NOT be saved after {PERSIST_TRIES} tries ({last}). \
         The old token is already spent, so account {} needs a fresh login: houston run -a {}",
        a.id, a.id
    ))
}

/// Fetch one account's usage, transparently refreshing an expired/rejected
/// access token so idle accounts keep reporting real usage between logins.
fn probe_account(a: &Account, timeout: Duration, now: DateTime<Utc>) -> Probe {
    let mut p = Probe { account: a.clone(), ..Default::default() };
    let Some(c) = a.credential() else {
        p.err = "no credential (account not logged in)".into();
        return p;
    };
    let mut token = c.access_token.clone();
    let mut refreshed = false;
    if expired_ms(c.expires_at, now) {
        match refresh_and_save(a, &c.access_token, timeout, now) {
            Ok(t) => {
                token = t;
                refreshed = true;
            }
            Err(e) => {
                p.err = e;
                return p;
            }
        }
    }
    let mut result = probe_token(&token, timeout);
    if matches!(result, Err(ProbeErr::Unauthorized)) && !refreshed {
        // expiresAt said valid but the endpoint rejects it (rotated/revoked):
        // one refresh (reusing an externally refreshed token) then retry.
        if let Ok(t) = refresh_and_save(a, &token, timeout, now) {
            result = probe_token(&t, timeout);
        }
    }
    match result {
        Ok(w) => {
            p.pressure = weighted_pressure(&w, now);
            p.u5 = w.u5;
            p.u7 = w.u7;
            p.reset5 = w.r5;
            p.reset7 = w.r7;
            p.ok = true;
        }
        Err(ProbeErr::Unauthorized) => p.err = "unauthorized (re-login)".into(),
        Err(ProbeErr::Msg(m)) => p.err = m,
    }
    p
}

/// Probe every account in parallel.
pub fn probe_all(accs: &[Account], timeout: Duration) -> Vec<Probe> {
    let now = Utc::now();
    let handles: Vec<_> = accs
        .iter()
        .map(|a| {
            let a = a.clone(); // moved into the thread (must be 'static)
            std::thread::spawn(move || probe_account(&a, timeout, now))
        })
        .collect();
    handles.into_iter().filter_map(|h| h.join().ok()).collect()
}

/// How an account came to be chosen. Carried with the decision instead of being
/// re-derived by each caller: the journal and `houston usage --pick` must say the
/// same word for the same event, and two independently written explanations
/// would eventually disagree about what actually happened.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Pick {
    /// The user named the account.
    Forced,
    /// Ranked from cached readings — no network involved.
    Cached,
    /// Ranked from a live probe.
    Probed,
    /// No reading anywhere: least-recently-used, which spreads load but knows
    /// nothing about quota.
    LruFallback,
    /// No account is logged in, so the config dir that physically holds the
    /// transcript is used.
    TranscriptOwner,
}

impl Pick {
    /// The token written to the journal.
    pub fn label(self) -> &'static str {
        match self {
            Pick::Forced => "forced",
            Pick::Cached => "cache",
            Pick::Probed => "probe",
            Pick::LruFallback => "lru",
            Pick::TranscriptOwner => "transcript-owner",
        }
    }

    /// The same fact, in a sentence, for a human reading `--pick` or `journal`.
    pub fn explain(self) -> &'static str {
        match self {
            Pick::Forced => "you named this account",
            Pick::Cached => "lowest pressure among usable accounts, from the cached readings",
            Pick::Probed => "lowest pressure among usable accounts, from a live probe",
            Pick::LruFallback => "no quota reading was available, so the least recently used account was taken",
            Pick::TranscriptOwner => "no account is logged in, so the config dir that owns the transcript was used",
        }
    }
}

/// A chosen account, the readings behind it, and how it was chosen.
#[derive(Debug, Clone)]
pub struct Decision {
    pub account: Account,
    pub probes: Vec<Probe>,
    pub how: Pick,
}

/// Select the account to launch: lowest pressure among usable successful
/// probes.
///
/// When NOTHING probes successfully the fallback is the *cache*, not plain LRU.
/// That distinction is the whole difference between "spread the load" and
/// "rotate into an account that has no quota left": a failed probe round says
/// nothing about quota, while the cache still remembers that two accounts are at
/// 100 % of the weekly window. Only with no reading anywhere does LRU decide.
pub fn best(accs: &[Account], timeout: Duration) -> Option<Decision> {
    if accs.is_empty() {
        return None;
    }
    let probes = probe_all(accs, timeout);
    let ok: Vec<&Probe> = probes.iter().filter(|p| p.ok).collect();
    if !ok.is_empty() {
        let account = pick_best(&ok);
        return Some(Decision { account, probes, how: Pick::Probed });
    }
    if let Some(d) = best_cached(accs) {
        // Report the probes we just made (they carry the error text), but the
        // decision itself came from the cache — say so.
        return Some(Decision { probes, ..d });
    }
    Some(Decision { account: lru_first(accs), probes, how: Pick::LruFallback })
}

/// Select an account from the CACHE — no network, no stall, and no extra load on
/// an endpoint that rate-limits.
///
/// `None` when no account has a usable cached reading, which is the caller's cue
/// that a real probe is the only way to decide. Ranking goes through the exact
/// same `pick_best` as a live decision: two ranking paths would eventually
/// disagree, and then the statusline would show one thing while Enter did
/// another.
pub fn best_cached(accs: &[Account]) -> Option<Decision> {
    if accs.is_empty() {
        return None;
    }
    let probes = cached_probes(accs, &read_cache(), Utc::now());
    let ok: Vec<&Probe> = probes.iter().filter(|p| p.ok).collect();
    if ok.is_empty() {
        return None;
    }
    let account = pick_best(&ok);
    Some(Decision { account, probes, how: Pick::Cached })
}

/// Rebuild probe-shaped rows from cached values, pressure included, so cached
/// and live decisions can share one ranking function.
fn cached_probes(accs: &[Account], cache: &Cache, now: DateTime<Utc>) -> Vec<Probe> {
    accs.iter()
        .map(|a| {
            let e = cache.get(&a.id).cloned().unwrap_or_default();
            let w = Windows {
                u5: e.u5,
                u7: e.u7,
                r5: (e.r5 != 0).then(|| DateTime::from_timestamp(e.r5, 0)).flatten(),
                r7: (e.r7 != 0).then(|| DateTime::from_timestamp(e.r7, 0)).flatten(),
            };
            Probe {
                account: a.clone(),
                u5: w.u5,
                u7: w.u7,
                reset5: w.r5,
                reset7: w.r7,
                pressure: weighted_pressure(&w, now),
                ok: e.ok,
                // The cached reason if there is one; "no cached reading" only when
                // the cache genuinely holds nothing to explain, which is a
                // different situation from a probe that failed for a known cause.
                err: match (e.ok, e.err.is_empty()) {
                    (_, false) => e.err.clone(),
                    (true, true) => String::new(),
                    (false, true) => "no cached reading".into(),
                },
            }
        })
        .collect()
}

fn usable_now(p: &Probe) -> bool {
    p.u5 < SATURATED && p.u7 < SATURATED
}

fn pick_best(ok: &[&Probe]) -> Account {
    let usable: Vec<&Probe> = ok.iter().copied().filter(|p| usable_now(p)).collect();
    let pool: Vec<&Probe> = if usable.is_empty() { ok.to_vec() } else { usable };
    let min_pressure = pool.iter().map(|p| p.pressure).fold(f64::INFINITY, f64::min);
    let tied: Vec<Account> = pool
        .iter()
        .filter(|p| p.pressure <= min_pressure + TIE_BAND)
        .map(|p| p.account.clone())
        .collect();
    lru_first(&tied)
}

fn lru_first(accs: &[Account]) -> Account {
    accs.iter()
        .min_by(|a, b| a.last_use.cmp(&b.last_use))
        .cloned()
        .unwrap_or_default()
}

// --- cache for the statusline / quota widget (avoids a probe per render) -----

/// How long a last-known-good value survives consecutive probe failures before
/// the account shows as off. It bridges transient failures (a 429 from probing
/// too often, a network blip) without freezing a stale percentage forever after
/// a logout or a revoked token.
const KEEP_GOOD_MAX_SECS: i64 = 10 * 60;

#[derive(Serialize, Deserialize, Default, Clone)]
struct CacheEntry {
    u5: f64,
    u7: f64,
    ok: bool,
    /// Why the last probe failed, verbatim, empty when it succeeded.
    ///
    /// Cached rather than recomputed because it is the answer to the only
    /// question an account showing "off" raises — *what do I do about it?* — and
    /// the reader (a status line render, `houston usage`) is forbidden from
    /// probing to find out. It used to be captured on `Probe` and dropped on the
    /// way into this struct, so Houston knew the account needed a re-login and
    /// displayed a bare "off".
    ///
    /// Kept even while a last-known-good value is still being served: "the number
    /// is from 4 minutes ago AND the token is now rejected" is two facts, and the
    /// second is the actionable one.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    err: String,
    /// Unix seconds of the last probe ATTEMPT.
    ts: i64,
    /// Unix seconds of the last SUCCESSFUL probe.
    #[serde(default, skip_serializing_if = "is_zero")]
    good_ts: i64,
    /// Unix seconds at which each window resets (0 = unknown). Cached because
    /// "when does it come back?" is the FIRST thing you want to know about an
    /// account that has no room left, and re-probing to answer it would defeat
    /// the point of the cache.
    #[serde(default, skip_serializing_if = "is_zero")]
    r5: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    r7: i64,
}

/// One account's cached quota, as the UI consumes it.
#[derive(Debug, Clone, Default)]
pub struct Usage {
    pub id: String,
    /// 5-hour window utilization (%).
    pub u5: f64,
    /// 7-day window utilization (%).
    pub u7: f64,
    /// Unix seconds at which each window resets, when the endpoint says.
    pub reset5: Option<i64>,
    pub reset7: Option<i64>,
    /// False when the last probe failed beyond the grace window.
    pub ok: bool,
    /// Why the last probe failed, empty when it succeeded. Present even when `ok`
    /// is still true on a cached value, because a token rejected four minutes ago
    /// is news whether or not the percentage is still being shown.
    pub err: String,
}

impl Usage {
    /// Whether the cached failure is one a re-login fixes, as opposed to a
    /// network blip or an unknown error.
    ///
    /// One classifier, in core, so the status line and the TUI cannot disagree
    /// about which accounts are asking for a login. It matches on the remedy
    /// strings this crate itself writes (`re-login`, `not logged in`), which is
    /// the one vocabulary that is ours rather than the endpoint's.
    pub fn needs_login(&self) -> bool {
        self.err.contains("re-login") || self.err.contains("not logged in")
    }
}

impl Usage {
    /// Seconds until the given window resets, if known, still ahead, and no
    /// further away than the window itself allows.
    ///
    /// That last bound is not defensiveness for its own sake: a 5-hour window
    /// cannot reset more than 5 hours from now, and a 7-day one not more than 7
    /// days, so anything beyond is a wrong number rather than a distant one. The
    /// failure it catches is concrete — a `resets_at` in **milliseconds** reads as
    /// an epoch ~50,000 years out and would render as `↻ 95055d`. Claude already
    /// mixes units across its own payloads (`agents --json` reports `startedAt` in
    /// millis), so this is a mistake with a precedent, not a hypothetical.
    ///
    /// Out-of-range returns None — "I cannot say when" — rather than a clamp,
    /// which would state a time Houston does not actually know.
    pub fn resets_in(&self, weekly: bool, now: i64) -> Option<i64> {
        let at = if weekly { self.reset7 } else { self.reset5 }?;
        let left = (at > now).then_some(at - now)?;
        (left <= window_secs(weekly)).then_some(left)
    }

    /// Replace this account's figures with the ones Claude reported for the
    /// session being rendered.
    ///
    /// Only ever applied to the ACTIVE account: the numbers arrive from the
    /// session's own API responses, so they describe that account and no other.
    /// Each field is taken only when present — Claude documents every window as
    /// independently absent, and an absent window must leave the cached value
    /// alone rather than blank a bar that was right.
    ///
    /// `ok` is deliberately untouched. It records whether Houston's own probe
    /// can still reach the endpoint, which is what decides whether the LAUNCHER
    /// trusts this account; a fresh number from a session already running says
    /// nothing about that.
    pub fn overlay(&mut self, live: &LiveWindows) {
        if let Some(v) = live.u5 {
            self.u5 = v;
        }
        if let Some(v) = live.u7 {
            self.u7 = v;
        }
        if live.reset5.is_some() {
            self.reset5 = live.reset5;
        }
        if live.reset7.is_some() {
            self.reset7 = live.reset7;
        }
    }
}

/// The quota Claude pipes into the status line on stdin, for the account that
/// session is signed in as.
///
/// Why this exists: Houston's own figures come from a cache a detached
/// refresher fills, so the account you are actively burning is the one whose
/// number is most likely to be behind — the render cannot probe (see
/// `statusline`), and the refresh it triggers lands one render later. Claude
/// already knows, exactly, and hands it over for free. Every field is optional:
/// `rate_limits` appears only for claude.ai subscribers and only after the
/// session's first API response, and each window may be absent on its own.
#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct LiveWindows {
    pub u5: Option<f64>,
    pub u7: Option<f64>,
    /// Unix **seconds** — the unit Claude documents for `resets_at`, and the
    /// same unit as `Usage::reset5`/`reset7`, so no conversion is right here.
    pub reset5: Option<i64>,
    pub reset7: Option<i64>,
}

impl LiveWindows {
    /// Read `rate_limits` out of the status line's stdin payload.
    ///
    /// Field names are Claude's (`five_hour`/`seven_day`, `used_percentage`,
    /// `resets_at`) and are snake_case where the rest of that payload is too.
    /// Anything missing or of the wrong type reads as absent, never as zero: a
    /// renamed field must degrade to "use the cache", which is what Houston did
    /// before this existed.
    pub fn from_statusline_json(v: &serde_json::Value) -> Self {
        let win = |name: &str| -> (Option<f64>, Option<i64>) {
            let w = v.get("rate_limits").and_then(|r| r.get(name));
            (
                w.and_then(|w| w.get("used_percentage")).and_then(serde_json::Value::as_f64),
                w.and_then(|w| w.get("resets_at")).and_then(serde_json::Value::as_i64),
            )
        };
        let (u5, reset5) = win("five_hour");
        let (u7, reset7) = win("seven_day");
        Self { u5, u7, reset5, reset7 }
    }
}

/// How long each quota window is, which is also the longest a countdown against
/// it can legitimately be.
fn window_secs(weekly: bool) -> i64 {
    if weekly { 7 * 86_400 } else { 5 * 3_600 }
}

/// A compact "comes back in" label: `45m`, `7h`, `6d`. None when unknown or
/// already past.
pub fn resets_in_label(secs: Option<i64>) -> Option<String> {
    let s = secs?;
    if s <= 0 {
        return None;
    }
    Some(if s < 3600 {
        format!("{}m", s / 60)
    } else if s < 86_400 {
        format!("{}h", s / 3600)
    } else {
        format!("{}d", s / 86_400)
    })
}

fn is_zero(n: &i64) -> bool {
    *n == 0
}

/// When an entry's value was actually probed OK. Entries written before
/// `good_ts` existed fall back to their write time.
fn good_ts(e: &CacheEntry) -> i64 {
    if e.good_ts != 0 {
        e.good_ts
    } else {
        e.ts
    }
}

/// Fold a fresh probe over the previous cache entry: a success refreshes the
/// value and its good_ts; a failure KEEPS the last-known-good value while it's
/// younger than KEEP_GOOD_MAX_SECS, and only then shows the account as off.
fn merge_probe(prev: Option<&CacheEntry>, p: &Probe, now: i64) -> CacheEntry {
    if p.ok {
        return CacheEntry {
            u5: p.u5,
            u7: p.u7,
            ok: true,
            // A success clears the reason: the account is reachable again, and a
            // stale "unauthorized" would keep telling the user to re-login after
            // they already did.
            err: String::new(),
            ts: now,
            good_ts: now,
            r5: p.reset5.map(|t| t.timestamp()).unwrap_or(0),
            r7: p.reset7.map(|t| t.timestamp()).unwrap_or(0),
        };
    }
    if let Some(prev) = prev {
        if prev.ok && now - good_ts(prev) <= KEEP_GOOD_MAX_SECS {
            // Keep the reset times along with the value they belong to — and the
            // fresh failure alongside them, because the grace window hides the
            // problem from `ok` and this is the only place it survives.
            return CacheEntry { ts: now, good_ts: good_ts(prev), err: p.err.clone(), ..prev.clone() };
        }
    }
    CacheEntry { ts: now, err: p.err.clone(), ..Default::default() }
}

fn cache_path() -> std::path::PathBuf {
    store_dir().join("usage-cache-v2.json")
}

type Cache = std::collections::HashMap<String, CacheEntry>;

fn read_cache() -> Cache {
    std::fs::read(cache_path())
        .ok()
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default()
}

fn project(accs: &[Account], cache: &Cache) -> Vec<Usage> {
    accs.iter()
        .map(|a| {
            let e = cache.get(&a.id).cloned().unwrap_or_default();
            Usage {
                id: a.id.clone(),
                u5: e.u5,
                u7: e.u7,
                reset5: (e.r5 != 0).then_some(e.r5),
                reset7: (e.r7 != 0).then_some(e.r7),
                ok: e.ok,
                err: e.err,
            }
        })
        .collect()
}

/// Accounts whose cached value is older than `ttl` (or missing entirely).
fn stale_ids(accs: &[Account], cache: &Cache, ttl: Duration, now: i64) -> Vec<Account> {
    accs.iter()
        .filter(|a| {
            cache
                .get(&a.id)
                .map(|e| now - e.ts >= ttl.as_secs() as i64)
                .unwrap_or(true)
        })
        .cloned()
        .collect()
}

/// Quota for every account **from cache only** — no network, no locks, nothing
/// that can block or be cancelled mid-flight.
///
/// This is what the statusline calls. Claude debounces the status line at 300 ms
/// and *kills* a script still running when the next update arrives, so a render
/// that probes is a render that can vanish; the answer is to make rendering a
/// read and refreshing somebody else's job (see `spawn_background_refresh`).
/// A value may therefore be stale, which is the right trade: a slightly old
/// percentage beats an empty line.
pub fn read_utilization(accs: &[Account]) -> Vec<Usage> {
    project(accs, &read_cache())
}

/// Whether any account's cached value has aged past `ttl`.
pub fn is_stale(accs: &[Account], ttl: Duration) -> bool {
    !stale_ids(accs, &read_cache(), ttl, Utc::now().timestamp()).is_empty()
}

/// Probe every account staler than `ttl` and rewrite the cache. Blocking: this
/// is the half that talks to the network, so only callers that can afford to
/// wait (the background refresher, the TUI's off-thread refresh, `usage
/// --refresh`) may call it.
pub fn refresh(accs: &[Account], ttl: Duration, timeout: Duration) {
    let now = Utc::now().timestamp();
    let mut cache = read_cache();
    let stale = stale_ids(accs, &cache, ttl, now);
    if stale.is_empty() {
        return;
    }
    // Single-flight across PROCESSES: with several Claude sessions open every
    // one renders a statusline, and the moment the cache goes stale they'd
    // all probe (and possibly refresh tokens) at once — which gets the
    // endpoint to rate-limit us and every account then reads as "off".
    // Whoever wins the lock probes; the rest serve the previous cache.
    let path = cache_path();
    let _ = std::fs::create_dir_all(path.parent().unwrap_or(std::path::Path::new(".")));
    let lock_path = path.with_extension("json.lock");
    if let Some(_lk) = flock::try_acquire(&lock_path) {
        for p in probe_all(&stale, timeout) {
            let merged = merge_probe(cache.get(&p.account.id), &p, now);
            cache.insert(p.account.id.clone(), merged);
        }
        write_cache(&cache);
    }
}

/// Cached quota for every account, refreshing inline what has gone stale. The
/// TUI and the launcher use this — they run off the render path and may wait.
/// **Not** for the statusline: use `read_utilization` there.
pub fn cached_utilization(accs: &[Account], ttl: Duration, timeout: Duration) -> Vec<Usage> {
    refresh(accs, ttl, timeout);
    read_utilization(accs)
}

// --- background refresh (what keeps a pure-read statusline from freezing) -----

/// How often a background refresh may be *started*, across all processes. The
/// probe itself is single-flighted by the cache lock, so this only exists to
/// stop a burst of renders spawning a burst of processes: while the cache is
/// stale, every session's statusline would otherwise claim it at once.
const SPAWN_THROTTLE: Duration = Duration::from_secs(10);

fn stamp_path() -> std::path::PathBuf {
    store_dir().join("usage-refresh.stamp")
}

/// True at most once per `SPAWN_THROTTLE`, and only when something is stale:
/// the caller that gets `true` owns starting a refresh.
///
/// The stamp is written *before* the refresh runs on purpose. A crashed or
/// killed refresher must not be able to make this return `true` on every
/// subsequent render — the throttle has to hold even when the work never
/// finishes, and the cost of a stamp for a refresh that never happened is one
/// extra stale interval.
pub fn claim_background_refresh(accs: &[Account], ttl: Duration) -> bool {
    let now = Utc::now().timestamp();
    if stale_ids(accs, &read_cache(), ttl, now).is_empty() {
        return false;
    }
    let path = stamp_path();
    let last = std::fs::read_to_string(&path).ok().and_then(|s| s.trim().parse::<i64>().ok()).unwrap_or(0);
    if now - last < SPAWN_THROTTLE.as_secs() as i64 {
        return false;
    }
    let _ = std::fs::create_dir_all(path.parent().unwrap_or(std::path::Path::new(".")));
    let tmp = path.with_extension(format!("stamp.{}.tmp", std::process::id()));
    if std::fs::write(&tmp, now.to_string()).is_ok() && std::fs::rename(&tmp, &path).is_err() {
        let _ = std::fs::remove_file(&tmp);
    }
    true
}

/// Start `houston usage --refresh` detached and return at once.
///
/// Nothing waits for it: the render that spawned it prints the cache as it
/// stands and exits, and the fresh value lands for the *next* render seconds
/// later. If Claude kills the child along with the cancelled parent, nothing is
/// left broken — the cache is written by rename, and the single-flight lock is
/// an OS lock the kernel releases when its owner dies.
///
/// `force` ignores the TTL, which is what a `rate_limit` hook needs: the cache
/// was written seconds ago and is now *known* to be wrong, so respecting its
/// freshness would keep an exhausted account looking free.
pub fn spawn_background_refresh(force: bool) {
    let exe = std::env::current_exe().unwrap_or_else(|_| std::path::PathBuf::from("houston"));
    let mut cmd = std::process::Command::new(exe);
    cmd.args(["usage", "--refresh"]);
    if force {
        cmd.arg("--force");
    }
    cmd
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null());
    #[cfg(windows)]
    {
        // The statusline renders with no console of its own; without these the
        // refresher would flash a window every minute of every session.
        use std::os::windows::process::CommandExt;
        const DETACHED_PROCESS: u32 = 0x0000_0008;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        cmd.creation_flags(DETACHED_PROCESS | CREATE_NO_WINDOW);
    }
    let _ = cmd.spawn();
}

/// Write the cache atomically (see `atomic`: unique temp + rename). A failed
/// write is dropped rather than reported — the cache is an optimisation, and the
/// next refresh tries again.
fn write_cache(cache: &std::collections::HashMap<String, CacheEntry>) {
    let Ok(b) = serde_json::to_vec(cache) else { return };
    let _ = crate::atomic::write(&cache_path(), &b);
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;

    fn t(hours_from_now: i64, now: DateTime<Utc>) -> Option<DateTime<Utc>> {
        Some(now + chrono::Duration::hours(hours_from_now))
    }

    fn now() -> DateTime<Utc> {
        Utc.with_ymd_and_hms(2026, 7, 24, 12, 0, 0).unwrap()
    }

    #[test]
    fn fresh_5h_window_is_attenuated_toward_reset() {
        let n = now();
        // 80% used, resets in ~5min (period 5h) → almost weightless.
        let w = Windows { u5: 80.0, u7: 0.0, r5: Some(n + chrono::Duration::minutes(5)), r7: None };
        assert!(weighted_pressure(&w, n) < 5.0, "a window about to reset barely counts");
    }

    #[test]
    fn saturated_window_far_from_reset_is_a_hard_block() {
        let n = now();
        // 95% used, resets in 4h → floored at its own value despite attenuation.
        let w = Windows { u5: 95.0, u7: 0.0, r5: t(4, n), r7: None };
        assert!(weighted_pressure(&w, n) >= 95.0, "a real block can't be discounted");
    }

    #[test]
    fn unknown_reset_counts_used_window_in_full_but_idle_as_zero() {
        let n = now();
        let idle = Windows { u5: 0.0, u7: 0.0, r5: None, r7: None };
        assert_eq!(weighted_pressure(&idle, n), 0.0);
        let used = Windows { u5: 40.0, u7: 0.0, r5: None, r7: None };
        assert_eq!(weighted_pressure(&used, n), 40.0);
    }

    #[test]
    fn pick_best_prefers_usable_now_then_pressure_then_lru() {
        let mk = |id: &str, u5: f64, u7: f64, pressure: f64, last: &str| Probe {
            account: Account { id: id.into(), last_use: last.into(), ..Default::default() },
            u5,
            u7,
            pressure,
            ok: true,
            ..Default::default()
        };
        // 'maxed' is saturated (unusable now) despite low pressure; 'a'/'b' tie
        // on pressure and 'b' is less-recently-used → wins.
        let maxed = mk("maxed", 95.0, 10.0, 1.0, "@1");
        let a = mk("a", 30.0, 10.0, 30.0, "@9");
        let b = mk("b", 31.0, 10.0, 31.0, "@2");
        let ok = vec![&maxed, &a, &b];
        assert_eq!(pick_best(&ok).id, "b");
    }

    #[test]
    fn null_resets_at_is_tolerated_like_go() {
        // The exact shape that made a healthy account read as "off": an
        // untouched 5h window reports resets_at: null.
        let real = r#"{"five_hour":{"utilization":0.0,"resets_at":null,"limit_dollars":null},
                       "seven_day":{"utilization":100.0,"resets_at":"2026-07-29T12:00:00+00:00"}}"#;
        let w = parse_usage(real).expect("null resets_at must not fail the probe");
        assert_eq!(w.u5, 0.0);
        assert_eq!(w.u7, 100.0);
        assert!(w.r5.is_none(), "null → unknown reset");
        assert!(w.r7.is_some());
        // A null utilization, and missing windows entirely, also degrade to zero.
        let sparse = parse_usage(r#"{"five_hour":{"utilization":null,"resets_at":null}}"#).unwrap();
        assert_eq!((sparse.u5, sparse.u7), (0.0, 0.0));
        assert!(parse_usage("{}").is_ok());
        assert!(parse_usage("not json").is_err());
    }

    #[test]
    fn failed_probe_keeps_last_good_value_within_grace_then_goes_off() {
        let ok_probe = Probe { u5: 42.0, u7: 7.0, ok: true, ..Default::default() };
        let bad = Probe { ok: false, ..Default::default() };
        // A success records the value and its good_ts.
        let e1 = merge_probe(None, &ok_probe, 1_000);
        assert!(e1.ok && e1.u5 == 42.0 && e1.good_ts == 1_000);
        // A failure 5 min later KEEPS the value (still ok, good_ts preserved).
        let e2 = merge_probe(Some(&e1), &bad, 1_000 + 300);
        assert!(e2.ok, "a transient failure must not immediately read as off");
        assert_eq!(e2.u5, 42.0);
        assert_eq!(e2.good_ts, 1_000);
        // Beyond the grace window it finally goes off.
        let e3 = merge_probe(Some(&e2), &bad, 1_000 + KEEP_GOOD_MAX_SECS + 1);
        assert!(!e3.ok, "a stale-good value must not freeze forever");
        // With no history at all, a failure is off.
        assert!(!merge_probe(None, &bad, 5).ok);
    }

    #[test]
    fn reset_times_survive_the_cache_and_a_transient_failure() {
        let n = now();
        let in_7h = n + chrono::Duration::hours(7);
        let ok_probe = Probe {
            u5: 0.0,
            u7: 100.0,
            reset7: Some(in_7h),
            ok: true,
            ..Default::default()
        };
        let e1 = merge_probe(None, &ok_probe, 1_000);
        assert_eq!(e1.r7, in_7h.timestamp(), "the reset time is cached, not discarded");
        assert_eq!(e1.r5, 0, "an unknown reset stays unknown");

        // A failure inside the grace window keeps the value AND its reset time:
        // reporting "no room" without saying when it returns is the useless half.
        let bad = Probe { ok: false, ..Default::default() };
        let e2 = merge_probe(Some(&e1), &bad, 1_300);
        assert!(e2.ok && e2.u7 == 100.0);
        assert_eq!(e2.r7, in_7h.timestamp());
    }

    #[test]
    fn resets_in_reads_the_right_window_and_ignores_the_past() {
        let u = Usage { u7: 100.0, reset5: Some(1_000), reset7: Some(10_000), ..Default::default() };
        assert_eq!(u.resets_in(true, 4_000), Some(6_000), "weekly window");
        assert_eq!(u.resets_in(false, 500), Some(500), "5h window");
        // A reset already in the past is not a countdown.
        assert_eq!(u.resets_in(false, 4_000), None);
        // An unknown reset yields nothing rather than a bogus 0.
        assert_eq!(Usage::default().resets_in(true, 0), None);
    }

    #[test]
    fn a_reset_further_away_than_its_window_is_not_a_countdown() {
        // The real shape of this bug: `resets_at` arriving in MILLISECONDS. It
        // parses, it is in the future, and it rendered as "↻ 95055d".
        let millis = Usage { reset7: Some(1_800_000_000_000), reset5: Some(1_800_000_000_000), ..Default::default() };
        assert_eq!(millis.resets_in(true, 1_800_000_000), None, "50,000 years is not a weekly reset");
        assert_eq!(millis.resets_in(false, 1_800_000_000), None);

        // The boundary is inclusive: a window that has just reset is legitimately
        // its own full length away.
        let now = 1_000_000;
        let exact = Usage {
            reset7: Some(now + 7 * 86_400),
            reset5: Some(now + 5 * 3_600),
            ..Default::default()
        };
        assert_eq!(exact.resets_in(true, now), Some(7 * 86_400));
        assert_eq!(exact.resets_in(false, now), Some(5 * 3_600));
        // One second past it is not.
        let over = Usage { reset5: Some(now + 5 * 3_600 + 1), ..Default::default() };
        assert_eq!(over.resets_in(false, now), None);
        // …and the same value IS valid against the weekly window, which is longer.
        let as_weekly = Usage { reset7: Some(now + 5 * 3_600 + 1), ..Default::default() };
        assert_eq!(as_weekly.resets_in(true, now), Some(5 * 3_600 + 1));
    }

    /// The failure reason must SURVIVE the cache, because its readers (the status
    /// line render, `houston usage`) are forbidden from probing to find it out.
    /// It used to be captured on `Probe` and dropped on the way in: Houston knew
    /// the account needed a re-login and displayed a bare "off".
    #[test]
    fn the_failure_reason_survives_the_cache() {
        let a = Account { id: "a1".into(), ..Default::default() };
        let failed = Probe { account: a.clone(), ok: false, err: "refresh rejected (re-login): refresh HTTP 400".into(), ..Default::default() };

        // No prior value: the reason is stored, not a blank.
        let e = merge_probe(None, &failed, 1_000);
        assert!(!e.ok);
        assert!(e.err.contains("re-login"), "{}", e.err);

        // Inside the grace window the last good value keeps being served — and
        // the failure is stored ANYWAY. Two facts, and the actionable one is the
        // second: `ok` hides it, this is the only place it survives.
        let good = Probe { account: a.clone(), ok: true, u7: 42.0, ..Default::default() };
        let e1 = merge_probe(None, &good, 1_000);
        let e2 = merge_probe(Some(&e1), &failed, 1_100);
        assert!(e2.ok && e2.u7 == 42.0, "the good value stays");
        assert!(e2.err.contains("HTTP 400"), "and so does the reason: {}", e2.err);

        // A success CLEARS the reason: otherwise it would keep asking for a
        // re-login after the user already did one.
        let e3 = merge_probe(Some(&e2), &good, 1_200);
        assert!(e3.err.is_empty(), "a success clears the reason: {}", e3.err);
    }

    /// The vocabulary `needs_login` recognizes is the one THIS crate writes. If
    /// someone rephrases the error messages above, this is what warns that the
    /// status line would go back to saying "off" where it should say "login".
    #[test]
    fn needs_login_recognizes_the_remedies_we_write() {
        let mk = |err: &str| Usage { err: err.into(), ..Default::default() };
        assert!(mk("refresh rejected (re-login: houston run -a x): refresh HTTP 400").needs_login());
        assert!(mk("token expired and no refresh token — re-login: houston run -a x").needs_login());
        assert!(mk("no credential (account not logged in)").needs_login());
        // A network failure is not fixed by a login, and saying "login" there
        // sends the user to a remedy that cures nothing.
        assert!(!mk("usage request failed: timeout").needs_login());
        assert!(!mk("usage HTTP 500").needs_login());
        assert!(!mk("").needs_login());
    }

    #[test]
    fn live_windows_read_both_windows_out_of_claudes_payload() {
        let v: serde_json::Value = serde_json::from_str(
            r#"{"model":{"display_name":"Opus"},
                "rate_limits":{"five_hour":{"used_percentage":23.5,"resets_at":1738425600},
                               "seven_day":{"used_percentage":41.2,"resets_at":1738857600}}}"#,
        )
        .unwrap();
        let l = LiveWindows::from_statusline_json(&v);
        assert_eq!(l.u5, Some(23.5));
        assert_eq!(l.u7, Some(41.2));
        assert_eq!(l.reset5, Some(1_738_425_600));
        assert_eq!(l.reset7, Some(1_738_857_600));
    }

    #[test]
    fn a_missing_window_reads_as_absent_not_as_zero() {
        // Claude documents each window as independently absent, and `rate_limits`
        // itself as absent for non-subscribers and before the first API response.
        // Reading absence as 0.0 would paint a full green bar for an account at
        // 100 % — the worst possible direction to be wrong in.
        let only5: serde_json::Value =
            serde_json::from_str(r#"{"rate_limits":{"five_hour":{"used_percentage":60}}}"#).unwrap();
        let l = LiveWindows::from_statusline_json(&only5);
        assert_eq!(l.u5, Some(60.0));
        assert_eq!(l.u7, None, "the weekly window was not reported");
        assert_eq!(l.reset5, None, "5h was reported without a reset");

        assert_eq!(LiveWindows::from_statusline_json(&serde_json::Value::Null), LiveWindows::default());
        let renamed: serde_json::Value =
            serde_json::from_str(r#"{"rate_limits":{"five_hour":{"pct":60}}}"#).unwrap();
        assert_eq!(LiveWindows::from_statusline_json(&renamed), LiveWindows::default(), "a renamed field falls back to the cache");
    }

    #[test]
    fn overlay_replaces_what_was_reported_and_keeps_the_rest() {
        let mut u = Usage { id: "a1".into(), u5: 10.0, u7: 99.0, reset5: Some(100), reset7: Some(200), ok: true, err: String::new() };
        u.overlay(&LiveWindows { u7: Some(100.0), reset7: Some(999), ..Default::default() });
        assert_eq!(u.u7, 100.0, "the reported weekly figure wins");
        assert_eq!(u.reset7, Some(999));
        // Untouched: an absent field must not blank a cached value that was right.
        assert_eq!(u.u5, 10.0);
        assert_eq!(u.reset5, Some(100));

        // `ok` is the probe's verdict, not the session's, and stays put: it is
        // what the launcher reads to decide the account can still be reached.
        let mut down = Usage { ok: false, ..Default::default() };
        down.overlay(&LiveWindows { u5: Some(1.0), ..Default::default() });
        assert!(!down.ok);
    }

    #[test]
    fn resets_in_label_scales_the_unit() {
        assert_eq!(resets_in_label(Some(45 * 60)).unwrap(), "45m");
        assert_eq!(resets_in_label(Some(7 * 3600 + 900)).unwrap(), "7h");
        assert_eq!(resets_in_label(Some(6 * 86_400 + 4 * 3600)).unwrap(), "6d");
        // Just under an hour still reads in minutes, not "0h".
        assert_eq!(resets_in_label(Some(3599)).unwrap(), "59m");
        assert!(resets_in_label(None).is_none());
        assert!(resets_in_label(Some(0)).is_none());
        assert!(resets_in_label(Some(-5)).is_none());
    }

    /// The defect this fixes, stated as a test: with two accounts at 100 % of the
    /// weekly window, a decision made from the cache must pick the third one —
    /// every time. Ranking from cached numbers instead of rotating by last-use is
    /// what stops three consecutive resumes landing on three different accounts.
    #[test]
    fn a_cached_decision_skips_saturated_accounts_instead_of_rotating() {
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("HOUSTON_HOME", tmp.path()) };

        let now = Utc::now().timestamp();
        let acc = |id: &str, last: i64| Account { id: id.into(), last_use: format!("@{last}"), ..Default::default() };
        // Exactly this machine's shape: two accounts out for the week (resetting
        // hours away, so the saturation is a real block), one with room. The
        // saturated pair are the LEAST recently used, so plain LRU would pick them.
        let accs = vec![acc("one", 1), acc("two", 2), acc("three", 99)];
        let mut cache = Cache::new();
        for (id, u5, u7, r7) in
            [("one", 0.0, 100.0, now + 8 * 3600), ("two", 0.0, 100.0, now + 4 * 3600), ("three", 17.0, 13.0, 0)]
        {
            cache.insert(id.into(), CacheEntry { u5, u7, ok: true, ts: now, good_ts: now, r5: 0, r7, err: String::new() });
        }
        write_cache(&cache);

        let d = best_cached(&accs).expect("a cache with readings can decide");
        assert_eq!(d.account.id, "three", "the only account with room wins despite being most recently used");
        assert_eq!(d.how, Pick::Cached, "the decision reports where it came from");
        assert!(d.probes.iter().all(|p| p.ok), "every account had a reading");
        // Repeated decisions are stable — no rotation.
        for _ in 0..3 {
            assert_eq!(best_cached(&accs).unwrap().account.id, "three");
        }

        // With no readings at all the cache must say so rather than guess, so the
        // caller knows a real probe is the only way to decide.
        write_cache(&Cache::new());
        assert!(best_cached(&accs).is_none());
        assert!(best_cached(&[]).is_none());

        // Everything saturated: no account is usable, so it falls back to ranking
        // them all — the least-bad, not nothing.
        let mut cache = Cache::new();
        for (id, u7) in [("one", 100.0), ("two", 95.0), ("three", 99.0)] {
            cache.insert(id.into(), CacheEntry { u5: 0.0, u7, ok: true, ts: now, good_ts: now, r5: 0, r7: now + 3600, err: String::new() });
        }
        write_cache(&cache);
        assert_eq!(best_cached(&accs).unwrap().account.id, "two", "lowest pressure of a bad lot");

        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }

    /// The unrecoverable case must at least be *explained*: a rotated token that
    /// could not be written means the account is stranded, and the message has to
    /// say so and say what to run.
    #[test]
    fn an_unpersistable_refresh_says_what_it_broke_and_how_to_fix_it() {
        // An account whose credential file does not exist: every save attempt
        // fails, so this also proves the retry loop terminates.
        let a = Account { id: "stranded".into(), config_dir: "Z:\\definitely\\not\\here".into(), ..Default::default() };
        let t = oauth::Tokens { access_token: "a".into(), refresh_token: "r".into(), expires_at: 0 };
        let err = persist_tokens(&a, &t).expect_err("this cannot succeed");
        assert!(err.contains("could NOT be saved"), "{err}");
        assert!(err.contains("already spent"), "the consequence is stated: {err}");
        assert!(err.contains("houston run -a stranded"), "and the fix is a command: {err}");
    }

    #[test]
    fn stale_ids_covers_missing_old_and_fresh() {
        let now = 10_000;
        let acc = |id: &str| Account { id: id.into(), ..Default::default() };
        let accs = vec![acc("fresh"), acc("old"), acc("unknown")];
        let mut cache = Cache::new();
        cache.insert("fresh".into(), CacheEntry { ts: now - 10, ..Default::default() });
        cache.insert("old".into(), CacheEntry { ts: now - 120, ..Default::default() });
        let stale: Vec<String> = stale_ids(&accs, &cache, Duration::from_secs(60), now).into_iter().map(|a| a.id).collect();
        // An account with no entry at all is stale — otherwise a first run would
        // never probe.
        assert_eq!(stale, vec!["old".to_string(), "unknown".to_string()]);
        // Exactly at the TTL counts as stale (>=), so a 60s TTL refreshes at 60s.
        let at_ttl = stale_ids(&accs[..1], &cache, Duration::from_secs(10), now);
        assert_eq!(at_ttl.len(), 1);
    }

    /// The statusline renders every few hundred ms in every open session; a
    /// stale cache must produce ONE background refresh, not one per render.
    #[test]
    fn background_refresh_is_claimed_once_per_throttle() {
        let _g = crate::TEST_ENV_LOCK.lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        unsafe { std::env::set_var("HOUSTON_HOME", tmp.path()) };

        let accs = vec![Account { id: "a".into(), ..Default::default() }];
        // Nothing cached → stale → the first caller claims it, the next does not.
        assert!(claim_background_refresh(&accs, Duration::from_secs(60)));
        assert!(!claim_background_refresh(&accs, Duration::from_secs(60)));
        // A stamp from before the throttle window lets the next render claim again.
        let old = Utc::now().timestamp() - SPAWN_THROTTLE.as_secs() as i64 - 1;
        std::fs::write(stamp_path(), old.to_string()).unwrap();
        assert!(claim_background_refresh(&accs, Duration::from_secs(60)));

        // A fresh cache is never worth refreshing, throttle or not.
        let mut cache = Cache::new();
        cache.insert("a".into(), CacheEntry { ts: Utc::now().timestamp(), ok: true, ..Default::default() });
        write_cache(&cache);
        std::fs::write(stamp_path(), "0").unwrap();
        assert!(!claim_background_refresh(&accs, Duration::from_secs(60)));
        // And a pure read answers from that cache without touching the network.
        assert!(read_utilization(&accs)[0].ok);
        assert!(!is_stale(&accs, Duration::from_secs(60)));

        unsafe { std::env::remove_var("HOUSTON_HOME") };
    }

    #[test]
    fn expired_ms_margin() {
        let n = now();
        assert!(!expired_ms(0, n)); // unknown → not expired
        assert!(expired_ms(n.timestamp_millis() - 1, n)); // past
        assert!(!expired_ms(n.timestamp_millis() + 120_000, n)); // 2min out → fine
        assert!(expired_ms(n.timestamp_millis() + 30_000, n)); // 30s out → within margin
    }
}
