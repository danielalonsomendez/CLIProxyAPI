package githubcopilot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestNormalizeDomain(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "default", want: "github.com"},
		{name: "hostname", input: "GitHub.Example.COM", want: "github.example.com"},
		{name: "https URL", input: "https://github.example.com/", want: "github.example.com"},
		{name: "reject HTTP", input: "http://github.example.com", wantErr: true},
		{name: "reject path", input: "https://github.example.com/path", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeDomain(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeDomain() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("NormalizeDomain() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAPIBaseURL(t *testing.T) {
	got, err := APIBaseURL(nil, "github.example.com")
	if err != nil {
		t.Fatalf("APIBaseURL() error = %v", err)
	}
	if got != "https://copilot-api.github.example.com" {
		t.Fatalf("APIBaseURL() = %q", got)
	}

	cfg := &config.Config{GitHubCopilot: config.GitHubCopilotConfig{APIBaseURL: "https://proxy.example.test/copilot/"}}
	got, err = APIBaseURL(cfg, "")
	if err != nil {
		t.Fatalf("APIBaseURL(override) error = %v", err)
	}
	if got != "https://proxy.example.test/copilot" {
		t.Fatalf("APIBaseURL(override) = %q", got)
	}
}

func TestRequestDeviceCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.Form.Get("client_id"); got != "test-client" {
			t.Errorf("client_id = %q", got)
		}
		if got := r.Form.Get("scope"); got != "read:user" {
			t.Errorf("scope = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_code":"device","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`))
	}))
	defer server.Close()

	client := testDeviceClient(server.URL)
	got, err := client.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("RequestDeviceCode() error = %v", err)
	}
	if got.DeviceCode != "device" || got.UserCode != "ABCD-1234" {
		t.Fatalf("RequestDeviceCode() = %#v", got)
	}
}

func TestExchangeDeviceCodeStates(t *testing.T) {
	responses := []string{
		`{"error":"authorization_pending"}`,
		`{"error":"slow_down"}`,
		`{"access_token":"token-value","token_type":"bearer","scope":"read:user"}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if len(responses) == 0 {
			t.Fatal("unexpected extra request")
		}
		response := responses[0]
		responses = responses[1:]
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	client := testDeviceClient(server.URL)
	_, pending, slowDown, err := client.exchangeDeviceCode(context.Background(), "device")
	if err != nil || !pending || slowDown {
		t.Fatalf("pending result = pending:%v slow:%v err:%v", pending, slowDown, err)
	}
	_, pending, slowDown, err = client.exchangeDeviceCode(context.Background(), "device")
	if err != nil || !pending || !slowDown {
		t.Fatalf("slow_down result = pending:%v slow:%v err:%v", pending, slowDown, err)
	}
	token, pending, slowDown, err := client.exchangeDeviceCode(context.Background(), "device")
	if err != nil || pending || slowDown || token == nil || token.AccessToken != "token-value" {
		t.Fatalf("success result = token:%#v pending:%v slow:%v err:%v", token, pending, slowDown, err)
	}
}

func TestPollForTokenExpires(t *testing.T) {
	client := &DeviceFlowClient{
		httpClient:     http.DefaultClient,
		clientID:       "test-client",
		tokenURL:       "http://127.0.0.1:1/unreachable",
		defaultTimeout: time.Millisecond,
	}
	_, err := client.PollForToken(context.Background(), &DeviceCodeResponse{DeviceCode: "device", Interval: 1})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("PollForToken() error = %v", err)
	}
}

func testDeviceClient(serverURL string) *DeviceFlowClient {
	parsed, _ := url.Parse(serverURL)
	return &DeviceFlowClient{
		httpClient:     &http.Client{Timeout: time.Second},
		clientID:       "test-client",
		domain:         parsed.Host,
		deviceCodeURL:  serverURL + "/login/device/code",
		tokenURL:       serverURL + "/login/oauth/access_token",
		defaultTimeout: time.Second,
	}
}
