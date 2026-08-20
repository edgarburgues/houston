//! Refreshes Claude.ai OAuth credentials. It runs the same refresh-token flow
//! Claude Code runs internally — same endpoint, same public client id — so the
//! tokens it obtains are exactly what a live session would write to
//! .credentials.json.

use anyhow::{anyhow, Context};
use std::time::Duration;

/// Anthropic's OAuth token endpoint.
pub const TOKEN_URL: &str = "https://console.anthropic.com/v1/oauth/token";

/// Claude Code's public OAuth client id (embedded in the CLI; not a secret —
/// public clients authenticate with the refresh token alone).
const CLIENT_ID: &str = "9d1c250a-e61b-44d9-88ed-5944d1962f5e";

/// A freshly-minted credential set. `expires_at` is unix milliseconds, the
/// unit .credentials.json uses.
#[derive(Debug, Clone)]
pub struct Tokens {
    pub access_token: String,
    /// May rotate: persist it or the account strands.
    pub refresh_token: String,
    pub expires_at: i64,
}

/// Exchange a refresh token for a new access token. The caller MUST persist
/// the returned `refresh_token`: the endpoint may rotate it, invalidating the
/// one just used.
pub fn refresh(refresh_token: &str, timeout: Duration) -> anyhow::Result<Tokens> {
    refresh_at(TOKEN_URL, refresh_token, timeout)
}

/// Testable core: refresh against an explicit endpoint URL.
pub fn refresh_at(url: &str, refresh_token: &str, timeout: Duration) -> anyhow::Result<Tokens> {
    let body = serde_json::json!({
        "grant_type": "refresh_token",
        "refresh_token": refresh_token,
        "client_id": CLIENT_ID,
    })
    .to_string();

    let resp = ureq::post(url)
        .timeout(timeout)
        .set("Content-Type", "application/json")
        .set("User-Agent", "houston-oauth")
        .send_string(&body);

    let resp = match resp {
        Ok(r) => r,
        Err(ureq::Error::Status(code, _)) => return Err(anyhow!("refresh HTTP {code}")),
        Err(e) => return Err(anyhow::Error::new(e).context("refresh request failed")),
    };
    let text = resp.into_string().context("reading refresh response")?;
    parse_refresh(&text, refresh_token)
}

/// Turn the token endpoint's JSON into `Tokens`, applying the two rules the
/// wire hides: a missing/zero `expires_in` becomes a modest lifetime (never
/// "already expired", which would storm the endpoint), and an absent
/// `refresh_token` means "not rotated — keep the current one". Pure, so it's
/// unit-tested without any network.
fn parse_refresh(text: &str, current_refresh: &str) -> anyhow::Result<Tokens> {
    #[derive(serde::Deserialize)]
    struct R {
        access_token: String,
        #[serde(default)]
        refresh_token: String,
        #[serde(default)]
        expires_in: i64,
    }
    let r: R = serde_json::from_str(text).context("decoding refresh response")?;
    if r.access_token.is_empty() {
        return Err(anyhow!("refresh response missing access_token"));
    }
    let exp_secs = if r.expires_in > 0 { r.expires_in } else { 3600 };
    let expires_at = chrono::Utc::now().timestamp_millis() + exp_secs * 1000;
    let refresh_out = if r.refresh_token.is_empty() {
        current_refresh.to_string()
    } else {
        r.refresh_token
    };
    Ok(Tokens { access_token: r.access_token, refresh_token: refresh_out, expires_at })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_tokens_and_expiry() {
        let before = chrono::Utc::now().timestamp_millis();
        let tk = parse_refresh(
            r#"{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}"#,
            "old-refresh",
        )
        .unwrap();
        assert_eq!(tk.access_token, "new-access");
        assert_eq!(tk.refresh_token, "new-refresh");
        assert!(tk.expires_at >= before + 3_600_000 && tk.expires_at <= before + 3_610_000);
    }

    #[test]
    fn keeps_current_refresh_when_not_rotated() {
        let tk = parse_refresh(r#"{"access_token":"a","expires_in":60}"#, "the-usual-one").unwrap();
        assert_eq!(tk.refresh_token, "the-usual-one");
    }

    #[test]
    fn zero_expires_in_becomes_a_modest_lifetime_not_expired() {
        let before = chrono::Utc::now().timestamp_millis();
        let tk = parse_refresh(r#"{"access_token":"a"}"#, "r").unwrap();
        assert!(tk.expires_at >= before + 3_600_000, "must not land already-expired");
    }

    #[test]
    fn missing_access_token_is_an_error() {
        assert!(parse_refresh(r#"{"refresh_token":"r"}"#, "r").is_err());
        assert!(parse_refresh("not json", "r").is_err());
    }
}
