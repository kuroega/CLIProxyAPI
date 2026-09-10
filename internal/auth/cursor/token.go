package cursor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	log "github.com/sirupsen/logrus"
)

// TokenStorage stores Cursor access and refresh credentials in the shared auth-file format.
type TokenStorage struct {
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token"`
	LastRefresh  string         `json:"last_refresh"`
	Type         string         `json:"type"`
	Metadata     map[string]any `json:"-"`
}

// SetMetadata injects non-Cursor fields retained by auth file hooks.
func (s *TokenStorage) SetMetadata(metadata map[string]any) { s.Metadata = metadata }

// SaveTokenToFile persists the credential with restrictive directory permissions.
func (s *TokenStorage) SaveTokenToFile(authFilePath string) error {
	if s == nil {
		return fmt.Errorf("cursor token storage is nil")
	}
	misc.LogSavingCredentials(authFilePath)
	s.Type = "cursor"
	if errMkdir := os.MkdirAll(filepath.Dir(authFilePath), 0o700); errMkdir != nil {
		return fmt.Errorf("create cursor credential directory: %w", errMkdir)
	}
	data, errMerge := misc.MergeMetadata(s, s.Metadata)
	if errMerge != nil {
		return fmt.Errorf("merge cursor credential metadata: %w", errMerge)
	}
	file, errCreate := os.Create(authFilePath)
	if errCreate != nil {
		return fmt.Errorf("create cursor credential file: %w", errCreate)
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Errorf("cursor token storage: close credential file: %v", errClose)
		}
	}()
	if errEncode := json.NewEncoder(file).Encode(data); errEncode != nil {
		return fmt.Errorf("write cursor credential file: %w", errEncode)
	}
	return nil
}
