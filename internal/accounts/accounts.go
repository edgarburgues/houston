// Package accounts manages Houston's Claude accounts. Each account is a separate
// CLAUDE_CONFIG_DIR (~/.claude-accounts/account-<id>) with its own /login and
// onboarding, so Claude shows the real account identity and different terminals
// can run different accounts concurrently. The bulk data (projects, sessions,
// plugins, plans, todos) is shared across accounts via junction/symlink.
package accounts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"houston/internal/flock"
)

type Account struct {
	ID        string `json:"id"`      // short stable id
	Label     string `json:"label"`   // human label (e.g. email / "work")
	AddedAt   string `json:"addedAt"` // RFC3339
	LastUse   string `json:"lastUse,omitempty"`
	ConfigDir string `json:"configDir,omitempty"` // per-account CLAUDE_CONFIG_DIR (isolated login/onboarding)
}

// AccountsDir is where per-account Claude config dirs live (one dir per account
// keeps login + onboarding isolated; shared data is linked in via junction /
// symlink). Override with $HOUSTON_ACCOUNTS_DIR.
func AccountsDir() string {
	if d := os.Getenv("HOUSTON_ACCOUNTS_DIR"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude-accounts")
}

// ResolveConfigDir returns the account's CLAUDE_CONFIG_DIR: the stored ConfigDir
// if set, else the conventional <AccountsDir>/account-<id>. Empty string means
// "use Claude's default" (no account configured).
func (a Account) ResolveConfigDir() string {
	if strings.TrimSpace(a.ConfigDir) != "" {
		return a.ConfigDir
	}
	if a.ID == "" {
		return ""
	}
	return filepath.Join(AccountsDir(), "account-"+a.ID)
}

// LoggedIn reports whether this account's config dir has its own credentials
// (i.e. the user has done a one-time `/login` in it).
func (a Account) LoggedIn() bool {
	_, err := os.Stat(filepath.Join(a.ResolveConfigDir(), ".credentials.json"))
	return err == nil
}

// Email returns the logged-in account email recorded in this account's config
// dir (.claude.json -> oauthAccount), or "" if the dir hasn't been logged in.
func (a Account) Email() string {
	b, err := os.ReadFile(filepath.Join(a.ResolveConfigDir(), ".claude.json"))
	if err != nil {
		return ""
	}
	var d struct {
		OauthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(b, &d) != nil {
		return ""
	}
	return d.OauthAccount.EmailAddress
}

// Credential is the claudeAiOauth block of .credentials.json Houston needs:
// the accessToken to probe usage (it carries the subscription usage scope),
// plus the refreshToken/expiry to renew it when it lapses.
type Credential struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // unix milliseconds
}

// Credential reads the account's stored OAuth credential. ok=false when the
// dir was never logged in (no file or no accessToken).
func (a Account) Credential() (Credential, bool) {
	b, err := os.ReadFile(a.credentialsPath())
	if err != nil {
		return Credential{}, false
	}
	var d struct {
		ClaudeAiOauth Credential `json:"claudeAiOauth"`
	}
	if json.Unmarshal(b, &d) != nil {
		return Credential{}, false
	}
	return d.ClaudeAiOauth, d.ClaudeAiOauth.AccessToken != ""
}

// SaveTokens writes refreshed OAuth tokens back into the account's
// .credentials.json, preserving every other field (top level and inside
// claudeAiOauth — scopes, subscriptionType, etc. survive untouched). Atomic
// write with 0600: it's the very file Claude Code reads, so the next launch
// of this account picks up the fresh token too.
func (a Account) SaveTokens(access, refresh string, expiresAt int64) error {
	p := a.credentialsPath()
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return err
	}
	oa := map[string]json.RawMessage{}
	if raw, ok := top["claudeAiOauth"]; ok {
		if err := json.Unmarshal(raw, &oa); err != nil {
			return err
		}
	}
	set := func(key string, v any) error {
		enc, err := json.Marshal(v)
		if err != nil {
			return err
		}
		oa[key] = enc
		return nil
	}
	if err := set("accessToken", access); err != nil {
		return err
	}
	if err := set("refreshToken", refresh); err != nil {
		return err
	}
	if err := set("expiresAt", expiresAt); err != nil {
		return err
	}
	enc, err := json.Marshal(oa)
	if err != nil {
		return err
	}
	top["claudeAiOauth"] = enc
	out, err := json.Marshal(top)
	if err != nil {
		return err
	}
	return writeFileAtomic(p, out, 0o600)
}

func (a Account) credentialsPath() string {
	return filepath.Join(a.ResolveConfigDir(), ".credentials.json")
}

// LockPath is the advisory lock (see internal/flock) that serializes refreshes
// of this account's credential across processes: statusline renders, `houston
// run` and the TUI can all hit an expired token at once, and refresh tokens
// rotate — concurrent refreshes can strand the account.
func (a Account) LockPath() string {
	return a.credentialsPath() + ".lock"
}

// writeFileAtomic writes b via a uniquely-named same-dir temp file + rename.
// The unique name matters as much as the rename: two processes writing the
// same fixed ".tmp" path can interleave and rename corrupted bytes into place.
func writeFileAtomic(p string, b []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, p); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// StoreDir is Houston's own data dir, kept stable regardless of
// CLAUDE_CONFIG_DIR so the Go store and houston-setup-accounts.ps1 always agree
// on where accounts.json lives. Override with $HOUSTON_HOME.
func StoreDir() string {
	if d := os.Getenv("HOUSTON_HOME"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude", "houston")
}

func storePath() string { return filepath.Join(StoreDir(), "accounts.json") }

// Load returns the stored accounts (empty slice if none yet).
func Load() ([]Account, error) {
	b, err := os.ReadFile(storePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a []Account
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	return a, nil
}

// Save writes accounts atomically with 0600 perms.
func Save(a []Account) error {
	p := storePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, b, 0o600)
}

// registryLockWait caps how long a registry mutation waits for the lock; these
// are read-modify-write cycles over a small JSON file, so contention is brief.
const registryLockWait = 3 * time.Second

// lockRegistry serializes Load-modify-Save cycles on accounts.json across
// processes (two `houston run` in parallel, statusline + TUI, ...): without it
// concurrent writers lose each other's updates.
func lockRegistry() (*flock.Lock, error) {
	_ = os.MkdirAll(StoreDir(), 0o700)
	return flock.Acquire(storePath()+".lock", registryLockWait)
}

func slug(label string) string {
	s := strings.ToLower(strings.TrimSpace(label))
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, s)
	s = strings.Trim(s, "-")
	if s == "" {
		s = "acct"
	}
	return s
}

// Add stores a new account. now is passed in (callers stamp time) to keep this
// testable. Returns the created account.
func Add(label, now string) (Account, error) {
	lk, err := lockRegistry()
	if err != nil {
		return Account{}, err
	}
	defer lk.Release()
	list, err := Load()
	if err != nil {
		return Account{}, err
	}
	base := slug(label)
	id := base
	for i := 2; containsID(list, id); i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	acc := Account{ID: id, Label: label, AddedAt: now}
	list = append(list, acc)
	return acc, Save(list)
}

// Remove deletes the account with the given id.
func Remove(id string) error {
	lk, err := lockRegistry()
	if err != nil {
		return err
	}
	defer lk.Release()
	list, err := Load()
	if err != nil {
		return err
	}
	out := list[:0]
	for _, a := range list {
		if a.ID != id {
			out = append(out, a)
		}
	}
	return Save(out)
}

// TouchUse records that an account was just launched. Best-effort: a busy
// lock or unreadable registry only loses the LastUse stamp.
func TouchUse(id, now string) {
	lk, err := lockRegistry()
	if err != nil {
		return
	}
	defer lk.Release()
	list, err := Load()
	if err != nil {
		return
	}
	for i := range list {
		if list[i].ID == id {
			list[i].LastUse = now
		}
	}
	_ = Save(list)
}

func containsID(list []Account, id string) bool {
	for _, a := range list {
		if a.ID == id {
			return true
		}
	}
	return false
}

// Now is a small helper so callers don't import time everywhere.
func Now() string { return time.Now().UTC().Format(time.RFC3339) }
