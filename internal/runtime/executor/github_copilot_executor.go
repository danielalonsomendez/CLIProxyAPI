package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	githubcopilotauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/githubcopilot"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/githubcopilot"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// GitHubCopilotExecutor routes requests through the endpoint advertised for each model.
type GitHubCopilotExecutor struct {
	cfg *config.Config
}

// NewGitHubCopilotExecutor creates the native Copilot executor.
func NewGitHubCopilotExecutor(cfg *config.Config) *GitHubCopilotExecutor {
	return &GitHubCopilotExecutor{cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *GitHubCopilotExecutor) Identifier() string {
	return githubcopilotauth.Provider
}

// RequestToFormat reports the account-scoped upstream format after auth selection.
func (e *GitHubCopilotExecutor) RequestToFormat(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format {
	authIDValue := ""
	if opts.Metadata != nil {
		authIDValue, _ = opts.Metadata[cliproxyexecutor.SelectedAuthMetadataKey].(string)
	}
	modelID := thinking.ParseSuffix(req.Model).ModelName
	if model, ok := githubcopilot.LookupModel(authIDValue, modelID); ok {
		return githubCopilotEndpointFormat(model.PreferredEndpoint())
	}
	return ""
}

// PrepareRequest injects the account token and common Copilot headers.
func (e *GitHubCopilotExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _ := githubCopilotCredentials(auth)
	if token == "" {
		return statusErr{code: http.StatusUnauthorized, msg: "github copilot: missing access token"}
	}
	githubcopilot.ApplyHeaders(req, token, githubcopilotauth.APIVersion(e.cfg), false, false)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest sends an arbitrary authenticated request through the selected proxy.
func (e *GitHubCopilotExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("github copilot executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	return helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0).Do(httpReq)
}

// Execute performs a non-streaming request using Messages, Responses, or Chat Completions.
func (e *GitHubCopilotExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "github copilot: /responses/compact is not supported"}
	}
	prepared, err := e.prepare(ctx, auth, req, opts, false)
	if err != nil {
		return resp, err
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, prepared.modelID, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(prepared.body, prepared.format.String())

	httpReq, err := e.newRequest(ctx, auth, prepared, false)
	if err != nil {
		return resp, err
	}
	e.recordRequest(ctx, auth, httpReq, prepared.body)
	httpClient := reporter.TrackHTTPClient(helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.WithError(errClose).Error("github copilot executor: close response body")
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		err = statusErr{code: httpResp.StatusCode, msg: string(body)}
		return resp, err
	}
	e.publishUsage(ctx, reporter, prepared.format, body)
	reporter.EnsurePublished(ctx)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, prepared.format, prepared.responseFormat, req.Model, opts.OriginalRequest, prepared.body, body, &param)
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

// ExecuteStream performs a streaming request using the model's advertised endpoint.
func (e *GitHubCopilotExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "github copilot: /responses/compact is not supported"}
	}
	prepared, err := e.prepare(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, prepared.modelID, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(prepared.body, prepared.format.String())

	httpReq, err := e.newRequest(ctx, auth, prepared, true)
	if err != nil {
		return nil, err
	}
	e.recordRequest(ctx, auth, httpReq, prepared.body)
	httpClient := reporter.TrackHTTPClient(helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.WithError(errClose).Error("github copilot executor: close error response body")
		}
		if errRead != nil {
			return nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, body)
		return nil, statusErr{code: httpResp.StatusCode, msg: string(body)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.WithError(errClose).Error("github copilot executor: close stream response body")
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		published := false
		for scanner.Scan() {
			line := bytes.Clone(scanner.Bytes())
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if e.publishStreamUsage(ctx, reporter, prepared.format, line) {
				published = true
			}
			chunks := sdktranslator.TranslateStream(ctx, prepared.format, prepared.responseFormat, req.Model, opts.OriginalRequest, prepared.body, line, &param)
			for _, chunk := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
				case <-ctx.Done():
					return
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil && ctx.Err() == nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
			return
		}
		if !published {
			reporter.EnsurePublished(ctx)
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens provides a local estimate through the existing OpenAI tokenizer helpers.
func (e *GitHubCopilotExecutor) CountTokens(ctx context.Context, _ *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FormatOpenAI
	body := sdktranslator.TranslateRequest(opts.SourceFormat, to, baseModel, req.Payload, false)
	encoder, err := helps.TokenizerForModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("github copilot executor: tokenizer init: %w", err)
	}
	count, err := helps.CountOpenAIChatTokens(encoder, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("github copilot executor: token counting: %w", err)
	}
	usageJSON := helps.BuildOpenAIUsageJSON(count)
	return cliproxyexecutor.Response{Payload: sdktranslator.TranslateTokenCount(ctx, to, responseFormat, count, usageJSON)}, nil
}

// Refresh delegates to Home when available; GitHub does not issue a refresh token here.
func (e *GitHubCopilotExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	return auth, nil
}

type githubCopilotPreparedRequest struct {
	modelID        string
	endpoint       string
	format         sdktranslator.Format
	responseFormat sdktranslator.Format
	body           []byte
	baseURL        string
	token          string
	vision         bool
}

func (e *GitHubCopilotExecutor) prepare(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (githubCopilotPreparedRequest, error) {
	modelID := thinking.ParseSuffix(req.Model).ModelName
	token, enterpriseDomain := githubCopilotCredentials(auth)
	if token == "" {
		return githubCopilotPreparedRequest{}, statusErr{code: http.StatusUnauthorized, msg: "github copilot: missing access token"}
	}
	baseURL, err := githubcopilotauth.APIBaseURL(e.cfg, enterpriseDomain)
	if err != nil {
		return githubCopilotPreparedRequest{}, err
	}
	model, ok := githubcopilot.LookupModel(authID(auth), modelID)
	if !ok {
		client := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
		models, errFetch := githubcopilot.FetchModels(ctx, client, baseURL, token, githubcopilotauth.APIVersion(e.cfg))
		if errFetch != nil {
			return githubCopilotPreparedRequest{}, errFetch
		}
		githubcopilot.StoreCatalog(authID(auth), models)
		model, ok = githubcopilot.LookupModel(authID(auth), modelID)
	}
	if !ok {
		return githubCopilotPreparedRequest{}, statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("github copilot: model %q is not enabled for this account", modelID)}
	}
	endpoint := model.PreferredEndpoint()
	format := githubCopilotEndpointFormat(endpoint)
	if format == "" {
		return githubCopilotPreparedRequest{}, statusErr{code: http.StatusBadGateway, msg: fmt.Sprintf("github copilot: model %q has no supported endpoint", modelID)}
	}
	original := req.Payload
	if len(opts.OriginalRequest) > 0 {
		original = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(opts.SourceFormat, format, modelID, original, stream)
	body := sdktranslator.TranslateRequest(opts.SourceFormat, format, modelID, req.Payload, stream)
	body, _ = sjson.SetBytes(body, "model", modelID)
	body, _ = sjson.SetBytes(body, "stream", stream)
	body, err = thinking.ApplyThinking(body, req.Model, opts.SourceFormat.String(), format.String(), e.Identifier())
	if err != nil {
		return githubCopilotPreparedRequest{}, err
	}
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, modelID, format.String(), opts.SourceFormat.String(), "", body, originalTranslated, helps.PayloadRequestedModel(opts, req.Model), helps.PayloadRequestPath(opts), opts.Headers)
	if endpoint == githubcopilot.EndpointMessages && !gjson.GetBytes(body, "max_tokens").Exists() {
		maxTokens := model.Capabilities.Limits.MaxOutputTokens
		if maxTokens <= 0 {
			maxTokens = 4096
		}
		body, _ = sjson.SetBytes(body, "max_tokens", maxTokens)
	}
	if endpoint == githubcopilot.EndpointChatCompletions && stream {
		body, _ = sjson.SetBytes(body, "stream_options.include_usage", true)
	}
	if strings.HasPrefix(strings.ToLower(model.Capabilities.Family), "gpt") || strings.HasPrefix(strings.ToLower(modelID), "gpt") {
		body, _ = sjson.DeleteBytes(body, "max_output_tokens")
	}
	return githubCopilotPreparedRequest{
		modelID:        modelID,
		endpoint:       endpoint,
		format:         format,
		responseFormat: cliproxyexecutor.ResponseFormatOrSource(opts),
		body:           body,
		baseURL:        baseURL,
		token:          token,
		vision:         githubCopilotPayloadHasVision(body),
	}, nil
}

func (e *GitHubCopilotExecutor) newRequest(ctx context.Context, auth *cliproxyauth.Auth, prepared githubCopilotPreparedRequest, stream bool) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(prepared.baseURL, "/")+prepared.endpoint, bytes.NewReader(prepared.body))
	if err != nil {
		return nil, err
	}
	githubcopilot.ApplyHeaders(httpReq, prepared.token, githubcopilotauth.APIVersion(e.cfg), stream, prepared.vision)
	httpReq.Header.Set("X-Initiator", githubCopilotInitiator(prepared.body))
	if prepared.endpoint == githubcopilot.EndpointMessages {
		httpReq.Header.Set("Anthropic-Version", "2023-06-01")
		if gjson.GetBytes(prepared.body, "thinking").Exists() {
			httpReq.Header.Set("Anthropic-Beta", "interleaved-thinking-2025-05-14")
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	return httpReq, nil
}

func (e *GitHubCopilotExecutor) recordRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request, body []byte) {
	var authIDValue, authLabel, authType, authValue string
	if auth != nil {
		authIDValue = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL: req.URL.String(), Method: req.Method, Headers: req.Header.Clone(), Body: body,
		Provider: e.Identifier(), AuthID: authIDValue, AuthLabel: authLabel, AuthType: authType, AuthValue: authValue,
	})
}

func (e *GitHubCopilotExecutor) publishUsage(ctx context.Context, reporter *helps.UsageReporter, format sdktranslator.Format, body []byte) {
	if format == sdktranslator.FormatClaude {
		reporter.Publish(ctx, helps.ParseClaudeUsage(body))
		return
	}
	reporter.Publish(ctx, helps.ParseOpenAIUsage(body))
}

func (e *GitHubCopilotExecutor) publishStreamUsage(ctx context.Context, reporter *helps.UsageReporter, format sdktranslator.Format, line []byte) bool {
	if format == sdktranslator.FormatClaude {
		if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
			reporter.Publish(ctx, detail)
			return true
		}
		return false
	}
	if detail, ok := helps.ParseOpenAIStreamUsage(line); ok {
		reporter.Publish(ctx, detail)
		return true
	}
	return false
}

func githubCopilotEndpointFormat(endpoint string) sdktranslator.Format {
	switch endpoint {
	case githubcopilot.EndpointMessages:
		return sdktranslator.FormatClaude
	case githubcopilot.EndpointResponses:
		return sdktranslator.FormatOpenAIResponse
	case githubcopilot.EndpointChatCompletions:
		return sdktranslator.FormatOpenAI
	default:
		return ""
	}
}

func githubCopilotCredentials(auth *cliproxyauth.Auth) (token, enterpriseDomain string) {
	if auth == nil {
		return "", ""
	}
	if auth.Metadata != nil {
		for _, key := range []string{"access_token", "github_token", "access", "token"} {
			if token = metadataString(auth.Metadata, key); token != "" {
				break
			}
		}
		for _, key := range []string{"enterprise_domain", "enterprise_url"} {
			if enterpriseDomain = metadataString(auth.Metadata, key); enterpriseDomain != "" {
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
			token = auth.Attributes["api_key"]
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

func authID(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return auth.ID
}

func githubCopilotPayloadHasVision(body []byte) bool {
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte(`"type":"image"`)) ||
		bytes.Contains(lower, []byte(`"type":"image_url"`)) ||
		bytes.Contains(lower, []byte(`"source":{"type":"base64"`))
}

func githubCopilotInitiator(body []byte) string {
	root := gjson.ParseBytes(body)
	for _, path := range []string{"messages", "input"} {
		items := root.Get(path)
		if !items.IsArray() {
			continue
		}
		values := items.Array()
		if len(values) == 0 {
			continue
		}
		last := values[len(values)-1]
		role := strings.ToLower(last.Get("role").String())
		typeName := strings.ToLower(last.Get("type").String())
		if role == "assistant" || role == "tool" || strings.Contains(typeName, "tool") || strings.Contains(typeName, "function") {
			return "agent"
		}
	}
	return "user"
}
