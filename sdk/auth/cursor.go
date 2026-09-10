package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// CursorAuthenticator implements Cursor's browser-mediated device login flow.
type CursorAuthenticator struct{}

// NewCursorAuthenticator constructs the Cursor authenticator.
func NewCursorAuthenticator() *CursorAuthenticator { return &CursorAuthenticator{} }

func (a *CursorAuthenticator) Provider() string { return "cursor" }

// RefreshLead conservatively refreshes Cursor tokens before their typical one-hour expiry.
func (a *CursorAuthenticator) RefreshLead() *time.Duration { return new(45 * time.Minute) }

func (a *CursorAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}
	pkce, errPKCE := cursorauth.NewPKCE()
	if errPKCE != nil {
		return nil, errPKCE
	}
	loginURL := pkce.LoginURL()
	if !opts.NoBrowser && browser.IsAvailable() {
		if errOpen := browser.OpenURL(loginURL); errOpen != nil {
			log.Warnf("cursor authentication: open browser: %v", errOpen)
		}
	}
	fmt.Printf("Open this URL to authenticate with Cursor:\n%s\n", loginURL)
	fmt.Println("Waiting for Cursor authentication...")
	tokens, errPoll := cursorauth.Poll(ctx, nil, pkce)
	if errPoll != nil {
		return nil, errPoll
	}
	if strings.TrimSpace(tokens.AccessToken) == "" || strings.TrimSpace(tokens.RefreshToken) == "" {
		return nil, fmt.Errorf("cursor authentication returned incomplete credentials")
	}
	storage := &cursorauth.TokenStorage{
		AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		LastRefresh: time.Now().UTC().Format(time.RFC3339), Type: a.Provider(),
	}
	return &coreauth.Auth{
		ID: "cursor.json", Provider: a.Provider(), FileName: "cursor.json", Storage: storage,
		Metadata: map[string]any{
			"access_token": tokens.AccessToken, "refresh_token": tokens.RefreshToken,
			"last_refresh": storage.LastRefresh, "type": a.Provider(),
		},
	}, nil
}
