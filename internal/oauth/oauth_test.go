package oauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRefresh(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "nuevo-access",
			"refresh_token": "nuevo-refresh",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()
	old := tokenURL
	tokenURL = srv.URL
	defer func() { tokenURL = old }()

	before := time.Now().UnixMilli()
	tk, err := Refresh("viejo-refresh", 5*time.Second)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if gotBody["grant_type"] != "refresh_token" || gotBody["refresh_token"] != "viejo-refresh" || gotBody["client_id"] != clientID {
		t.Errorf("cuerpo de la petición incorrecto: %v", gotBody)
	}
	if tk.AccessToken != "nuevo-access" || tk.RefreshToken != "nuevo-refresh" {
		t.Errorf("tokens incorrectos: %+v", tk)
	}
	// expiresAt ≈ ahora + 3600s, en milisegundos
	want := before + 3600_000
	if tk.ExpiresAt < want || tk.ExpiresAt > want+10_000 {
		t.Errorf("ExpiresAt fuera de rango: %d (esperaba ~%d)", tk.ExpiresAt, want)
	}
}

func TestRefreshKeepsTokenWhenNotRotated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// sin refresh_token en la respuesta: el endpoint no rotó
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "expires_in": 60})
	}))
	defer srv.Close()
	old := tokenURL
	tokenURL = srv.URL
	defer func() { tokenURL = old }()

	tk, err := Refresh("el-de-siempre", 5*time.Second)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tk.RefreshToken != "el-de-siempre" {
		t.Errorf("sin rotación debería conservar el refresh token actual, dio %q", tk.RefreshToken)
	}
}

func TestRefreshErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
	}))
	defer srv.Close()
	old := tokenURL
	tokenURL = srv.URL
	defer func() { tokenURL = old }()

	if _, err := Refresh("caducado", 5*time.Second); err == nil {
		t.Fatal("un HTTP 400 debería devolver error")
	}
}
