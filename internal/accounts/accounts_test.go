package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCRUD(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())

	if a, _ := Load(); len(a) != 0 {
		t.Fatalf("expected 0 initial accounts, got %d", len(a))
	}
	a1, err := Add("work@x.com", "2026-05-28T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if a1.ID != "work-x-com" {
		t.Errorf("unexpected slug id: %q", a1.ID)
	}
	if _, err := Add("work@x.com", "2026-05-28T11:00:00Z"); err != nil {
		t.Fatal(err)
	}
	list, _ := Load()
	if len(list) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(list))
	}
	if list[1].ID != "work-x-com-2" {
		t.Errorf("id collision not resolved: %q", list[1].ID)
	}
	if list[0].Label != "work@x.com" {
		t.Errorf("label stored wrong: %q", list[0].Label)
	}

	// Token is optional now (identity comes from the per-account /login).
	if _, err := Add("no-token", "2026-05-28T12:00:00Z"); err != nil {
		t.Errorf("adding an account without a token should be allowed, got: %v", err)
	}
	Remove("no-token")

	Remove(a1.ID)
	list, _ = Load()
	if len(list) != 1 || list[0].ID != "work-x-com-2" {
		t.Fatalf("Remove did not keep the right account: %+v", list)
	}
}

func TestCredentialAndSaveTokens(t *testing.T) {
	dir := t.TempDir()
	a := Account{ID: "t", ConfigDir: dir}

	// no file: no credential
	if _, ok := a.Credential(); ok {
		t.Fatal("missing .credentials.json should give ok=false")
	}

	// realistic file with extra fields that must survive SaveTokens
	seed := `{
  "claudeAiOauth": {
    "accessToken": "acc-1",
    "refreshToken": "ref-1",
    "expiresAt": 1782744792017,
    "scopes": ["user:profile", "user:inference"],
    "subscriptionType": "max",
    "rateLimitTier": "tier5"
  },
  "otherTopLevel": {"keep": true}
}`
	credPath := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(credPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	c, ok := a.Credential()
	if !ok || c.AccessToken != "acc-1" || c.RefreshToken != "ref-1" || c.ExpiresAt != 1782744792017 {
		t.Fatalf("Credential read wrong: %+v ok=%v", c, ok)
	}

	if err := a.SaveTokens("acc-2", "ref-2", 1783000000000); err != nil {
		t.Fatalf("SaveTokens: %v", err)
	}
	c2, ok := a.Credential()
	if !ok || c2.AccessToken != "acc-2" || c2.RefreshToken != "ref-2" || c2.ExpiresAt != 1783000000000 {
		t.Fatalf("tokens not updated: %+v", c2)
	}
	// unrelated fields (scopes, subscriptionType, extra top-level) are still
	// there, intact as JSON
	b, _ := os.ReadFile(credPath)
	var full map[string]json.RawMessage
	if err := json.Unmarshal(b, &full); err != nil {
		t.Fatalf("resulting JSON invalid: %v", err)
	}
	if _, ok := full["otherTopLevel"]; !ok {
		t.Error("an unrelated top-level field was lost")
	}
	var oa map[string]json.RawMessage
	_ = json.Unmarshal(full["claudeAiOauth"], &oa)
	if string(oa["subscriptionType"]) != `"max"` || string(oa["rateLimitTier"]) != `"tier5"` {
		t.Errorf("oauth block fields altered: %v", oa)
	}
	if string(oa["scopes"]) != `["user:profile", "user:inference"]` && string(oa["scopes"]) != `["user:profile","user:inference"]` {
		t.Errorf("scopes altered: %s", oa["scopes"])
	}
	if string(oa["expiresAt"]) != "1783000000000" {
		t.Errorf("expiresAt should serialize as an integer: %s", oa["expiresAt"])
	}
}

