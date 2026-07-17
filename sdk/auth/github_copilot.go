package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	githubcopilotauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/githubcopilot"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// GitHubCopilotAuthenticator implements GitHub Device Flow login for Copilot.
type GitHubCopilotAuthenticator struct{}

// NewGitHubCopilotAuthenticator creates a GitHub Copilot authenticator.
func NewGitHubCopilotAuthenticator() Authenticator {
	return &GitHubCopilotAuthenticator{}
}

// Provider returns the native provider identifier.
func (GitHubCopilotAuthenticator) Provider() string {
	return githubcopilotauth.Provider
}

// RefreshLead is nil because GitHub Device Flow tokens do not expose refresh tokens.
func (GitHubCopilotAuthenticator) RefreshLead() *time.Duration {
	return nil
}

// Login authenticates a GitHub user and returns a persistable credential.
func (a GitHubCopilotAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("github copilot: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}
	authService, err := githubcopilotauth.NewAuth(cfg)
	if err != nil {
		return nil, err
	}

	fmt.Println("Starting GitHub Copilot authentication...")
	deviceCode, err := authService.StartDeviceFlow(ctx)
	if err != nil {
		return nil, fmt.Errorf("github copilot: start device flow: %w", err)
	}
	fmt.Printf("\nOpen this URL to authenticate:\n%s\n\nUser code: %s\n\n", deviceCode.VerificationURI, deviceCode.UserCode)
	if !opts.NoBrowser && browser.IsAvailable() {
		if errOpen := browser.OpenURL(deviceCode.VerificationURI); errOpen != nil {
			log.WithError(errOpen).Warn("failed to open GitHub device authorization page")
		}
	}

	fmt.Println("Waiting for authorization...")
	token, err := authService.WaitForAuthorization(ctx, deviceCode)
	if err != nil {
		return nil, err
	}
	storage := authService.CreateTokenStorage(token)
	metadata := map[string]any{
		"type":         githubcopilotauth.Provider,
		"access_token": token.AccessToken,
		"token_type":   token.TokenType,
		"scope":        token.Scope,
		"timestamp":    time.Now().UnixMilli(),
	}
	if storage != nil {
		if domain := strings.TrimSpace(storage.EnterpriseDomain); domain != "" {
			metadata["enterprise_domain"] = domain
		}
		if clientID := strings.TrimSpace(storage.ClientID); clientID != "" {
			metadata["client_id"] = clientID
		}
	}
	fileName := fmt.Sprintf("github-copilot-%d.json", time.Now().UnixMilli())
	return &coreauth.Auth{
		ID:       fileName,
		Provider: githubcopilotauth.Provider,
		FileName: fileName,
		Label:    "GitHub Copilot User",
		Storage:  storage,
		Metadata: metadata,
	}, nil
}
