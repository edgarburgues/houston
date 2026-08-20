//! Checks GitHub Releases for a newer Houston, produces a short notice, and
//! performs the self-update: download this platform's release binary, VERIFY it
//! against the release's checksums.txt, and swap it in over the running
//! executable. The notice check is cached (24h) in the Houston data dir so
//! normal use never waits on the network or hammers the GitHub API, and dev
//! builds never nag.

use crate::paths::store_dir;
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};
use std::time::Duration;

/// The GitHub repo to check. Override with $HOUSTON_REPO (handy for forks).
pub fn repo() -> String {
    match std::env::var("HOUSTON_REPO") {
        Ok(r) if !r.is_empty() => r,
        _ => "edgarburgues/houston".to_string(),
    }
}

/// How long a "latest version" lookup is reused before re-querying.
const CHECK_TTL_SECS: i64 = 24 * 3600;

/// Download size caps: a hijacked or misconfigured release must not be able to
/// balloon memory via an unbounded read.
const MAX_CHECKSUMS_BYTES: usize = 1 << 20; // a handful of lines
const MAX_BINARY_BYTES: usize = 1 << 28; // 256 MiB — far above any houston binary

#[derive(Serialize, Deserialize, Default)]
struct Cache {
    #[serde(default)]
    latest: String,
    /// Unix seconds of the last successful fetch.
    #[serde(default)]
    ts: i64,
}

fn cache_path() -> PathBuf {
    store_dir().join("version-check.json")
}

/// A one-line, user-facing update notice if a newer release exists, or None
/// when up to date, on a dev build, or if the check can't run. Safe to call on
/// every launch: the network is only touched once per TTL.
pub fn notice(current: &str, timeout: Duration) -> Option<String> {
    if current.is_empty() || current == "dev" {
        return None; // unversioned/local build: don't nag
    }
    let latest = cached_latest(timeout);
    if latest.is_empty() || !newer(&latest, current) {
        return None;
    }
    Some(format!("new version {latest} available (you have {current}) — update with:  houston update"))
}

/// The latest release tag, querying GitHub at most once per TTL and serving the
/// cached value otherwise. Empty if never fetched and the lookup fails.
fn cached_latest(timeout: Duration) -> String {
    let mut c: Cache = std::fs::read(cache_path())
        .ok()
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default();
    let now = chrono::Utc::now().timestamp();
    if !c.latest.is_empty() && now - c.ts < CHECK_TTL_SECS {
        return c.latest; // still fresh
    }
    if let Some(tag) = fetch_latest(timeout) {
        write_cache(&Cache { latest: tag.clone(), ts: now });
        return tag;
    }
    // Network failed: fall back to whatever we had (possibly empty).
    std::mem::take(&mut c.latest)
}

/// The newest release tag, bypassing the cache. None if the lookup fails.
pub fn fetch_latest(timeout: Duration) -> Option<String> {
    let url = format!("https://api.github.com/repos/{}/releases/latest", repo());
    let resp = ureq::get(&url)
        .timeout(timeout)
        .set("Accept", "application/vnd.github+json")
        .set("User-Agent", "houston-update-check") // GitHub requires a UA
        .call()
        .ok()?;
    let text = resp.into_string().ok()?;
    #[derive(Deserialize)]
    struct R {
        #[serde(default)]
        tag_name: String,
    }
    let r: R = serde_json::from_str(&text).ok()?;
    if r.tag_name.is_empty() {
        None
    } else {
        Some(r.tag_name)
    }
}

fn write_cache(c: &Cache) {
    let Ok(b) = serde_json::to_vec(c) else { return };
    let p = cache_path();
    if let Some(dir) = p.parent() {
        if std::fs::create_dir_all(dir).is_err() {
            return;
        }
    }
    let tmp = p.with_extension(format!("json.{}.tmp", std::process::id()));
    if std::fs::write(&tmp, b).is_ok() && std::fs::rename(&tmp, &p).is_err() {
        let _ = std::fs::remove_file(&tmp);
    }
}

// ------------------------------------------------------------- versioning ---

/// A parsed version: the three numeric fields plus a pre-release rank.
///
/// The pre-release rank is a DELIBERATE improvement over v1, which dropped the
/// suffix entirely: with it, `2.0.0-alpha.0` and `2.0.0` compared equal, so a
/// real release would never be offered to someone running the alpha — exactly
/// the situation this build is in. A pre-release sorts BEFORE its release.
#[derive(PartialEq, Eq, PartialOrd, Ord, Debug)]
struct Version {
    nums: [u64; 3],
    /// 0 = pre-release, 1 = final. Compared after the numbers.
    stage: u8,
    /// The pre-release identifier, compared last so alpha.1 > alpha.0.
    pre: String,
}

fn parse(v: &str) -> Version {
    let v = v.trim().trim_start_matches('v');
    // Split off any pre-release/build suffix (e.g. 1.2.3-rc1, 2.0.0-alpha.0).
    let (core, pre) = match v.find(['-', '+']) {
        Some(i) => (&v[..i], v[i + 1..].to_string()),
        None => (v, String::new()),
    };
    let mut nums = [0u64; 3];
    for (i, f) in core.splitn(3, '.').enumerate() {
        if i > 2 {
            break;
        }
        nums[i] = f.trim().parse().unwrap_or(0);
    }
    let stage = if pre.is_empty() { 1 } else { 0 };
    Version { nums, stage, pre }
}

/// Whether version `a` is strictly newer than `b`. Both may carry a leading
/// "v"; comparison is field-by-field numeric (1.10.0 > 1.9.0), with
/// non-numeric/missing fields treated as 0, then a pre-release sorting before
/// its final release. Unparseable input yields no nag.
pub fn newer(a: &str, b: &str) -> bool {
    parse(a) > parse(b)
}

// ------------------------------------------------------------ self-update ---

/// The release asset for this platform, matching the names the release workflow
/// produces (houston-<os>-<arch>, .exe on Windows).
pub fn asset_name() -> String {
    let os = if cfg!(windows) {
        "windows"
    } else if cfg!(target_os = "macos") {
        "darwin"
    } else {
        "linux"
    };
    let arch = if cfg!(target_arch = "aarch64") { "arm64" } else { "amd64" };
    let mut n = format!("houston-{os}-{arch}");
    if cfg!(windows) {
        n.push_str(".exe");
    }
    n
}

fn download_url(tag: &str, file: &str) -> String {
    format!("https://github.com/{}/releases/download/{tag}/{file}", repo())
}

fn http_get(url: &str, timeout: Duration, max_bytes: usize) -> anyhow::Result<Vec<u8>> {
    use std::io::Read;
    let resp = ureq::get(url)
        .timeout(timeout)
        .set("User-Agent", "houston-update") // GitHub requires a UA
        .call()
        .map_err(|e| anyhow::anyhow!("{e}"))?;
    // Read one byte past the cap so an oversized body is detected, not truncated.
    let mut buf = Vec::new();
    resp.into_reader().take(max_bytes as u64 + 1).read_to_end(&mut buf)?;
    if buf.len() > max_bytes {
        return Err(anyhow::anyhow!("response larger than {max_bytes} bytes for {url}"));
    }
    Ok(buf)
}

/// The lowercase sha256 hex for `file` in a checksums.txt body
/// ("<hex>␠␠<filename>" per line, the sha256sum format).
fn expected_sha(checksums: &[u8], file: &str) -> Option<String> {
    for line in String::from_utf8_lossy(checksums).lines() {
        let f: Vec<&str> = line.split_whitespace().collect();
        if f.len() == 2 && f[1].trim_start_matches('*') == file {
            return Some(f[0].to_lowercase());
        }
    }
    None
}

fn sha256_hex(bytes: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    let mut h = Sha256::new();
    h.update(bytes);
    h.finalize().iter().map(|b| format!("{b:02x}")).collect()
}

// ------------------------------------------------- signature verification ---

/// The release-signing public key (base64, minisign layout). Override with
/// $HOUSTON_UPDATE_PUBKEY — useful for forks and for testing.
///
/// This is the ROOT OF TRUST for self-update: everything a downloaded binary is
/// checked against derives from it. It is public by design — publishing it is
/// the point — but it must only ever change together with a deliberate key
/// rotation, since replacing it is equivalent to trusting a different signer.
/// The matching secret key is NOT in this repository (`houston keygen` writes it
/// owner-only outside the working tree).
///
/// If this is ever empty there is no root of trust, and `download_verified`
/// refuses to install unless the caller explicitly opts into unsigned updates.
const UPDATE_PUBKEY: &str = "RWQIQDB05FRJEgi1W8tFHFrqNC337cPTDF3QwORxXkLFJ37MJ2Bw60Wi";

fn configured_pubkey() -> String {
    match std::env::var("HOUSTON_UPDATE_PUBKEY") {
        Ok(k) if !k.trim().is_empty() => k.trim().to_string(),
        _ => UPDATE_PUBKEY.trim().to_string(),
    }
}

fn b64(s: &str) -> anyhow::Result<Vec<u8>> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD
        .decode(s.trim())
        .map_err(|e| anyhow::anyhow!("bad base64: {e}"))
}

/// A parsed minisign public key: algorithm tag, key id, and the Ed25519 key.
struct PubKey {
    key_id: [u8; 8],
    key: [u8; 32],
}

/// Parse a minisign public key. Accepts the bare base64 line or the whole
/// two-line `minisign.pub` file (the untrusted comment is ignored).
fn parse_pubkey(text: &str) -> anyhow::Result<PubKey> {
    let line = text
        .lines()
        .map(str::trim)
        .find(|l| !l.is_empty() && !l.starts_with("untrusted comment:"))
        .ok_or_else(|| anyhow::anyhow!("public key has no key line"))?;
    let raw = b64(line)?;
    if raw.len() != 42 {
        return Err(anyhow::anyhow!("public key is {} bytes, expected 42", raw.len()));
    }
    // Only pure Ed25519 keys ("Ed") exist in minisign; the signature carries
    // whether the payload was prehashed.
    if &raw[..2] != b"Ed" {
        return Err(anyhow::anyhow!("unsupported public key algorithm"));
    }
    let mut key_id = [0u8; 8];
    key_id.copy_from_slice(&raw[2..10]);
    let mut key = [0u8; 32];
    key.copy_from_slice(&raw[10..42]);
    Ok(PubKey { key_id, key })
}

/// Verify a minisign signature file over `message` with `pubkey_text`, and
/// return the trusted comment IT SIGNED.
///
/// The chain this closes: an embedded public key → the signature → checksums.txt
/// → sha256 → the binary. Without it, checksums.txt comes from the SAME place as
/// the binary, so it only proves the download wasn't corrupted in transit — not
/// that the release is genuine.
///
/// The comment is returned rather than read by the caller from the file, because
/// an unverified comment is attacker-controlled text: getting it from here makes
/// "trust the comment before checking the signature" unrepresentable.
pub fn verify_minisign(pubkey_text: &str, sig_text: &str, message: &[u8]) -> anyhow::Result<String> {
    use ed25519_dalek::{Signature, VerifyingKey};

    let pk = parse_pubkey(pubkey_text)?;
    let vk = VerifyingKey::from_bytes(&pk.key).map_err(|e| anyhow::anyhow!("bad public key: {e}"))?;

    // The .minisig layout: untrusted comment, signature, trusted comment,
    // global signature. Only the LINE TERMINATOR is stripped — the trusted
    // comment is part of the signed payload, so "helpfully" trimming its spaces
    // would make a byte-exact-but-padded signature fail to verify. (Base64 lines
    // are unaffected either way.)
    let lines: Vec<&str> =
        sig_text.lines().map(|l| l.trim_end_matches(['\r', '\n'])).filter(|l| !l.trim().is_empty()).collect();
    let sig_line = lines
        .iter()
        .find(|l| !l.starts_with("untrusted comment:") && !l.starts_with("trusted comment:"))
        .ok_or_else(|| anyhow::anyhow!("signature file has no signature line"))?;
    let raw_comment = lines
        .iter()
        .find_map(|l| l.strip_prefix("trusted comment:"))
        .ok_or_else(|| anyhow::anyhow!("signature file has no trusted comment"))?;
    // minisign writes "trusted comment: <text>"; the single separating space is
    // not part of the comment. Anything else is kept verbatim rather than
    // discarded — dropping it would verify against the wrong bytes.
    let trusted_comment = raw_comment.strip_prefix(' ').unwrap_or(raw_comment);
    let global_line = lines
        .iter()
        .skip_while(|l| !l.starts_with("trusted comment:"))
        .nth(1)
        .ok_or_else(|| anyhow::anyhow!("signature file has no global signature"))?;

    let raw = b64(sig_line)?;
    if raw.len() != 74 {
        return Err(anyhow::anyhow!("signature is {} bytes, expected 74", raw.len()));
    }
    let alg = &raw[..2];
    if raw[2..10] != pk.key_id {
        return Err(anyhow::anyhow!("signature was made with a different key than the one trusted here"));
    }
    let sig = Signature::from_slice(&raw[10..74]).map_err(|e| anyhow::anyhow!("bad signature: {e}"))?;

    // "Ed" signs the file directly; "ED" signs its BLAKE2b-512 digest.
    let signed: Vec<u8> = match alg {
        b"Ed" => message.to_vec(),
        b"ED" => {
            use blake2::{digest::consts::U64, Blake2b, Digest};
            let mut h = Blake2b::<U64>::new();
            h.update(message);
            h.finalize().to_vec()
        }
        _ => return Err(anyhow::anyhow!("unsupported signature algorithm")),
    };
    vk.verify_strict(&signed, &sig)
        .map_err(|_| anyhow::anyhow!("SIGNATURE DOES NOT MATCH — refusing this download"))?;

    // The global signature binds the trusted comment to the signature, so the
    // comment can't be swapped for another release's.
    let global = b64(global_line)?;
    let gsig = Signature::from_slice(&global).map_err(|e| anyhow::anyhow!("bad global signature: {e}"))?;
    let mut bound = raw[10..74].to_vec();
    bound.extend_from_slice(trusted_comment.as_bytes());
    vk.verify_strict(&bound, &gsig)
        .map_err(|_| anyhow::anyhow!("trusted comment is not signed — refusing this download"))?;
    Ok(trusted_comment.to_string())
}

/// The value of a `key:value` field in a (verified) trusted comment.
fn comment_field(comment: &str, key: &str) -> Option<String> {
    comment
        .split_whitespace()
        .find_map(|kv| kv.strip_prefix(key).and_then(|r| r.strip_prefix(':')))
        .map(str::to_string)
}

/// The release tag a signature was made for, if it says.
pub fn signed_tag(comment: &str) -> Option<String> {
    comment_field(comment, "tag")
}

/// Fetch this platform's binary for `tag` and verify it. The chain is: the
/// embedded minisign public key → the signature over checksums.txt → the
/// sha256 in checksums.txt → the binary bytes. Any break in it is an error;
/// this NEVER returns unverified bytes.
///
/// `allow_unsigned` applies ONLY when no signing key is configured at all: a
/// checksum still catches a corrupted or truncated download, but cannot prove
/// the release is genuine. It deliberately CANNOT bypass a signature we are able
/// to check — otherwise one flag would downgrade the entire chain.
pub fn download_verified(tag: &str, timeout: Duration, allow_unsigned: bool) -> anyhow::Result<(Vec<u8>, String)> {
    let file = asset_name();
    let sums = http_get(&download_url(tag, "checksums.txt"), timeout, MAX_CHECKSUMS_BYTES)
        .map_err(|e| anyhow::anyhow!("could not download checksums.txt: {e}"))?;

    // Authenticity first: everything else derives its trust from this step.
    let pubkey = configured_pubkey();
    if !pubkey.is_empty() {
        let sig = http_get(&download_url(tag, "checksums.txt.minisig"), timeout, MAX_CHECKSUMS_BYTES)
            .map_err(|e| anyhow::anyhow!("release {tag} has no signature (checksums.txt.minisig): {e}"))?;
        let comment = verify_minisign(&pubkey, &String::from_utf8_lossy(&sig), &sums)?;

        // The signature must name THIS release. Without that binding, a
        // genuinely-signed checksums.txt + .minisig pair could be lifted from an
        // older release and dropped into a newer tag: the signature verifies, so
        // users "updating" would silently install the OLD binaries, and the
        // version check would then keep offering the update forever. Binding the
        // tag into the signed comment makes that replay fail.
        match signed_tag(&comment) {
            Some(signed) if signed == tag => {}
            Some(signed) => {
                return Err(anyhow::anyhow!(
                    "this signature was made for release {signed}, not {tag} — refusing \
                     (a signed checksum file from another release cannot authorise this one)"
                ))
            }
            None => {
                return Err(anyhow::anyhow!(
                    "the signature does not name a release, so it cannot be tied to {tag} — \
                     re-sign with `houston sign checksums.txt --tag {tag}`"
                ))
            }
        }
    } else if !allow_unsigned {
        return Err(anyhow::anyhow!(
            "no release-signing key is configured, so this download cannot be proven genuine.\n\
             Set $HOUSTON_UPDATE_PUBKEY (or bake UPDATE_PUBKEY in), or re-run with --allow-unsigned \
             to accept checksum-only verification."
        ));
    }

    let want = expected_sha(&sums, &file)
        .ok_or_else(|| anyhow::anyhow!("release {tag} has no checksum for {file}"))?;
    let bin = http_get(&download_url(tag, &file), timeout, MAX_BINARY_BYTES)
        .map_err(|e| anyhow::anyhow!("could not download {file}: {e}"))?;
    let got = sha256_hex(&bin);
    if got != want {
        return Err(anyhow::anyhow!("checksum mismatch for {file} (want {want}, got {got})"));
    }
    Ok((bin, file))
}

/// Whether a release-signing key is configured (i.e. updates can be proven
/// genuine). Callers surface this so an unsigned install is never silent.
pub fn signing_key_configured() -> bool {
    !configured_pubkey().is_empty()
}

/// The public key this build trusts for releases (empty if none).
pub fn trusted_pubkey() -> String {
    configured_pubkey()
}

// -------------------------------------------------------- release signing ---
//
// The maintainer side of the same chain. Signatures are emitted in the standard
// minisign format, so `minisign -Vm` verifies them and this code is not the only
// thing that can. The SECRET key file, by contrast, is Houston's own layout:
// minisign encrypts its secret key with scrypt, and pulling in a KDF and an AEAD
// to re-implement that is a poor trade when the file can simply be owner-only
// and, in CI, never touch the disk at all (see $HOUSTON_SIGNING_KEY).
//
// This means the secret key sits UNENCRYPTED on disk. That is a deliberate,
// documented trade-off for a single-maintainer project: it is protected by file
// permissions, and anyone who can read it can already read the OAuth tokens next
// door. Do not commit it, and keep a copy somewhere you control.

/// A newly generated signing keypair, ready to write out.
pub struct KeyPair {
    /// The `minisign.pub` contents — safe to publish and to bake into the source.
    pub public_key_file: String,
    /// The secret key file contents. NEVER print or log this.
    pub secret_key_file: String,
    /// The base64 public-key line alone, for `UPDATE_PUBKEY`.
    pub public_key_line: String,
}

fn b64enc(b: &[u8]) -> String {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.encode(b)
}

/// Generate an Ed25519 release-signing keypair. The randomness comes from the
/// OS CSPRNG via `getrandom`, not from any seed we choose.
pub fn generate_keypair() -> anyhow::Result<KeyPair> {
    use ed25519_dalek::SigningKey;
    let mut seed = [0u8; 32];
    getrandom::fill(&mut seed).map_err(|e| anyhow::anyhow!("no OS randomness available: {e}"))?;
    let sk = SigningKey::from_bytes(&seed);
    let mut key_id = [0u8; 8];
    getrandom::fill(&mut key_id).map_err(|e| anyhow::anyhow!("no OS randomness available: {e}"))?;

    let mut pub_raw = b"Ed".to_vec();
    pub_raw.extend_from_slice(&key_id);
    pub_raw.extend_from_slice(sk.verifying_key().as_bytes());
    let public_key_line = b64enc(&pub_raw);

    // The secret file carries the key id so a signature can name the key it was
    // made with, and the algorithm tag so the format can evolve.
    let mut sec_raw = b"Ed".to_vec();
    sec_raw.extend_from_slice(&key_id);
    sec_raw.extend_from_slice(&seed);

    let id_hex: String = key_id.iter().rev().map(|b| format!("{b:02X}")).collect();
    Ok(KeyPair {
        public_key_file: format!("untrusted comment: houston release public key {id_hex}\n{public_key_line}\n"),
        secret_key_file: format!(
            "untrusted comment: houston release SECRET key {id_hex} — keep private, do not commit\n{}\n",
            b64enc(&sec_raw)
        ),
        public_key_line,
    })
}

/// Parse a secret key file (the format `generate_keypair` writes).
fn parse_secret(text: &str) -> anyhow::Result<(ed25519_dalek::SigningKey, [u8; 8])> {
    let line = text
        .lines()
        .map(str::trim)
        .find(|l| !l.is_empty() && !l.starts_with("untrusted comment:"))
        .ok_or_else(|| anyhow::anyhow!("secret key has no key line"))?;
    let raw = b64(line)?;
    if raw.len() != 42 || &raw[..2] != b"Ed" {
        return Err(anyhow::anyhow!(
            "not a houston secret key (expected 42 bytes with an 'Ed' tag; a minisign \
             secret key is encrypted and cannot be used here — sign with `minisign -Sm` instead)"
        ));
    }
    let mut key_id = [0u8; 8];
    key_id.copy_from_slice(&raw[2..10]);
    let mut seed = [0u8; 32];
    seed.copy_from_slice(&raw[10..42]);
    Ok((ed25519_dalek::SigningKey::from_bytes(&seed), key_id))
}

/// Sign `message` with a secret key file's contents, producing a `.minisig`
/// body. Uses the prehashed "ED" algorithm (BLAKE2b-512), which is what modern
/// minisign emits by default.
pub fn sign_detached(secret_key_file: &str, message: &[u8], trusted_comment: &str) -> anyhow::Result<String> {
    use blake2::{digest::consts::U64, Blake2b, Digest};
    use ed25519_dalek::Signer;
    let (sk, key_id) = parse_secret(secret_key_file)?;

    let mut h = Blake2b::<U64>::new();
    h.update(message);
    let sig = sk.sign(&h.finalize());

    let mut raw = b"ED".to_vec();
    raw.extend_from_slice(&key_id);
    raw.extend_from_slice(&sig.to_bytes());

    // The global signature binds the trusted comment to the signature itself.
    let mut bound = sig.to_bytes().to_vec();
    bound.extend_from_slice(trusted_comment.as_bytes());
    let gsig = sk.sign(&bound);

    Ok(format!(
        "untrusted comment: signature from houston release key\n{}\ntrusted comment: {}\n{}\n",
        b64enc(&raw),
        trusted_comment,
        b64enc(&gsig.to_bytes())
    ))
}

/// Replace the binary at `exe_path` with `new_bin`. On Windows a running .exe
/// can't be overwritten, so the in-use binary is renamed aside first — to a
/// UNIQUE name, because a fixed ".old" from a previous update may still be
/// locked by a lingering session and would wedge every future update — then the
/// new one is moved into place; the old copy is removed if free, or reported as
/// leftover (`cleanup_stale` collects it on a later run). On unix the replace is
/// a single atomic rename. On any failure the original is restored.
pub fn swap(exe_path: &Path, new_bin: &[u8]) -> anyhow::Result<Option<PathBuf>> {
    let tmp = exe_path.with_extension("new");
    write_executable(&tmp, new_bin)?;

    if !cfg!(windows) {
        if let Err(e) = std::fs::rename(&tmp, exe_path) {
            let _ = std::fs::remove_file(&tmp);
            return Err(e.into());
        }
        return Ok(None);
    }
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    let old = PathBuf::from(format!("{}.old-{nanos}", exe_path.display()));
    if let Err(e) = std::fs::rename(exe_path, &old) {
        let _ = std::fs::remove_file(&tmp);
        return Err(anyhow::anyhow!("could not move the in-use binary aside: {e}"));
    }
    if let Err(e) = std::fs::rename(&tmp, exe_path) {
        let _ = std::fs::rename(&old, exe_path); // rollback
        let _ = std::fs::remove_file(&tmp);
        return Err(e.into());
    }
    // Still locked by another running houston? Leave it for cleanup_stale.
    Ok(std::fs::remove_file(&old).err().map(|_| old))
}

fn write_executable(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
    let mut opts = std::fs::OpenOptions::new();
    opts.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o755);
    }
    use std::io::Write;
    let mut f = opts.open(path)?;
    f.write_all(bytes)?;
    f.flush()
}

/// Best-effort removal of binaries left aside by a previous Windows update once
/// they're no longer locked. Matches every "<exe>.old*", covering the unique
/// .old-<nanos> names plus legacy leftovers. Safe and silent to call on startup.
pub fn cleanup_stale(exe_path: &Path) {
    let Some(dir) = exe_path.parent() else { return };
    let Some(base) = exe_path.file_name().map(|n| n.to_string_lossy().to_string()) else { return };
    let Ok(entries) = std::fs::read_dir(if dir.as_os_str().is_empty() { Path::new(".") } else { dir }) else {
        return;
    };
    let prefix = format!("{base}.old");
    for e in entries.flatten() {
        if e.file_name().to_string_lossy().starts_with(&prefix) {
            let _ = std::fs::remove_file(e.path());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn newer_compares_numerically_not_lexically() {
        assert!(newer("1.10.0", "1.9.0"), "10 > 9 numerically");
        assert!(newer("v2.0.0", "1.99.99"));
        assert!(newer("1.2.4", "1.2.3"));
        assert!(!newer("1.2.3", "1.2.3"));
        assert!(!newer("1.2.3", "1.2.4"));
        // Missing/garbage fields count as 0 and never nag.
        assert!(!newer("", "1.0.0"));
        assert!(!newer("garbage", "1.0.0"));
        assert!(newer("1.0.1", "1"));
    }

    #[test]
    fn a_prerelease_sorts_before_its_release() {
        // The case v1 got wrong, and the one this build is in.
        assert!(newer("2.0.0", "2.0.0-alpha.0"), "the real release IS newer than its alpha");
        assert!(!newer("2.0.0-alpha.0", "2.0.0"));
        // Later pre-releases beat earlier ones.
        assert!(newer("2.0.0-alpha.1", "2.0.0-alpha.0"));
        assert!(newer("2.0.0-beta", "2.0.0-alpha"));
        // And an alpha of a newer version still wins on the numbers.
        assert!(newer("2.1.0-alpha", "2.0.0"));
        // v1's released version must not nag someone on the v2 alpha.
        assert!(!newer("v1.2.1", "2.0.0-alpha.0"));
    }

    #[test]
    fn notice_is_silent_on_dev_builds() {
        assert!(notice("", Duration::from_millis(1)).is_none());
        assert!(notice("dev", Duration::from_millis(1)).is_none());
    }

    #[test]
    fn expected_sha_reads_the_sha256sum_format() {
        let body = b"abc123  houston-linux-amd64\n\
                     DEF456  houston-windows-amd64.exe\n\
                     999  other.txt\n";
        assert_eq!(expected_sha(body, "houston-linux-amd64").unwrap(), "abc123");
        // Case is normalized, and the binary-mode '*' prefix is tolerated.
        assert_eq!(expected_sha(body, "houston-windows-amd64.exe").unwrap(), "def456");
        assert_eq!(expected_sha(b"aa *houston-linux-amd64\n", "houston-linux-amd64").unwrap(), "aa");
        // A missing entry is None — never a silent pass.
        assert!(expected_sha(body, "houston-darwin-arm64").is_none());
        assert!(expected_sha(b"malformed line here now\n", "x").is_none());
    }

    #[test]
    fn sha256_matches_known_vector() {
        assert_eq!(sha256_hex(b""), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
        assert_eq!(sha256_hex(b"abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
    }

    #[test]
    fn asset_name_matches_the_release_convention() {
        let n = asset_name();
        assert!(n.starts_with("houston-"));
        #[cfg(windows)]
        assert!(n.ends_with(".exe"));
    }

    // --- signature verification ------------------------------------------
    //
    // Keys and signatures are built here with the same crate that verifies
    // them, so the tests pin the FORMAT and the failure modes rather than
    // re-implementing minisign.

    fn b64enc(b: &[u8]) -> String {
        use base64::Engine;
        base64::engine::general_purpose::STANDARD.encode(b)
    }

    struct Signer {
        sk: ed25519_dalek::SigningKey,
        key_id: [u8; 8],
    }

    impl Signer {
        fn new(seed: u8) -> Self {
            Signer { sk: ed25519_dalek::SigningKey::from_bytes(&[seed; 32]), key_id: [seed; 8] }
        }
        fn pubkey(&self) -> String {
            let mut raw = b"Ed".to_vec();
            raw.extend_from_slice(&self.key_id);
            raw.extend_from_slice(self.sk.verifying_key().as_bytes());
            format!("untrusted comment: test key\n{}\n", b64enc(&raw))
        }
        /// A .minisig for `msg`. `prehash` selects "ED" (BLAKE2b) over "Ed".
        fn sign(&self, msg: &[u8], prehash: bool, comment: &str) -> String {
            use ed25519_dalek::Signer as _;
            let (alg, payload): (&[u8; 2], Vec<u8>) = if prehash {
                use blake2::{digest::consts::U64, Blake2b, Digest};
                let mut h = Blake2b::<U64>::new();
                h.update(msg);
                (b"ED", h.finalize().to_vec())
            } else {
                (b"Ed", msg.to_vec())
            };
            let sig = self.sk.sign(&payload);
            let mut raw = alg.to_vec();
            raw.extend_from_slice(&self.key_id);
            raw.extend_from_slice(&sig.to_bytes());
            let mut bound = sig.to_bytes().to_vec();
            bound.extend_from_slice(comment.as_bytes());
            let gsig = self.sk.sign(&bound);
            format!(
                "untrusted comment: sig\n{}\ntrusted comment: {}\n{}\n",
                b64enc(&raw),
                comment,
                b64enc(&gsig.to_bytes())
            )
        }
    }

    #[test]
    fn verifies_both_minisign_algorithms() {
        let s = Signer::new(7);
        let msg = b"abc123  houston-linux-amd64\n";
        // Legacy "Ed" (raw) and modern "ED" (BLAKE2b-prehashed) both verify, and
        // both return the comment they signed.
        let c = verify_minisign(&s.pubkey(), &s.sign(msg, false, "timestamp:1 file:checksums.txt"), msg).unwrap();
        assert_eq!(c, "timestamp:1 file:checksums.txt");
        verify_minisign(&s.pubkey(), &s.sign(msg, true, "timestamp:1 file:checksums.txt"), msg).unwrap();
    }

    #[test]
    fn relabelling_the_algorithm_cannot_downgrade_a_signature() {
        // The alg bytes are NOT covered by the signature, so an attacker may
        // rewrite them. That must not help: "Ed" verifies against the message and
        // "ED" against its BLAKE2b digest, so a relabelled signature is checked
        // against different bytes and fails.
        let s = Signer::new(11);
        let msg = b"payload\n";
        let prehashed = s.sign(msg, true, "t");
        let relabelled = prehashed.replacen("\nRUQ", "\nRWQ", 1); // "ED" -> "Ed" in base64
        if relabelled != prehashed {
            assert!(verify_minisign(&s.pubkey(), &relabelled, msg).is_err());
        }
        // And the same in the other direction.
        let raw = s.sign(msg, false, "t");
        let relabelled = raw.replacen("\nRWQ", "\nRUQ", 1);
        if relabelled != raw {
            assert!(verify_minisign(&s.pubkey(), &relabelled, msg).is_err());
        }
    }

    #[test]
    fn the_trusted_comment_is_verified_byte_exact() {
        let s = Signer::new(13);
        let msg = b"x\n";
        // A comment with inner spacing must round-trip EXACTLY: a verifier that
        // trims the signed bytes would reject its own valid signature.
        let comment = "timestamp:1  file:checksums.txt  tag:v2.0.0";
        let got = verify_minisign(&s.pubkey(), &s.sign(msg, true, comment), msg).unwrap();
        assert_eq!(got, comment);
        assert_eq!(signed_tag(&got).as_deref(), Some("v2.0.0"));
        // An empty comment is legitimate and must not be confused with a missing one.
        let got = verify_minisign(&s.pubkey(), &s.sign(msg, true, ""), msg).unwrap();
        assert_eq!(got, "");
        assert!(signed_tag(&got).is_none());
    }

    #[test]
    fn a_signature_is_bound_to_its_release() {
        // Replay across releases: a genuinely-signed checksums.txt from v2.0.0
        // dropped into v2.0.1 would verify, and users "updating" would install
        // the OLD binaries. The tag in the signed comment is what stops it.
        let s = Signer::new(17);
        let msg = b"deadbeef  houston-linux-amd64\n";
        let c = verify_minisign(&s.pubkey(), &s.sign(msg, true, "timestamp:9 tag:v2.0.0"), msg).unwrap();
        assert_eq!(signed_tag(&c).as_deref(), Some("v2.0.0"), "the tag survives verification");
        // Fields are matched whole, so a lookalike key does not satisfy `tag`.
        assert!(signed_tag("timestamp:9 stage:v9").is_none());
        assert_eq!(comment_field("a:1 b:2", "b").as_deref(), Some("2"));
        assert!(comment_field("a:1", "z").is_none());
    }

    #[test]
    fn rejects_tampering_wrong_key_and_malformed_input() {
        let s = Signer::new(7);
        let msg = b"abc123  houston-linux-amd64\n";
        let sig = s.sign(msg, true, "timestamp:1");

        // A single flipped byte in the signed payload fails.
        let tampered = b"abc124  houston-linux-amd64\n";
        assert!(verify_minisign(&s.pubkey(), &sig, tampered).is_err(), "tampered checksums must fail");

        // A valid signature from a DIFFERENT key is refused (key id mismatch),
        // which is what stops an attacker signing with their own key.
        let other = Signer::new(9);
        assert!(verify_minisign(&s.pubkey(), &other.sign(msg, true, "t"), msg).is_err());

        // Swapping the trusted comment breaks the global signature.
        let swapped = sig.replace("trusted comment: timestamp:1", "trusted comment: timestamp:999");
        assert!(verify_minisign(&s.pubkey(), &swapped, msg).is_err(), "comment must be bound to the signature");

        // Malformed shapes are errors, never silent passes.
        assert!(verify_minisign("", &sig, msg).is_err());
        assert!(verify_minisign(&s.pubkey(), "untrusted comment: only\n", msg).is_err());
        assert!(verify_minisign(&s.pubkey(), "not base64 at all", msg).is_err());
        assert!(verify_minisign("untrusted comment: x\nAAAA\n", &sig, msg).is_err(), "short key");
    }

    #[test]
    fn pubkey_parses_bare_line_or_whole_file() {
        let s = Signer::new(3);
        let whole = s.pubkey();
        let bare = whole.lines().nth(1).unwrap();
        assert!(parse_pubkey(&whole).is_ok());
        assert!(parse_pubkey(bare).is_ok());
        // A non-Ed algorithm tag is rejected rather than assumed.
        let mut raw = b"XX".to_vec();
        raw.extend_from_slice(&[0u8; 40]);
        assert!(parse_pubkey(&b64enc(&raw)).is_err());
    }

    #[test]
    fn a_generated_key_signs_what_it_can_verify() {
        // The full maintainer→user loop: keygen, sign a checksums.txt, verify it
        // with the public half. If these two halves ever disagree, releases
        // become uninstallable, so the loop is pinned here.
        let kp = generate_keypair().unwrap();
        let sums = b"abc123  houston-windows-amd64.exe\n";
        let sig = sign_detached(&kp.secret_key_file, sums, "timestamp:1784900000 file:checksums.txt").unwrap();

        verify_minisign(&kp.public_key_file, &sig, sums).expect("our own signature must verify");
        // The bare base64 line is what goes into UPDATE_PUBKEY, and it works too.
        verify_minisign(&kp.public_key_line, &sig, sums).unwrap();

        // Tampered payload, and a signature from a DIFFERENT generated key.
        assert!(verify_minisign(&kp.public_key_file, &sig, b"abc124  x\n").is_err());
        let other = generate_keypair().unwrap();
        assert!(verify_minisign(&other.public_key_file, &sig, sums).is_err());

        // Two keygens never collide, and the secret never leaks into the public.
        assert_ne!(kp.public_key_line, other.public_key_line);
        assert!(!kp.public_key_file.contains(kp.secret_key_file.lines().nth(1).unwrap()));
        // A minisign (encrypted) secret key is rejected with an explanation
        // rather than a confusing parse error.
        let err = sign_detached("untrusted comment: minisign encrypted secret key\nRWRTY0Iy\n", b"x", "t")
            .expect_err("an encrypted key cannot be used");
        assert!(err.to_string().contains("minisign"), "got: {err}");
    }

    #[test]
    fn a_release_signing_key_is_configured_and_parses() {
        // The root of trust must actually be present in shipped builds: with it
        // empty, `--allow-unsigned` would be the only way to update, and nobody
        // would notice until someone shipped a bad binary.
        assert!(signing_key_configured(), "UPDATE_PUBKEY must not be empty in a release build");
        parse_pubkey(&configured_pubkey()).expect("the baked-in key must be well-formed");
    }

    #[test]
    fn allow_unsigned_cannot_skip_a_signature_we_can_check() {
        // --allow-unsigned exists for the no-key case ONLY. Once a key is
        // configured, a missing or bad signature must fail even with the flag —
        // otherwise the flag would be a one-word downgrade of the whole chain.
        assert!(signing_key_configured());
        for allow in [false, true] {
            let err = download_verified("v0.0.0-does-not-exist", Duration::from_millis(1), allow)
                .expect_err("a nonexistent release must never yield bytes");
            // It fails at the download/signature step, never by falling through
            // to "checksum was fine".
            let msg = err.to_string();
            assert!(
                msg.contains("checksums.txt") || msg.contains("signature"),
                "must fail on the trust chain, got: {msg}"
            );
        }
    }

    #[test]
    fn swap_replaces_the_binary_and_cleanup_collects_leftovers() {
        let dir = tempfile::tempdir().unwrap();
        let exe = dir.path().join("houston.exe");
        std::fs::write(&exe, b"old binary").unwrap();

        let leftover = swap(&exe, b"new binary").unwrap();
        assert_eq!(std::fs::read(&exe).unwrap(), b"new binary");
        // Nothing holds the old copy in a test, so it's removed outright.
        assert!(leftover.is_none());
        // No .new temp survives.
        assert!(!exe.with_extension("new").exists());

        // A stranded .old-<nanos> is collected by cleanup_stale.
        let stale = dir.path().join("houston.exe.old-12345");
        std::fs::write(&stale, b"x").unwrap();
        cleanup_stale(&exe);
        assert!(!stale.exists());
        assert!(exe.exists(), "the live binary is never touched by cleanup");
    }
}
