// Package githubcopilot provides GitHub Device Flow authentication and token storage.
package githubcopilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	log "github.com/sirupsen/logrus"
)

// TokenStorage is the on-disk credential format for GitHub Copilot.
type TokenStorage struct {
	AccessToken      string         `json:"access_token"`
	TokenType        string         `json:"token_type,omitempty"`
	Scope            string         `json:"scope,omitempty"`
	EnterpriseDomain string         `json:"enterprise_domain,omitempty"`
	ClientID         string         `json:"client_id,omitempty"`
	Type             string         `json:"type"`
	Metadata         map[string]any `json:"-"`
}

// SetMetadata injects auth record metadata before persistence.
func (s *TokenStorage) SetMetadata(metadata map[string]any) {
	s.Metadata = metadata
}

// SaveTokenToFile serializes the credential with restrictive permissions.
func (s *TokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	s.Type = Provider
	if err := os.MkdirAll(filepath.Dir(authFilePath), 0o700); err != nil {
		return fmt.Errorf("github copilot: create auth directory: %w", err)
	}

	data, err := misc.MergeMetadata(s, s.Metadata)
	if err != nil {
		return fmt.Errorf("github copilot: merge auth metadata: %w", err)
	}
	f, err := os.OpenFile(authFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("github copilot: create token file: %w", err)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.WithError(errClose).Error("github copilot: close token file")
		}
	}()

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(data); err != nil {
		return fmt.Errorf("github copilot: write token file: %w", err)
	}
	return nil
}

// TokenData is the successful GitHub OAuth token response.
type TokenData struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
}

// DeviceCodeResponse is returned by GitHub's device authorization endpoint.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}
