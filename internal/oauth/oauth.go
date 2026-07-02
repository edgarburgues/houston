// Package oauth refreshes Claude.ai OAuth credentials. It runs the same
// refresh-token flow Claude Code runs internally — same endpoint, same public
// client id — so the tokens it obtains are exactly what a live session would
// have written to .credentials.json.
package oauth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// tokenURL is Anthropic's OAuth token endpoint (var, not const, so tests can
// point it at a local httptest server).
var tokenURL = "https://console.anthropic.com/v1/oauth/token"

// clientID is Claude Code's public OAuth client id (embedded in the CLI; not a
// secret — public clients authenticate with the refresh token alone).
const clientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// Tokens is a freshly-minted credential set. ExpiresAt is unix milliseconds,
// the unit .credentials.json uses.
type Tokens struct {
	AccessToken  string
	RefreshToken string // may rotate: persist it or the account strands
	ExpiresAt    int64
}

// Refresh exchanges a refresh token for a new access token. The caller MUST
// persist the returned RefreshToken: the endpoint may rotate it, invalidating
// the one just used.
func Refresh(refreshToken string, timeout time.Duration) (Tokens, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     clientID,
	})
	if err != nil {
		return Tokens{}, err
	}
	req, err := http.NewRequest(http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "houston-oauth")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return Tokens{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Tokens{}, fmt.Errorf("refresh HTTP %d", resp.StatusCode)
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"` // seconds
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return Tokens{}, err
	}
	if r.AccessToken == "" {
		return Tokens{}, fmt.Errorf("refresh sin access_token en la respuesta")
	}
	t := Tokens{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).UnixMilli(),
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken // no rotó: conserva el actual
	}
	return t, nil
}
