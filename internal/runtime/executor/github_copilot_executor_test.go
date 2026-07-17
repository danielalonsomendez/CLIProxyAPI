package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/githubcopilot"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestGitHubCopilotRequestToFormatUsesSelectedAuthCatalog(t *testing.T) {
	githubcopilot.StoreCatalog("auth-1", []githubcopilot.Model{{
		ID:                 "claude-model",
		SupportedEndpoints: []string{githubcopilot.EndpointMessages},
	}})
	t.Cleanup(func() { githubcopilot.DeleteCatalog("auth-1") })

	executor := NewGitHubCopilotExecutor(&config.Config{})
	got := executor.RequestToFormat(cliproxyexecutor.Request{Model: "claude-model"}, cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.SelectedAuthMetadataKey: "auth-1"},
	})
	if got != sdktranslator.FormatClaude {
		t.Fatalf("RequestToFormat() = %q", got)
	}
}

func TestGitHubCopilotPrepareChoosesResponsesEndpoint(t *testing.T) {
	model := githubcopilot.Model{ID: "gpt-model", SupportedEndpoints: []string{githubcopilot.EndpointResponses}}
	model.Capabilities.Family = "gpt"
	githubcopilot.StoreCatalog("auth-2", []githubcopilot.Model{model})
	t.Cleanup(func() { githubcopilot.DeleteCatalog("auth-2") })

	executor := NewGitHubCopilotExecutor(&config.Config{GitHubCopilot: config.GitHubCopilotConfig{
		APIBaseURL: "https://copilot.example.test",
	}})
	auth := &cliproxyauth.Auth{ID: "auth-2", Metadata: map[string]any{"access_token": "secret-token"}}
	prepared, err := executor.prepare(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-model",
		Payload: []byte(`{"model":"gpt-model","input":"hello","max_output_tokens":100}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, false)
	if err != nil {
		t.Fatalf("prepare() error = %v", err)
	}
	if prepared.endpoint != githubcopilot.EndpointResponses || prepared.format != sdktranslator.FormatOpenAIResponse {
		t.Fatalf("prepare() endpoint = %q, format = %q", prepared.endpoint, prepared.format)
	}
	if string(prepared.body) == "" || string(prepared.body) == "null" {
		t.Fatalf("prepare() body = %s", prepared.body)
	}
	if httpReq, errRequest := executor.newRequest(context.Background(), auth, prepared, false); errRequest != nil {
		t.Fatalf("newRequest() error = %v", errRequest)
	} else {
		if httpReq.URL.String() != "https://copilot.example.test/responses" {
			t.Fatalf("request URL = %q", httpReq.URL.String())
		}
		if got := httpReq.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("Authorization = %q", got)
		}
	}
}

func TestGitHubCopilotInitiator(t *testing.T) {
	if got := githubCopilotInitiator([]byte(`{"messages":[{"role":"user","content":"hello"}]}`)); got != "user" {
		t.Fatalf("user initiator = %q", got)
	}
	if got := githubCopilotInitiator([]byte(`{"messages":[{"role":"tool","content":"result"}]}`)); got != "agent" {
		t.Fatalf("tool initiator = %q", got)
	}
}

func TestGitHubCopilotPrepareRequestRejectsMissingToken(t *testing.T) {
	executor := NewGitHubCopilotExecutor(&config.Config{})
	req, _ := http.NewRequest(http.MethodGet, "https://api.githubcopilot.com/models", nil)
	err := executor.PrepareRequest(req, &cliproxyauth.Auth{})
	if err == nil {
		t.Fatal("PrepareRequest() error = nil")
	}
}
