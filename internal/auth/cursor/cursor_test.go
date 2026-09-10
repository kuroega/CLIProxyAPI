package cursor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokensUnmarshalAcceptsCursorWireCasing(t *testing.T) {
	for _, body := range []string{
		`{"accessToken":"access","refreshToken":"refresh"}`,
		`{"access_token":"access","refresh_token":"refresh"}`,
	} {
		var tokens Tokens
		if err := json.Unmarshal([]byte(body), &tokens); err != nil {
			t.Fatalf("Unmarshal() error = %v", err)
		}
		if tokens.AccessToken != "access" || tokens.RefreshToken != "refresh" {
			t.Fatalf("tokens = %#v", tokens)
		}
	}
}

func TestExchangeAcceptsCursorWireCasing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer refresh" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"access","refreshToken":"rotated"}`))
	}))
	defer server.Close()

	original := ExchangeURL
	ExchangeURL = server.URL
	defer func() { ExchangeURL = original }()

	tokens, err := Exchange(t.Context(), server.Client(), "refresh")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if tokens.AccessToken != "access" || tokens.RefreshToken != "rotated" {
		t.Fatalf("tokens = %#v", tokens)
	}
}

func TestPKCELoginURLIncludesRequiredParameters(t *testing.T) {
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE() error = %v", err)
	}
	url := pkce.LoginURL()
	for _, want := range []string{"challenge=", "uuid=", "mode=login", "redirectTarget=cli"} {
		if !strings.Contains(url, want) {
			t.Fatalf("LoginURL() = %q, missing %q", url, want)
		}
	}
}
