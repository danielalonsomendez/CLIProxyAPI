package githubcopilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchModelsFiltersAndSorts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":[
			{"id":"z-model","model_picker_enabled":true,"supported_endpoints":["/responses"],"policy":{"state":"enabled"}},
			{"id":"disabled","model_picker_enabled":false,"supported_endpoints":["/chat/completions"],"policy":{"state":"enabled"}},
			{"id":"blocked","model_picker_enabled":true,"supported_endpoints":["/chat/completions"],"policy":{"state":"disabled"}},
			{"id":"a-model","model_picker_enabled":true,"supported_endpoints":["/v1/messages"],"policy":{"state":"enabled"}}
		]}`))
	}))
	defer server.Close()

	models, err := FetchModels(context.Background(), server.Client(), server.URL, "token", "2026-06-01")
	if err != nil {
		t.Fatalf("FetchModels() error = %v", err)
	}
	if len(models) != 2 || models[0].ID != "a-model" || models[1].ID != "z-model" {
		t.Fatalf("FetchModels() = %#v", models)
	}
}

func TestPreferredEndpoint(t *testing.T) {
	model := Model{SupportedEndpoints: []string{"/chat/completions", "/responses", "/v1/messages"}}
	if got := model.PreferredEndpoint(); got != EndpointMessages {
		t.Fatalf("PreferredEndpoint() = %q", got)
	}
}

func TestRegistryModelCapabilities(t *testing.T) {
	model := Model{ID: "copilot-model", Name: "Copilot Model"}
	model.Capabilities.Limits.MaxContextWindowTokens = 128000
	model.Capabilities.Limits.MaxOutputTokens = 16000
	model.Capabilities.Supports.ToolCalls = true
	model.Capabilities.Supports.Vision = true
	model.Capabilities.Supports.ReasoningEffort = FeatureValue{Supported: true, Values: []string{"low", "high"}}

	info := model.RegistryModel()
	if info.ContextLength != 128000 || info.MaxCompletionTokens != 16000 {
		t.Fatalf("RegistryModel() limits = context:%d output:%d", info.ContextLength, info.MaxCompletionTokens)
	}
	if len(info.SupportedInputModalities) != 2 || info.Thinking == nil {
		t.Fatalf("RegistryModel() capabilities = %#v", info)
	}
}

func TestFeatureValueAcceptsBooleanAndLevels(t *testing.T) {
	var boolean FeatureValue
	if err := json.Unmarshal([]byte(`true`), &boolean); err != nil {
		t.Fatalf("boolean UnmarshalJSON() error = %v", err)
	}
	if !boolean.Supported || len(boolean.Values) != 0 {
		t.Fatalf("boolean FeatureValue = %#v", boolean)
	}

	var levels FeatureValue
	if err := json.Unmarshal([]byte(`["low","medium","high"]`), &levels); err != nil {
		t.Fatalf("levels UnmarshalJSON() error = %v", err)
	}
	if !levels.Supported || len(levels.Values) != 3 || levels.Values[2] != "high" {
		t.Fatalf("levels FeatureValue = %#v", levels)
	}
}
