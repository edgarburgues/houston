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

// ProbeCredential returns the token to query the usage endpoint: the account's
// own logged-in accessToken (it carries the subscription usage scope) if the
// dir has been logged in, else "" (so the probe fails and the account shows as
// "not logged in yet").
func (a Account) ProbeCredential() string {
	b, err := os.ReadFile(filepath.Join(a.ResolveConfigDir(), ".credentials.json"))
	if err == nil {
		var d struct {
			ClaudeAiOauth struct {
				AccessToken string `json:"accessToken"`
			} `json:"claudeAiOauth"`
		}
		if json.Unmarshal(b, &d) == nil && d.ClaudeAiOauth.AccessToken != "" {
			return d.ClaudeAiOauth.AccessToken
		}
	}
	return ""
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

// Save writes accounts with 0600 perms (tokens are secrets).
func Save(a []Account) error {
	p := storePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
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

// TouchUse records that an account was just launched.
func TouchUse(id, now string) {
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
