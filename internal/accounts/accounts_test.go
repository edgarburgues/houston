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
		t.Fatalf("esperaba 0 cuentas iniciales, hay %d", len(a))
	}
	a1, err := Add("work@x.com", "2026-05-28T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if a1.ID != "work-x-com" {
		t.Errorf("slug id inesperado: %q", a1.ID)
	}
	if _, err := Add("work@x.com", "2026-05-28T11:00:00Z"); err != nil {
		t.Fatal(err)
	}
	list, _ := Load()
	if len(list) != 2 {
		t.Fatalf("esperaba 2 cuentas, hay %d", len(list))
	}
	if list[1].ID != "work-x-com-2" {
		t.Errorf("colisión de id no resuelta: %q", list[1].ID)
	}
	if list[0].Label != "work@x.com" {
		t.Errorf("label mal guardado: %q", list[0].Label)
	}

	// Token is optional now (identity comes from the per-account /login).
	if _, err := Add("no-token", "2026-05-28T12:00:00Z"); err != nil {
		t.Errorf("añadir cuenta sin token debería permitirse, dio: %v", err)
	}
	Remove("no-token")

	Remove(a1.ID)
	list, _ = Load()
	if len(list) != 1 || list[0].ID != "work-x-com-2" {
		t.Fatalf("Remove no dejó la cuenta correcta: %+v", list)
	}
}

func TestCredentialAndSaveTokens(t *testing.T) {
	dir := t.TempDir()
	a := Account{ID: "t", ConfigDir: dir}

	// sin fichero: sin credencial
	if _, ok := a.Credential(); ok {
		t.Fatal("sin .credentials.json debería dar ok=false")
	}

	// fichero real con campos extra que deben sobrevivir al SaveTokens
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
		t.Fatalf("Credential mal leído: %+v ok=%v", c, ok)
	}

	if err := a.SaveTokens("acc-2", "ref-2", 1783000000000); err != nil {
		t.Fatalf("SaveTokens: %v", err)
	}
	c2, ok := a.Credential()
	if !ok || c2.AccessToken != "acc-2" || c2.RefreshToken != "ref-2" || c2.ExpiresAt != 1783000000000 {
		t.Fatalf("tokens no actualizados: %+v", c2)
	}
	// los campos ajenos (scopes, subscriptionType, top-level extra) siguen ahí,
	// intactos byte a byte como JSON
	b, _ := os.ReadFile(credPath)
	var full map[string]json.RawMessage
	if err := json.Unmarshal(b, &full); err != nil {
		t.Fatalf("JSON resultante inválido: %v", err)
	}
	if _, ok := full["otherTopLevel"]; !ok {
		t.Error("se perdió un campo top-level ajeno")
	}
	var oa map[string]json.RawMessage
	_ = json.Unmarshal(full["claudeAiOauth"], &oa)
	if string(oa["subscriptionType"]) != `"max"` || string(oa["rateLimitTier"]) != `"tier5"` {
		t.Errorf("campos del bloque oauth alterados: %v", oa)
	}
	if string(oa["scopes"]) != `["user:profile", "user:inference"]` && string(oa["scopes"]) != `["user:profile","user:inference"]` {
		t.Errorf("scopes alterados: %s", oa["scopes"])
	}
	if string(oa["expiresAt"]) != "1783000000000" {
		t.Errorf("expiresAt debería serializarse como entero: %s", oa["expiresAt"])
	}
}

