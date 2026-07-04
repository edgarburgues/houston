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
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()
	old := tokenURL
	tokenURL = srv.URL
	defer func() { tokenURL = old }()

	before := time.Now().UnixMilli()
	tk, err := Refresh("old-refresh", 5*time.Second)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if gotBody["grant_type"] != "refresh_token" || gotBody["refresh_token"] != "old-refresh" || gotBody["client_id"] != clientID {
		t.Errorf("wrong request body: %v", gotBody)
	}
	if tk.AccessToken != "new-access" || tk.RefreshToken != "new-refresh" {
		t.Errorf("wrong tokens: %+v", tk)
	}
	// expiresAt ≈ now + 3600s, in milliseconds
	want := before + 3600_000
	if tk.ExpiresAt < want || tk.ExpiresAt > want+10_000 {
		t.Errorf("ExpiresAt out of range: %d (expected ~%d)", tk.ExpiresAt, want)
	}
}

func TestRefreshKeepsTokenWhenNotRotated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// no refresh_token in the response: the endpoint did not rotate
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "expires_in": 60})
	}))
	defer srv.Close()
	old := tokenURL
	tokenURL = srv.URL
	defer func() { tokenURL = old }()

	tk, err := Refresh("the-usual-one", 5*time.Second)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tk.RefreshToken != "the-usual-one" {
		t.Errorf("without rotation the current refresh token should be kept, got %q", tk.RefreshToken)
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

	if _, err := Refresh("expired", 5*time.Second); err == nil {
		t.Fatal("an HTTP 400 should return an error")
	}
}
