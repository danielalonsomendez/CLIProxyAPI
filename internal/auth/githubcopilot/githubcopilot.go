package githubcopilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	Provider          = "github-copilot"
	DefaultAPIBaseURL = "https://api.githubcopilot.com"
	DefaultAPIVersion = "2026-06-01"
	defaultDomain     = "github.com"
	defaultScope      = "read:user"
	defaultInterval   = 5 * time.Second
	maxPollDuration   = 15 * time.Minute
)

// Auth coordinates GitHub's OAuth Device Flow for a Copilot credential.
type Auth struct {
	client *DeviceFlowClient
}

// NewAuth creates a GitHub Copilot authenticator from application configuration.
func NewAuth(cfg *config.Config) (*Auth, error) {
	client, err := NewDeviceFlowClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Auth{client: client}, nil
}

// StartDeviceFlow requests a user code from GitHub.
func (a *Auth) StartDeviceFlow(ctx context.Context) (*DeviceCodeResponse, error) {
	return a.client.RequestDeviceCode(ctx)
}

// WaitForAuthorization polls GitHub until the user approves or rejects the request.
func (a *Auth) WaitForAuthorization(ctx context.Context, deviceCode *DeviceCodeResponse) (*TokenData, error) {
	return a.client.PollForToken(ctx, deviceCode)
}

// CreateTokenStorage converts the OAuth result into the persistent auth format.
func (a *Auth) CreateTokenStorage(token *TokenData) *TokenStorage {
	if token == nil {
		return nil
	}
	return &TokenStorage{
		AccessToken:      token.AccessToken,
		TokenType:        token.TokenType,
		Scope:            token.Scope,
		EnterpriseDomain: a.client.domain,
		ClientID:         a.client.clientID,
		Type:             Provider,
	}
}

// DeviceFlowClient implements GitHub's OAuth Device Authorization Grant.
type DeviceFlowClient struct {
	httpClient     *http.Client
	clientID       string
	domain         string
	deviceCodeURL  string
	tokenURL       string
	defaultTimeout time.Duration
}

// NewDeviceFlowClient creates a proxy-aware GitHub Device Flow client.
func NewDeviceFlowClient(cfg *config.Config) (*DeviceFlowClient, error) {
	clientID := strings.TrimSpace(os.Getenv("GITHUB_COPILOT_CLIENT_ID"))
	domain := ""
	proxyURL := ""
	if cfg != nil {
		if clientID == "" {
			clientID = strings.TrimSpace(cfg.GitHubCopilot.ClientID)
		}
		domain = cfg.GitHubCopilot.EnterpriseDomain
		proxyURL = cfg.ProxyURL
	}
	if clientID == "" {
		return nil, errors.New("github copilot: client ID is required; set github-copilot.client-id or GITHUB_COPILOT_CLIENT_ID")
	}

	domain, err := NormalizeDomain(domain)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	sdkCfg := config.SDKConfig{ProxyURL: strings.TrimSpace(proxyURL)}
	httpClient = util.SetProxy(&sdkCfg, httpClient)
	oauthBase := "https://" + domain
	return &DeviceFlowClient{
		httpClient:     httpClient,
		clientID:       clientID,
		domain:         domain,
		deviceCodeURL:  oauthBase + "/login/device/code",
		tokenURL:       oauthBase + "/login/oauth/access_token",
		defaultTimeout: maxPollDuration,
	}, nil
}

// NormalizeDomain validates and normalizes a GitHub or GitHub Enterprise hostname.
func NormalizeDomain(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultDomain, nil
	}
	candidate := raw
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("github copilot: invalid enterprise domain %q", raw)
	}
	if parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("github copilot: enterprise domain must be an HTTPS hostname")
	}
	host := strings.ToLower(parsed.Host)
	if strings.ContainsAny(host, " \\@") {
		return "", fmt.Errorf("github copilot: invalid enterprise domain %q", raw)
	}
	return strings.TrimSuffix(host, "."), nil
}

// APIBaseURL resolves the Copilot API base URL for public GitHub or GHES.
func APIBaseURL(cfg *config.Config, enterpriseDomain string) (string, error) {
	if cfg != nil {
		if override := strings.TrimRight(strings.TrimSpace(cfg.GitHubCopilot.APIBaseURL), "/"); override != "" {
			parsed, err := url.Parse(override)
			if err != nil || parsed.Scheme == "" || parsed.Host == "" {
				return "", fmt.Errorf("github copilot: invalid API base URL")
			}
			return override, nil
		}
	}
	domain, err := NormalizeDomain(enterpriseDomain)
	if err != nil {
		return "", err
	}
	if domain == defaultDomain {
		return DefaultAPIBaseURL, nil
	}
	return "https://copilot-api." + domain, nil
}

// APIVersion returns the configured GitHub API version or the tested default.
func APIVersion(cfg *config.Config) string {
	if cfg != nil && strings.TrimSpace(cfg.GitHubCopilot.APIVersion) != "" {
		return strings.TrimSpace(cfg.GitHubCopilot.APIVersion)
	}
	return DefaultAPIVersion
}

// RequestDeviceCode starts the OAuth Device Flow.
func (c *DeviceFlowClient) RequestDeviceCode(ctx context.Context) (*DeviceCodeResponse, error) {
	form := url.Values{"client_id": {c.clientID}, "scope": {defaultScope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.deviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("github copilot: create device code request: %w", err)
	}
	c.applyOAuthHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github copilot: device code request failed: %w", err)
	}
	defer closeBody(resp.Body, "device code")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("github copilot: read device code response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, oauthStatusError("device code", resp.StatusCode, body)
	}
	var result DeviceCodeResponse
	if err = json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("github copilot: decode device code response: %w", err)
	}
	if result.DeviceCode == "" || result.UserCode == "" || result.VerificationURI == "" {
		return nil, errors.New("github copilot: incomplete device code response")
	}
	return &result, nil
}

// PollForToken waits for the Device Flow to complete.
func (c *DeviceFlowClient) PollForToken(ctx context.Context, deviceCode *DeviceCodeResponse) (*TokenData, error) {
	if deviceCode == nil || strings.TrimSpace(deviceCode.DeviceCode) == "" {
		return nil, errors.New("github copilot: device code is required")
	}
	interval := time.Duration(deviceCode.Interval) * time.Second
	if interval <= 0 {
		interval = defaultInterval
	}
	maxDuration := c.defaultTimeout
	if maxDuration <= 0 {
		maxDuration = maxPollDuration
	}
	if deviceCode.ExpiresIn > 0 && time.Duration(deviceCode.ExpiresIn)*time.Second < maxDuration {
		maxDuration = time.Duration(deviceCode.ExpiresIn) * time.Second
	}
	deadline := time.NewTimer(maxDuration)
	defer deadline.Stop()
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("github copilot: authorization cancelled: %w", ctx.Err())
		case <-deadline.C:
			return nil, errors.New("github copilot: device code expired")
		case <-timer.C:
			token, pending, slowDown, err := c.exchangeDeviceCode(ctx, deviceCode.DeviceCode)
			if err != nil {
				return nil, err
			}
			if token != nil {
				return token, nil
			}
			if !pending {
				return nil, errors.New("github copilot: token endpoint returned no result")
			}
			if slowDown {
				interval += 5 * time.Second
			}
			timer.Reset(interval)
		}
	}
}

func (c *DeviceFlowClient) exchangeDeviceCode(ctx context.Context, deviceCode string) (*TokenData, bool, bool, error) {
	form := url.Values{
		"client_id":   {c.clientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, false, false, fmt.Errorf("github copilot: create token request: %w", err)
	}
	c.applyOAuthHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false, false, fmt.Errorf("github copilot: token request failed: %w", err)
	}
	defer closeBody(resp.Body, "token exchange")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, false, fmt.Errorf("github copilot: read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, false, oauthStatusError("token exchange", resp.StatusCode, body)
	}
	var result struct {
		TokenData
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err = json.Unmarshal(body, &result); err != nil {
		return nil, false, false, fmt.Errorf("github copilot: decode token response: %w", err)
	}
	if result.Error != "" {
		switch result.Error {
		case "authorization_pending":
			return nil, true, false, nil
		case "slow_down":
			return nil, true, true, nil
		case "expired_token":
			return nil, false, false, errors.New("github copilot: device code expired")
		case "access_denied":
			return nil, false, false, errors.New("github copilot: access denied by user")
		default:
			return nil, false, false, fmt.Errorf("github copilot: OAuth error %s: %s", result.Error, result.ErrorDescription)
		}
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return nil, false, false, errors.New("github copilot: empty access token in response")
	}
	if result.TokenType == "" {
		result.TokenType = "bearer"
	}
	return &result.TokenData, false, false, nil
}

func (c *DeviceFlowClient) applyOAuthHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	version := strings.TrimSpace(buildinfo.Version)
	if version == "" {
		version = "dev"
	}
	req.Header.Set("User-Agent", "CLIProxyAPI/"+version)
}

func oauthStatusError(operation string, status int, body []byte) error {
	var payload struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Message          string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	detail := strings.TrimSpace(payload.ErrorDescription)
	if detail == "" {
		detail = strings.TrimSpace(payload.Message)
	}
	if detail == "" {
		detail = strings.TrimSpace(payload.Error)
	}
	if detail == "" {
		detail = http.StatusText(status)
	}
	return fmt.Errorf("github copilot: %s failed with status %d: %s", operation, status, detail)
}

func closeBody(body io.Closer, operation string) {
	if err := body.Close(); err != nil {
		log.WithError(err).Debugf("github copilot: close %s response body", operation)
	}
}
