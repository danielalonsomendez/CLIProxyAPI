// Package githubcopilot contains the shared GitHub Copilot API contract.
package githubcopilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	githubcopilotauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/githubcopilot"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	log "github.com/sirupsen/logrus"
)

const (
	EndpointMessages        = "/v1/messages"
	EndpointResponses       = "/responses"
	EndpointChatCompletions = "/chat/completions"
)

// Model describes the relevant fields returned by GitHub Copilot's /models API.
type Model struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Version            string   `json:"version"`
	ModelPickerEnabled *bool    `json:"model_picker_enabled"`
	SupportedEndpoints []string `json:"supported_endpoints"`
	Policy             struct {
		State string `json:"state"`
	} `json:"policy"`
	Capabilities struct {
		Family string `json:"family"`
		Limits struct {
			MaxContextWindowTokens int `json:"max_context_window_tokens"`
			MaxOutputTokens        int `json:"max_output_tokens"`
			MaxPromptTokens        int `json:"max_prompt_tokens"`
			MinThinkingBudget      int `json:"min_thinking_budget"`
			MaxThinkingBudget      int `json:"max_thinking_budget"`
		} `json:"limits"`
		Supports struct {
			AdaptiveThinking  bool         `json:"adaptive_thinking"`
			ReasoningEffort   FeatureValue `json:"reasoning_effort"`
			Streaming         bool         `json:"streaming"`
			StructuredOutputs bool         `json:"structured_outputs"`
			ToolCalls         bool         `json:"tool_calls"`
			Vision            bool         `json:"vision"`
			MinThinkingBudget int          `json:"min_thinking_budget"`
			MaxThinkingBudget int          `json:"max_thinking_budget"`
		} `json:"supports"`
		Vision bool `json:"vision"`
	} `json:"capabilities"`
}

// FeatureValue accepts Copilot capability fields represented as either a boolean
// or a list of supported values, such as reasoning_effort.
type FeatureValue struct {
	Supported bool
	Values    []string
}

// UnmarshalJSON implements the mixed boolean-or-list Copilot capability schema.
func (v *FeatureValue) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("true")) || bytes.Equal(data, []byte("false")) {
		return json.Unmarshal(data, &v.Supported)
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	v.Values = values
	v.Supported = len(values) > 0
	return nil
}

// Catalog is the account-scoped result of the Copilot models endpoint.
type Catalog struct {
	Data []Model `json:"data"`
}

var modelCatalogCache = struct {
	sync.RWMutex
	byAuthID map[string]map[string]Model
}{byAuthID: make(map[string]map[string]Model)}

// FetchModels obtains the models enabled for one Copilot credential.
func FetchModels(ctx context.Context, client *http.Client, baseURL, token, apiVersion string) ([]Model, error) {
	if client == nil {
		return nil, errors.New("github copilot: HTTP client is required")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || strings.TrimSpace(token) == "" {
		return nil, errors.New("github copilot: base URL and access token are required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("github copilot: create models request: %w", err)
	}
	ApplyHeaders(req, token, apiVersion, false, false)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github copilot: models request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("github copilot: close models response body")
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("github copilot: read models response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstreamError("models", resp.StatusCode, body)
	}
	var catalog Catalog
	if err = json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("github copilot: decode models response: %w", err)
	}
	models := make([]Model, 0, len(catalog.Data))
	for _, model := range catalog.Data {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" || (model.ModelPickerEnabled != nil && !*model.ModelPickerEnabled) {
			continue
		}
		if state := strings.ToLower(strings.TrimSpace(model.Policy.State)); state != "" && state != "enabled" {
			continue
		}
		if len(model.SupportedEndpoints) == 0 {
			continue
		}
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

// ApplyHeaders adds the headers expected by GitHub Copilot API endpoints.
func ApplyHeaders(req *http.Request, token, apiVersion string, stream, vision bool) {
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	version := strings.TrimSpace(apiVersion)
	if version == "" {
		version = githubcopilotauth.DefaultAPIVersion
	}
	req.Header.Set("X-GitHub-Api-Version", version)
	clientVersion := strings.TrimSpace(buildinfo.Version)
	if clientVersion == "" {
		clientVersion = "dev"
	}
	req.Header.Set("User-Agent", "CLIProxyAPI/"+clientVersion)
	req.Header.Set("Openai-Intent", "conversation-edits")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if vision {
		req.Header.Set("Copilot-Vision-Request", "true")
	}
}

// PreferredEndpoint selects the richest protocol supported by a model.
func (m Model) PreferredEndpoint() string {
	for _, preferred := range []string{EndpointMessages, EndpointResponses, EndpointChatCompletions} {
		for _, endpoint := range m.SupportedEndpoints {
			if normalizeEndpoint(endpoint) == preferred {
				return preferred
			}
		}
	}
	return ""
}

// RegistryModel converts Copilot capabilities to CLIProxyAPI model metadata.
func (m Model) RegistryModel() *registry.ModelInfo {
	parameters := []string{"max_tokens", "temperature", "top_p", "stop"}
	if m.Capabilities.Supports.Streaming {
		parameters = append(parameters, "stream")
	}
	if m.Capabilities.Supports.ToolCalls {
		parameters = append(parameters, "tools", "tool_choice")
	}
	if m.Capabilities.Supports.StructuredOutputs {
		parameters = append(parameters, "response_format")
	}
	if m.Capabilities.Supports.ReasoningEffort.Supported || m.Capabilities.Supports.AdaptiveThinking {
		parameters = append(parameters, "reasoning_effort")
	}
	inputModalities := []string{"TEXT"}
	if m.Capabilities.Vision || m.Capabilities.Supports.Vision {
		inputModalities = append(inputModalities, "IMAGE")
	}
	contextLength := m.Capabilities.Limits.MaxContextWindowTokens
	if contextLength == 0 {
		contextLength = m.Capabilities.Limits.MaxPromptTokens
	}
	var thinking *registry.ThinkingSupport
	minThinkingBudget := m.Capabilities.Limits.MinThinkingBudget
	if minThinkingBudget == 0 {
		minThinkingBudget = m.Capabilities.Supports.MinThinkingBudget
	}
	maxThinkingBudget := m.Capabilities.Limits.MaxThinkingBudget
	if maxThinkingBudget == 0 {
		maxThinkingBudget = m.Capabilities.Supports.MaxThinkingBudget
	}
	if m.Capabilities.Supports.ReasoningEffort.Supported || m.Capabilities.Supports.AdaptiveThinking || maxThinkingBudget > 0 {
		levels := append([]string(nil), m.Capabilities.Supports.ReasoningEffort.Values...)
		if len(levels) == 0 && m.Capabilities.Supports.ReasoningEffort.Supported {
			levels = []string{"low", "medium", "high"}
		}
		thinking = &registry.ThinkingSupport{
			Min:            minThinkingBudget,
			Max:            maxThinkingBudget,
			ZeroAllowed:    minThinkingBudget == 0,
			DynamicAllowed: m.Capabilities.Supports.AdaptiveThinking,
			Levels:         levels,
		}
	}
	displayName := strings.TrimSpace(m.Name)
	if displayName == "" {
		displayName = m.ID
	}
	return &registry.ModelInfo{
		ID:                       m.ID,
		Object:                   "model",
		OwnedBy:                  "github-copilot",
		Type:                     "github-copilot",
		DisplayName:              displayName,
		Name:                     m.ID,
		Version:                  m.Version,
		ContextLength:            contextLength,
		InputTokenLimit:          m.Capabilities.Limits.MaxPromptTokens,
		OutputTokenLimit:         m.Capabilities.Limits.MaxOutputTokens,
		MaxCompletionTokens:      m.Capabilities.Limits.MaxOutputTokens,
		SupportedParameters:      parameters,
		SupportedInputModalities: inputModalities,
		SupportedOutputModalities: []string{
			"TEXT",
		},
		Thinking: thinking,
	}
}

// StoreCatalog caches the endpoint capabilities associated with one auth record.
func StoreCatalog(authID string, models []Model) {
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	modelCatalogCache.Lock()
	modelCatalogCache.byAuthID[authID] = byID
	modelCatalogCache.Unlock()
}

// DeleteCatalog removes cached model capabilities for an auth record.
func DeleteCatalog(authID string) {
	modelCatalogCache.Lock()
	delete(modelCatalogCache.byAuthID, authID)
	modelCatalogCache.Unlock()
}

// LookupModel returns the account-scoped model capabilities.
func LookupModel(authID, modelID string) (Model, bool) {
	modelCatalogCache.RLock()
	model, ok := modelCatalogCache.byAuthID[authID][modelID]
	modelCatalogCache.RUnlock()
	return model, ok
}

func normalizeEndpoint(endpoint string) string {
	endpoint = strings.Trim(strings.TrimSpace(endpoint), "/")
	switch endpoint {
	case "v1/messages", "messages":
		return EndpointMessages
	case "v1/responses", "responses":
		return EndpointResponses
	case "v1/chat/completions", "chat/completions":
		return EndpointChatCompletions
	default:
		return "/" + endpoint
	}
}

func upstreamError(operation string, status int, body []byte) error {
	var payload struct {
		Message string `json:"message"`
		Error   any    `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	detail := strings.TrimSpace(payload.Message)
	if detail == "" && payload.Error != nil {
		detail = fmt.Sprint(payload.Error)
	}
	if detail == "" {
		detail = http.StatusText(status)
	}
	return fmt.Errorf("github copilot: %s failed with status %d: %s", operation, status, detail)
}
