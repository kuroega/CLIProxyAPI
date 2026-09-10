package cmd

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
)

// DoCursorLogin starts Cursor's browser-and-poll authentication flow.
func DoCursorLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}
	manager := newAuthManager()
	_, savedPath, errLogin := manager.Login(context.Background(), "cursor", cfg, &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Metadata:  map[string]string{},
		Prompt:    options.Prompt,
	})
	if errLogin != nil {
		fmt.Printf("Cursor authentication failed: %v\n", errLogin)
		return
	}
	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	fmt.Println("Cursor authentication successful!")
}
