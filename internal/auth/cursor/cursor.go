// Package cursor implements Cursor's browser-and-poll credential exchange.
package cursor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	PollURL     = "https://api2.cursor.sh/auth/poll"
	ExchangeURL = "https://api2.cursor.sh/auth/exchange_user_api_key"
)

type PKCE struct{ Verifier, Challenge, UUID string }
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func (t *Tokens) UnmarshalJSON(data []byte) error {
	var raw struct {
		AccessToken       string `json:"access_token"`
		RefreshToken      string `json:"refresh_token"`
		CamelAccessToken  string `json:"accessToken"`
		CamelRefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.AccessToken = raw.AccessToken
	if t.AccessToken == "" {
		t.AccessToken = raw.CamelAccessToken
	}
	t.RefreshToken = raw.RefreshToken
	if t.RefreshToken == "" {
		t.RefreshToken = raw.CamelRefreshToken
	}
	return nil
}

func NewPKCE() (PKCE, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return PKCE{}, fmt.Errorf("generate cursor verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(bytes)
	sum := sha256.Sum256([]byte(verifier))
	return PKCE{Verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(sum[:]), UUID: uuid.NewString()}, nil
}
func (p PKCE) LoginURL() string {
	return "https://cursor.com/loginDeepControl?" + url.Values{"challenge": {p.Challenge}, "uuid": {p.UUID}, "mode": {"login"}, "redirectTarget": {"cli"}}.Encode()
}

func Poll(ctx context.Context, client *http.Client, pkce PKCE) (Tokens, error) {
	if client == nil {
		client = http.DefaultClient
	}
	delay := time.Second
	failures := 0
	for attempt := 0; attempt < 150; attempt++ {
		if err := wait(ctx, delay); err != nil {
			return Tokens{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, PollURL+"?"+url.Values{"uuid": {pkce.UUID}, "verifier": {pkce.Verifier}}.Encode(), nil)
		if err != nil {
			return Tokens{}, err
		}
		res, err := client.Do(req)
		if err != nil {
			failures++
			if failures >= 3 {
				return Tokens{}, fmt.Errorf("cursor poll: %w", err)
			}
			continue
		}
		if res.StatusCode == http.StatusNotFound {
			res.Body.Close()
			failures = 0
			delay = min(time.Duration(float64(delay)*1.2), 10*time.Second)
			continue
		}
		var tokens Tokens
		decodeErr := json.NewDecoder(res.Body).Decode(&tokens)
		res.Body.Close()
		if res.StatusCode >= 300 || decodeErr != nil || strings.TrimSpace(tokens.AccessToken) == "" {
			failures++
			if failures >= 3 {
				return Tokens{}, fmt.Errorf("cursor poll failed: status %d", res.StatusCode)
			}
			continue
		}
		return tokens, nil
	}
	return Tokens{}, fmt.Errorf("cursor poll expired")
}
func Exchange(ctx context.Context, client *http.Client, refreshToken string) (Tokens, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ExchangeURL, strings.NewReader("{}"))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Authorization", "Bearer "+refreshToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return Tokens{}, fmt.Errorf("cursor refresh: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return Tokens{}, fmt.Errorf("cursor refresh: status %d", res.StatusCode)
	}
	var tokens Tokens
	if err = json.NewDecoder(res.Body).Decode(&tokens); err != nil {
		return Tokens{}, fmt.Errorf("decode cursor refresh: %w", err)
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = refreshToken
	}
	return tokens, nil
}
func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
