package cliproxy

import (
	"context"
	"strings"

	githubcopilotauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/githubcopilot"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/githubcopilot"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func (s *Service) fetchGitHubCopilotModelsForAuth(ctx context.Context, auth *coreauth.Auth) ([]*ModelInfo, error) {
	token, enterpriseDomain := githubCopilotModelCredentials(auth)
	baseURL, err := githubcopilotauth.APIBaseURL(s.cfg, enterpriseDomain)
	if err != nil {
		return nil, err
	}
	client := helps.NewProxyAwareHTTPClient(ctx, s.cfg, auth, 0)
	models, err := githubcopilot.FetchModels(ctx, client, baseURL, token, githubcopilotauth.APIVersion(s.cfg))
	if err != nil {
		return nil, err
	}
	githubcopilot.StoreCatalog(auth.ID, models)
	result := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		result = append(result, model.RegistryModel())
	}
	return result, nil
}

func githubCopilotModelCredentials(auth *coreauth.Auth) (token, enterpriseDomain string) {
	if auth == nil {
		return "", ""
	}
	if auth.Metadata != nil {
		for _, key := range []string{"access_token", "github_token", "access", "token"} {
			if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
				token = value
				break
			}
		}
		for _, key := range []string{"enterprise_domain", "enterprise_url"} {
			if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
				enterpriseDomain = value
				break
			}
		}
	}
	if storage, ok := auth.Storage.(*githubcopilotauth.TokenStorage); ok && storage != nil {
		if token == "" {
			token = storage.AccessToken
		}
		if enterpriseDomain == "" {
			enterpriseDomain = storage.EnterpriseDomain
		}
	}
	if auth.Attributes != nil {
		if token == "" {
			token = auth.Attributes["access_token"]
		}
		if token == "" {
			token = auth.Attributes["github_token"]
		}
		if enterpriseDomain == "" {
			enterpriseDomain = auth.Attributes["enterprise_domain"]
		}
		if enterpriseDomain == "" {
			enterpriseDomain = auth.Attributes["enterprise_url"]
		}
	}
	return strings.TrimSpace(token), strings.TrimSpace(enterpriseDomain)
}
