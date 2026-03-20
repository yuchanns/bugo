package codexauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-kratos/blades"
	bladestools "github.com/go-kratos/blades/tools"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	log "github.com/yuchanns/bugo/internal/logging"
	"github.com/yuchanns/bugo/internal/modelparts"
)

const (
	oauthClientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	oauthAuthorizeURL  = "https://auth.openai.com/oauth/authorize"
	oauthTokenURL      = "https://auth.openai.com/oauth/token"
	oauthRedirectURI   = "http://localhost:1455/auth/callback"
	oauthScopes        = "openid profile email offline_access"
	backendBaseURL     = "https://chatgpt.com/backend-api/"
	modelsURL          = backendBaseURL + "codex/models?client_version=1.0.0"
	authCallbackAddr   = ":1455"
	authExpiryBuffer   = 5 * time.Minute
	authPendingTimeout = 10 * time.Minute
	codexOriginator    = "codex_cli_rs"
	codexClientVersion = "1.0.0"
)

type Config struct {
	Model           string
	AuthFile        string
	MaxOutputTokens int64
	HTTPClient      *http.Client
}

type Provider struct {
	model           string
	authFile        string
	maxOutputTokens int64
	httpClient      *http.Client
	client          openai.Client

	mu      sync.RWMutex
	tokens  *tokens
	pending *pendingAuth

	refreshMu  sync.Mutex
	serverOnce sync.Once
	serverErr  error
}

type tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	AccountID    string    `json:"account_id"`
}

type pendingAuth struct {
	State       string    `json:"state"`
	Verifier    string    `json:"verifier"`
	StartedAt   time.Time `json:"started_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	RedirectURI string    `json:"redirect_uri"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

type codexTransport struct {
	base     http.RoundTripper
	provider *Provider
}

func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("codex model is required")
	}
	if strings.TrimSpace(cfg.AuthFile) == "" {
		return nil, fmt.Errorf("codex auth file is required")
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}

	p := &Provider{
		model:           strings.TrimSpace(cfg.Model),
		authFile:        strings.TrimSpace(cfg.AuthFile),
		maxOutputTokens: cfg.MaxOutputTokens,
		httpClient:      client,
	}
	if err := p.loadTokens(); err != nil {
		return nil, err
	}

	p.client = openai.NewClient(
		option.WithBaseURL(backendBaseURL),
		option.WithAPIKey("codex-auth-placeholder"),
		option.WithHTTPClient(p.newResponsesHTTPClient()),
	)
	return p, nil
}

func (p *Provider) Name() string {
	return p.model
}

func (p *Provider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	params, err := p.toResponsesParams(ctx, req)
	if err != nil {
		return nil, err
	}
	response, err := p.client.Responses.New(ctx, params)
	if err != nil {
		return nil, err
	}
	return responseToModelResponse(response), nil
}

func (p *Provider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		params, err := p.toResponsesParams(ctx, req)
		if err != nil {
			yield(nil, err)
			return
		}

		streaming := p.client.Responses.NewStreaming(ctx, params)
		defer streaming.Close()

		for streaming.Next() {
			switch ev := streaming.Current().AsAny().(type) {
			case responses.ResponseTextDeltaEvent:
				if ev.Delta == "" {
					continue
				}
				if !yield(incompleteTextResponse(ev.Delta), nil) {
					return
				}
			case responses.ResponseReasoningSummaryTextDeltaEvent:
				if ev.Delta == "" {
					continue
				}
				if !yield(incompleteReasoningResponse(ev.Delta), nil) {
					return
				}
			case responses.ResponseReasoningTextDeltaEvent:
				if ev.Delta == "" {
					continue
				}
				if !yield(incompleteReasoningResponse(ev.Delta), nil) {
					return
				}
			case responses.ResponseCompletedEvent:
				if !yield(responseToModelResponse(&ev.Response), nil) {
					return
				}
				return
			case responses.ResponseFailedEvent:
				yield(nil, fmt.Errorf("codex response failed: %s", ev.Response.Error.Message))
				return
			case responses.ResponseIncompleteEvent:
				yield(nil, fmt.Errorf("codex response incomplete: %s", ev.Response.IncompleteDetails.Reason))
				return
			case responses.ResponseErrorEvent:
				yield(nil, fmt.Errorf("codex response error: %s", ev.Message))
				return
			}
		}
		if err := streaming.Err(); err != nil {
			yield(nil, err)
			return
		}
		yield(nil, blades.ErrNoFinalResponse)
	}
}

func (p *Provider) StartLogin() (string, error) {
	if err := p.ensureCallbackServer(); err != nil {
		return "", err
	}
	verifier, challenge, err := generatePKCEPair()
	if err != nil {
		return "", err
	}
	state, err := randomURLSafeString(32)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.pending = &pendingAuth{
		State:       state,
		Verifier:    verifier,
		StartedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(authPendingTimeout),
		RedirectURI: oauthRedirectURI,
	}
	p.mu.Unlock()

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", oauthClientID)
	q.Set("redirect_uri", oauthRedirectURI)
	q.Set("scope", oauthScopes)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", "codex_cli_rs")
	return oauthAuthorizeURL + "?" + q.Encode(), nil
}

func (p *Provider) CompleteLogin(ctx context.Context, raw string) error {
	code, state, err := parseAuthorizationInput(raw)
	if err != nil {
		return err
	}

	p.mu.RLock()
	pending := p.pending
	p.mu.RUnlock()
	if pending == nil {
		return fmt.Errorf("no login is pending, run ,codex.login first")
	}
	if time.Now().After(pending.ExpiresAt) {
		p.clearPending()
		return fmt.Errorf("pending login expired, run ,codex.login again")
	}
	if state != "" && !constantTimeEqual(state, pending.State) {
		return fmt.Errorf("state mismatch")
	}

	current, err := p.exchangeCode(ctx, code, pending.Verifier)
	if err != nil {
		return err
	}
	if err := p.storeTokens(current); err != nil {
		return err
	}
	p.clearPending()
	return nil
}

func (p *Provider) Status() string {
	p.mu.RLock()
	current := cloneTokens(p.tokens)
	pending := p.pending
	p.mu.RUnlock()

	if current == nil {
		if pending != nil && time.Now().Before(pending.ExpiresAt) {
			return fmt.Sprintf("pending_login expires_at=%s", pending.ExpiresAt.Format(time.RFC3339))
		}
		return "not_authenticated"
	}
	status := "authenticated"
	if time.Now().After(current.ExpiresAt) {
		status = "expired"
	}
	return fmt.Sprintf(
		"%s account_id=%s expires_at=%s auth_file=%s",
		status,
		maskAccountID(current.AccountID),
		current.ExpiresAt.Format(time.RFC3339),
		p.authFile,
	)
}

func (p *Provider) Logout() error {
	p.mu.Lock()
	p.tokens = nil
	p.pending = nil
	p.mu.Unlock()
	if err := os.Remove(p.authFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (p *Provider) FetchModels(ctx context.Context) ([]byte, error) {
	current, err := p.getValidTokens(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req.Header, current)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := ensureResponseOK(resp); err != nil {
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

func (p *Provider) newResponsesHTTPClient() *http.Client {
	cloned := *p.httpClient
	baseTransport := cloned.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	cloned.Transport = &codexTransport{
		base:     baseTransport,
		provider: p,
	}
	return &cloned
}

func (t *codexTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}

	cloned := req.Clone(req.Context())
	cloned.URL = cloneURL(req.URL)
	cloned.Header = req.Header.Clone()
	rewriteCodexPath(cloned.URL)

	current, err := t.provider.getValidTokens(cloned.Context())
	if err != nil {
		return nil, err
	}
	applyHeaders(cloned.Header, current)
	return t.base.RoundTrip(cloned)
}

func (p *Provider) toResponsesParams(ctx context.Context, req *blades.ModelRequest) (responses.ResponseNewParams, error) {
	if req == nil {
		return responses.ResponseNewParams{}, fmt.Errorf("model request is required")
	}

	baseModel, reasoningEffort := normalizeModelName(p.model)
	input, err := toInputItems(req.Messages)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	tools, err := toTools(req.Tools)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(baseModel),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: input,
		},
		Store:             param.NewOpt(false),
		ParallelToolCalls: param.NewOpt(true),
		Text: responses.ResponseTextConfigParam{
			Verbosity: responses.ResponseTextConfigVerbosityMedium,
		},
		ToolChoice: responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsAuto),
		},
		Tools: tools,
	}
	if instruction := messageText(req.Instruction); instruction != "" {
		params.Instructions = param.NewOpt(instruction)
	}
	if reasoningEffort != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort:  shared.ReasoningEffort(reasoningEffort),
			Summary: shared.ReasoningSummaryAuto,
		}
		params.Include = []responses.ResponseIncludable{
			responses.ResponseIncludableReasoningEncryptedContent,
		}
	}
	if req.OutputSchema != nil {
		format, err := buildResponseFormat(req.OutputSchema)
		if err != nil {
			return responses.ResponseNewParams{}, err
		}
		params.Text.Format = format
	}
	if promptCacheKey := promptCacheKeyFromContext(ctx); promptCacheKey != "" {
		params.PromptCacheKey = param.NewOpt(promptCacheKey)
	}
	return params, nil
}

func promptCacheKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	session, ok := blades.FromSessionContext(ctx)
	if !ok || session == nil {
		return ""
	}
	return strings.TrimSpace(session.ID())
}

func toInputItems(messages []*blades.Message) (responses.ResponseInputParam, error) {
	input := make(responses.ResponseInputParam, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		switch msg.Role {
		case blades.RoleUser:
			text, err := supportedTextOnlyContent(msg)
			if err != nil {
				return nil, err
			}
			if text != "" {
				input = append(input, responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleUser))
			}
		case blades.RoleAssistant:
			text, err := supportedTextOnlyContent(msg)
			if err != nil {
				return nil, err
			}
			if text != "" {
				input = append(input, responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleAssistant))
			}
		case blades.RoleSystem:
			text, err := supportedTextOnlyContent(msg)
			if err != nil {
				return nil, err
			}
			if text != "" {
				input = append(input, responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleDeveloper))
			}
		case blades.RoleTool:
			for _, part := range msg.Parts {
				toolPart, ok := part.(blades.ToolPart)
				if !ok {
					continue
				}
				if strings.TrimSpace(toolPart.Request) != "" {
					input = append(input, responses.ResponseInputItemParamOfFunctionCall(toolPart.Request, toolPart.ID, toolPart.Name))
				}
				if strings.TrimSpace(toolPart.Response) != "" {
					input = append(input, responses.ResponseInputItemParamOfFunctionCallOutput(toolPart.ID, toolPart.Response))
				}
			}
		}
	}
	return input, nil
}

func toTools(toolset []bladestools.Tool) ([]responses.ToolUnionParam, error) {
	if len(toolset) == 0 {
		return nil, nil
	}
	out := make([]responses.ToolUnionParam, 0, len(toolset))
	for _, tool := range toolset {
		if tool == nil {
			continue
		}
		item := responses.FunctionToolParam{
			Name:       tool.Name(),
			Strict:     param.NewOpt(false),
			Parameters: defaultToolParameters(),
		}
		if desc := strings.TrimSpace(tool.Description()); desc != "" {
			item.Description = param.NewOpt(desc)
		}
		if schema := tool.InputSchema(); schema != nil {
			var parameters map[string]any
			b, err := json.Marshal(schema)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(b, &parameters); err != nil {
				return nil, err
			}
			item.Parameters = parameters
		}
		out = append(out, responses.ToolUnionParam{OfFunction: &item})
	}
	return out, nil
}

func buildResponseFormat(schema any) (responses.ResponseFormatTextConfigUnionParam, error) {
	var raw map[string]any
	data, err := json.Marshal(schema)
	if err != nil {
		return responses.ResponseFormatTextConfigUnionParam{}, err
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return responses.ResponseFormatTextConfigUnionParam{}, err
	}

	name := "structured_outputs"
	description := ""
	switch v := schema.(type) {
	case interface{ GetTitle() string }:
		if strings.TrimSpace(v.GetTitle()) != "" {
			name = strings.TrimSpace(v.GetTitle())
		}
	case interface{ TitleValue() string }:
		if strings.TrimSpace(v.TitleValue()) != "" {
			name = strings.TrimSpace(v.TitleValue())
		}
	}
	type schemaMeta struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	var meta schemaMeta
	_ = json.Unmarshal(data, &meta)
	if strings.TrimSpace(meta.Title) != "" {
		name = strings.TrimSpace(meta.Title)
	}
	if strings.TrimSpace(meta.Description) != "" {
		description = strings.TrimSpace(meta.Description)
	}

	format := responses.ResponseFormatTextJSONSchemaConfigParam{
		Name:   name,
		Schema: raw,
		Strict: param.NewOpt(true),
	}
	if description != "" {
		format.Description = param.NewOpt(description)
	}
	return responses.ResponseFormatTextConfigUnionParam{
		OfJSONSchema: &format,
	}, nil
}

func defaultToolParameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func supportedTextOnlyContent(message *blades.Message) (string, error) {
	if message == nil {
		return "", nil
	}
	var parts []string
	for _, part := range message.Parts {
		switch v := part.(type) {
		case modelparts.ReasoningPart:
			continue
		case blades.TextPart:
			if strings.TrimSpace(v.Text) != "" {
				parts = append(parts, v.Text)
			}
		case blades.ToolPart:
			continue
		case blades.FilePart:
			return "", fmt.Errorf("codex auth provider does not support file uri parts yet")
		case blades.DataPart:
			return "", fmt.Errorf("codex auth provider does not support binary data parts yet")
		}
	}
	return strings.Join(parts, "\n"), nil
}

func messageText(message *blades.Message) string {
	if message == nil {
		return ""
	}
	text, err := supportedTextOnlyContent(message)
	if err != nil {
		return ""
	}
	return text
}

func normalizeModelName(model string) (string, string) {
	base := strings.TrimSpace(model)
	switch {
	case strings.HasSuffix(base, "-minimal"):
		return strings.TrimSuffix(base, "-minimal"), string(shared.ReasoningEffortMinimal)
	case strings.HasSuffix(base, "-none"):
		return strings.TrimSuffix(base, "-none"), ""
	case strings.HasSuffix(base, "-low"):
		return strings.TrimSuffix(base, "-low"), string(shared.ReasoningEffortLow)
	case strings.HasSuffix(base, "-medium"):
		return strings.TrimSuffix(base, "-medium"), string(shared.ReasoningEffortMedium)
	case strings.HasSuffix(base, "-high"):
		return strings.TrimSuffix(base, "-high"), string(shared.ReasoningEffortHigh)
	case strings.HasSuffix(base, "-xhigh"):
		return strings.TrimSuffix(base, "-xhigh"), string(shared.ReasoningEffortHigh)
	default:
		return base, string(shared.ReasoningEffortHigh)
	}
}

func responseToModelResponse(resp *responses.Response) *blades.ModelResponse {
	message := blades.NewAssistantMessage(blades.StatusCompleted)
	if resp == nil {
		return &blades.ModelResponse{Message: message}
	}

	if resp.Usage.TotalTokens > 0 || resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		message.TokenUsage = blades.TokenUsage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		}
	}

	var toolParts []blades.Part
	for _, item := range resp.Output {
		switch output := item.AsAny().(type) {
		case responses.ResponseOutputMessage:
			for _, content := range output.Content {
				switch part := content.AsAny().(type) {
				case responses.ResponseOutputText:
					if part.Text != "" {
						message.Parts = append(message.Parts, blades.TextPart{Text: part.Text})
					}
				case responses.ResponseOutputRefusal:
					if part.Refusal != "" {
						message.Parts = append(message.Parts, blades.TextPart{Text: part.Refusal})
					}
				}
			}
		case responses.ResponseFunctionToolCall:
			toolParts = append(toolParts, blades.ToolPart{
				ID:      output.CallID,
				Name:    output.Name,
				Request: output.Arguments,
			})
		}
	}

	if len(toolParts) > 0 {
		message.Role = blades.RoleTool
		message.Parts = append(message.Parts, toolParts...)
	}
	return &blades.ModelResponse{Message: message}
}

func incompleteTextResponse(delta string) *blades.ModelResponse {
	return &blades.ModelResponse{
		Message: &blades.Message{
			ID:     blades.NewMessageID(),
			Role:   blades.RoleAssistant,
			Status: blades.StatusIncomplete,
			Parts:  []blades.Part{blades.TextPart{Text: delta}},
		},
	}
}

func incompleteReasoningResponse(delta string) *blades.ModelResponse {
	return &blades.ModelResponse{
		Message: &blades.Message{
			ID:     blades.NewMessageID(),
			Role:   blades.RoleAssistant,
			Status: blades.StatusIncomplete,
			Parts: []blades.Part{
				modelparts.ReasoningPart{ReasoningText: delta},
			},
		},
	}
}

func ensureResponseOK(resp *http.Response) error {
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("codex api error: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
}

func (p *Provider) getValidTokens(ctx context.Context) (*tokens, error) {
	p.mu.RLock()
	current := cloneTokens(p.tokens)
	p.mu.RUnlock()
	if current == nil {
		return nil, fmt.Errorf("codex auth is not configured, run ,codex.login first")
	}
	if time.Now().Before(current.ExpiresAt.Add(-authExpiryBuffer)) {
		return current, nil
	}

	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()

	p.mu.RLock()
	current = cloneTokens(p.tokens)
	p.mu.RUnlock()
	if current == nil {
		return nil, fmt.Errorf("codex auth is not configured, run ,codex.login first")
	}
	if time.Now().Before(current.ExpiresAt.Add(-authExpiryBuffer)) {
		return current, nil
	}

	refreshed, err := p.refreshTokens(ctx, current.RefreshToken)
	if err != nil {
		return nil, err
	}
	if err := p.storeTokens(refreshed); err != nil {
		return nil, err
	}
	return cloneTokens(refreshed), nil
}

func (p *Provider) refreshTokens(ctx context.Context, refreshToken string) (*tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", oauthClientID)

	tokenResp, err := p.doTokenRequest(ctx, form)
	if err != nil {
		return nil, err
	}
	accountID, err := extractAccountID(tokenResp.AccessToken)
	if err != nil {
		return nil, err
	}
	if tokenResp.RefreshToken == "" {
		tokenResp.RefreshToken = refreshToken
	}
	return &tokens{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
		AccountID:    accountID,
	}, nil
}

func (p *Provider) exchangeCode(ctx context.Context, code, verifier string) (*tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", oauthClientID)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", oauthRedirectURI)

	tokenResp, err := p.doTokenRequest(ctx, form)
	if err != nil {
		return nil, err
	}
	accountID, err := extractAccountID(tokenResp.AccessToken)
	if err != nil {
		return nil, err
	}
	return &tokens{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
		AccountID:    accountID,
	}, nil
}

func (p *Provider) doTokenRequest(ctx context.Context, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("codex token exchange failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokenResp tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, err
	}
	return &tokenResp, nil
}

func (p *Provider) ensureCallbackServer() error {
	p.serverOnce.Do(func() {
		listener, err := net.Listen("tcp", authCallbackAddr)
		if err != nil {
			p.serverErr = err
			return
		}
		server := &http.Server{
			Handler: http.HandlerFunc(p.handleCallback),
		}
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error().Err(err).Msg("codex.auth.callback.server.failed")
			}
		}()
	})
	return p.serverErr
}

func (p *Provider) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/auth/callback" {
		http.NotFound(w, r)
		return
	}

	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		writeCallbackHTML(w, http.StatusBadRequest, "Codex login failed", "Missing authorization code.")
		return
	}

	if err := p.CompleteLogin(r.Context(), r.URL.String()); err != nil {
		log.Error().Err(err).Msg("codex.auth.complete.failed")
		writeCallbackHTML(w, http.StatusInternalServerError, "Codex login failed", err.Error())
		return
	}
	writeCallbackHTML(w, http.StatusOK, "Codex login complete", "Authentication succeeded. You can return to Telegram and run ,codex.status.")
}

func writeCallbackHTML(w http.ResponseWriter, status int, title string, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>"+escapeHTML(title)+"</title></head><body><h1>"+escapeHTML(title)+"</h1><p>"+escapeHTML(message)+"</p></body></html>")
}

func (p *Provider) storeTokens(current *tokens) error {
	if current == nil {
		return fmt.Errorf("tokens are required")
	}
	if err := os.MkdirAll(filepath.Dir(p.authFile), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p.authFile, data, 0o600); err != nil {
		return err
	}
	p.mu.Lock()
	p.tokens = cloneTokens(current)
	p.mu.Unlock()
	return nil
}

func (p *Provider) loadTokens() error {
	data, err := os.ReadFile(p.authFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var current tokens
	if err := json.Unmarshal(data, &current); err != nil {
		return err
	}
	p.mu.Lock()
	p.tokens = cloneTokens(&current)
	p.mu.Unlock()
	return nil
}

func (p *Provider) clearPending() {
	p.mu.Lock()
	p.pending = nil
	p.mu.Unlock()
}

func applyHeaders(header http.Header, current *tokens) {
	header.Set("Authorization", "Bearer "+current.AccessToken)
	header.Set("chatgpt-account-id", current.AccountID)
	header.Set("originator", codexOriginator)
	header.Set("User-Agent", codexUserAgent())
	header.Set("OpenAI-Beta", "responses=experimental")
	stripStainlessHeaders(header)
}

func stripStainlessHeaders(header http.Header) {
	for key := range header {
		if strings.HasPrefix(http.CanonicalHeaderKey(key), "X-Stainless-") {
			header.Del(key)
		}
	}
}

func codexUserAgent() string {
	return fmt.Sprintf(
		"%s/%s (%s; %s)",
		codexOriginator,
		codexClientVersion,
		normalizedCodexOS(),
		normalizedCodexArch(),
	)
}

func normalizedCodexOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "Mac OS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	default:
		return runtime.GOOS
	}
}

func normalizedCodexArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "arm64"
	default:
		return runtime.GOARCH
	}
}

func rewriteCodexPath(u *url.URL) {
	if u == nil {
		return
	}
	u.Path = strings.Replace(u.Path, "/backend-api/responses", "/backend-api/codex/responses", 1)
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	cloned := *u
	return &cloned
}

func cloneTokens(current *tokens) *tokens {
	if current == nil {
		return nil
	}
	cloned := *current
	return &cloned
}

func extractAccountID(accessToken string) (string, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid jwt")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return "", err
	}
	if authClaim, ok := payload["https://api.openai.com/auth"].(map[string]any); ok {
		if accountID, ok := authClaim["chatgpt_account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
			return accountID, nil
		}
	}
	if sub, ok := payload["sub"].(string); ok && strings.TrimSpace(sub) != "" {
		return sub, nil
	}
	return "", fmt.Errorf("chatgpt account id not found in access token")
}

func generatePKCEPair() (string, string, error) {
	verifier, err := randomURLSafeString(64)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randomURLSafeString(length int) (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	buf := make([]byte, length)
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(buf), nil
}

func maskAccountID(accountID string) string {
	accountID = strings.TrimSpace(accountID)
	if len(accountID) <= 8 {
		return accountID
	}
	return accountID[:8] + "..."
}

func escapeHTML(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#39;",
	)
	return replacer.Replace(s)
}

func constantTimeEqual(a string, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func parseAuthorizationInput(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("authorization input is required")
	}

	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", err
		}
		return parseAuthorizationValues(u.Query())
	}

	if strings.Contains(raw, "code=") || strings.Contains(raw, "state=") {
		values, err := url.ParseQuery(strings.TrimPrefix(raw, "?"))
		if err != nil {
			return "", "", err
		}
		return parseAuthorizationValues(values)
	}

	return raw, "", nil
}

func parseAuthorizationValues(values url.Values) (string, string, error) {
	code := strings.TrimSpace(values.Get("code"))
	state := strings.TrimSpace(values.Get("state"))
	if code == "" {
		return "", "", fmt.Errorf("authorization code not found")
	}
	return code, state, nil
}
