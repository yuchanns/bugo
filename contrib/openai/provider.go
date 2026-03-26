package openai

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/go-kratos/blades"
	"github.com/go-kratos/blades/contrib/openai"
	"github.com/go-kratos/blades/tools"
	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	log "github.com/yuchanns/bugo/internal/logging"
	"github.com/yuchanns/bugo/internal/modelctx"
	"github.com/yuchanns/bugo/internal/modelparts"
	"github.com/yuchanns/bugo/internal/wireapi"
)

const (
	WireAPIChat      = wireapi.Chat
	WireAPIResponses = wireapi.Responses
)

type Config struct {
	Model           string
	BaseURL         string
	APIKey          string
	MaxOutputTokens int64
	HTTPClient      *http.Client
	WireAPI         string
}

type Provider struct {
	wireAPI   string
	base      blades.ModelProvider
	responses *responsesModel
}

type responsesModel struct {
	model           string
	maxOutputTokens int64
	client          openaisdk.Client

	chainMu sync.Mutex
	chains  map[string]responseChainState
}

type responseChainState struct {
	ResponseID         string
	RequestFingerprint string
	MessageCount       int
	MessagePrefixHash  string
}

type responseRequestPlan struct {
	Params                responses.ResponseNewParams
	SessionID             string
	RequestFingerprint    string
	MessageCount          int
	MessagePrefixHash     string
	PreviousResponseReady bool
	UsedPreviousResponse  bool
	ResponseChainDisabled bool
	DecisionReason        string
	DeltaMessageCount     int
}

func New(cfg Config) (*Provider, error) {
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, fmt.Errorf("openai model is required")
	}

	wireAPI, err := NormalizeWireAPI(cfg.WireAPI)
	if err != nil {
		return nil, err
	}

	opts := make([]option.RequestOption, 0, 3)
	if strings.TrimSpace(cfg.BaseURL) != "" {
		opts = append(opts, option.WithBaseURL(strings.TrimSpace(cfg.BaseURL)))
	}
	if strings.TrimSpace(cfg.APIKey) != "" {
		opts = append(opts, option.WithAPIKey(strings.TrimSpace(cfg.APIKey)))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}

	provider := &Provider{wireAPI: wireAPI}
	switch wireAPI {
	case WireAPIChat:
		provider.base = openai.NewModel(model, openai.Config{
			BaseURL:         strings.TrimSpace(cfg.BaseURL),
			APIKey:          strings.TrimSpace(cfg.APIKey),
			MaxOutputTokens: cfg.MaxOutputTokens,
			RequestOptions:  opts,
		})
	case WireAPIResponses:
		responsesProvider := &responsesModel{
			model:           model,
			maxOutputTokens: cfg.MaxOutputTokens,
			client:          openaisdk.NewClient(opts...),
			chains:          map[string]responseChainState{},
		}
		provider.base = responsesProvider
		provider.responses = responsesProvider
	default:
		return nil, fmt.Errorf("unsupported openai wire api %q", wireAPI)
	}

	return provider, nil
}

func (p *Provider) Name() string {
	if p == nil || p.base == nil {
		return ""
	}
	return p.base.Name()
}

func (p *Provider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	if p == nil || p.base == nil {
		return nil, fmt.Errorf("openai provider is not initialized")
	}
	return p.base.Generate(ctx, req)
}

func (p *Provider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	if p == nil || p.base == nil {
		return func(yield func(*blades.ModelResponse, error) bool) {
			yield(nil, fmt.Errorf("openai provider is not initialized"))
		}
	}
	return p.base.NewStreaming(ctx, req)
}

func (p *Provider) ResetSessionChain(sessionID string) {
	if p == nil || p.responses == nil {
		return
	}
	p.responses.ResetSessionChain(sessionID)
}

func (p *Provider) WireAPI() string {
	if p == nil {
		return ""
	}
	return p.wireAPI
}

func NormalizeWireAPI(raw string) (string, error) {
	return wireapi.Normalize(raw)
}

func (m *responsesModel) Name() string {
	if m == nil {
		return ""
	}
	return m.model
}

func (m *responsesModel) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	plan, err := m.buildResponseRequestPlan(ctx, req)
	if err != nil {
		return nil, err
	}

	resp, err := m.client.Responses.New(ctx, plan.Params)
	if err != nil {
		m.clearResponseChainOnError(plan)
		return nil, err
	}
	if resp != nil {
		m.rememberResponseChain(plan, resp.ID)
	}
	return responseToModelResponse(resp), nil
}

func (m *responsesModel) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		plan, err := m.buildResponseRequestPlan(ctx, req)
		if err != nil {
			yield(nil, err)
			return
		}

		stream := m.client.Responses.NewStreaming(ctx, plan.Params)
		defer stream.Close()

		for stream.Next() {
			event := stream.Current()
			switch ev := event.AsAny().(type) {
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
				m.rememberResponseChain(plan, ev.Response.ID)
				if !yield(responseToModelResponse(&ev.Response), nil) {
					return
				}
				return
			case responses.ResponseFailedEvent:
				m.clearResponseChainOnError(plan)
				yield(nil, fmt.Errorf("openai response failed: %s", ev.Response.Error.Message))
				return
			case responses.ResponseIncompleteEvent:
				m.clearResponseChainOnError(plan)
				yield(nil, fmt.Errorf("openai response incomplete: %s", ev.Response.IncompleteDetails.Reason))
				return
			case responses.ResponseErrorEvent:
				m.clearResponseChainOnError(plan)
				yield(nil, fmt.Errorf("openai response error: %s", ev.Message))
				return
			}
		}

		if err := stream.Err(); err != nil {
			m.clearResponseChainOnError(plan)
			yield(nil, err)
			return
		}
	}
}

func (m *responsesModel) ResetSessionChain(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	m.chainMu.Lock()
	if m.chains == nil {
		m.chains = map[string]responseChainState{}
	}
	delete(m.chains, sessionID)
	m.chainMu.Unlock()
	log.Info().
		Str("session_id", sessionID).
		Msg("openai.response_chain.reset")
}

func (m *responsesModel) buildResponseRequestPlan(ctx context.Context, req *blades.ModelRequest) (responseRequestPlan, error) {
	if req == nil {
		return responseRequestPlan{}, fmt.Errorf("model request is required")
	}

	baseModel, reasoningEffort := normalizeModelName(m.model)
	requestFingerprint, err := buildRequestFingerprint(baseModel, reasoningEffort, req)
	if err != nil {
		return responseRequestPlan{}, err
	}
	messagePrefixHash, err := hashMessages(req.Messages)
	if err != nil {
		return responseRequestPlan{}, err
	}
	tools, err := toResponseTools(req.Tools)
	if err != nil {
		return responseRequestPlan{}, err
	}

	inputMessages := req.Messages
	sessionID := promptCacheKeyFromContext(ctx)
	responseChainDisabled := modelctx.ResponseChainDisabled(ctx)
	previousResponseReady := false
	usedPreviousResponse := false
	var previousResponseID string
	decisionReason := "no_session"

	if sessionID != "" && !responseChainDisabled {
		inputMessages, previousResponseID, usedPreviousResponse, decisionReason, err = m.resolveIncrementalMessages(sessionID, req, requestFingerprint)
		if err != nil {
			return responseRequestPlan{}, err
		}
		previousResponseReady = usedPreviousResponse
	} else if sessionID != "" && responseChainDisabled {
		decisionReason = "chain_disabled"
	}

	input, err := toInputItems(inputMessages)
	if err != nil {
		return responseRequestPlan{}, err
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
	if m.maxOutputTokens > 0 {
		params.MaxOutputTokens = param.NewOpt(m.maxOutputTokens)
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
			return responseRequestPlan{}, err
		}
		params.Text.Format = format
	}
	if sessionID != "" {
		params.PromptCacheKey = param.NewOpt(sessionID)
	}
	if previousResponseID != "" {
		params.PreviousResponseID = param.NewOpt(previousResponseID)
	}

	plan := responseRequestPlan{
		Params:                params,
		SessionID:             sessionID,
		RequestFingerprint:    requestFingerprint,
		MessageCount:          len(req.Messages),
		MessagePrefixHash:     messagePrefixHash,
		PreviousResponseReady: previousResponseReady,
		UsedPreviousResponse:  usedPreviousResponse,
		ResponseChainDisabled: responseChainDisabled,
		DecisionReason:        decisionReason,
		DeltaMessageCount:     len(inputMessages),
	}

	log.Info().
		Str("session_id", sessionID).
		Str("prompt_cache_key", sessionID).
		Bool("prompt_cache_key_enabled", sessionID != "").
		Bool("previous_response_id_ready", previousResponseReady).
		Bool("previous_response_id_enabled", usedPreviousResponse).
		Str("previous_response_id", previousResponseID).
		Int("history_messages", len(req.Messages)).
		Int("request_messages", len(inputMessages)).
		Str("decision_reason", decisionReason).
		Msg("openai.request.plan")

	return plan, nil
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

func (m *responsesModel) resolveIncrementalMessages(sessionID string, req *blades.ModelRequest, requestFingerprint string) ([]*blades.Message, string, bool, string, error) {
	chain, ok := m.loadResponseChain(sessionID)
	if !ok || chain.ResponseID == "" {
		return req.Messages, "", false, "chain_missing", nil
	}
	if chain.RequestFingerprint != requestFingerprint {
		return req.Messages, "", false, "fingerprint_mismatch", nil
	}
	if chain.MessageCount <= 0 || chain.MessageCount >= len(req.Messages) {
		return req.Messages, "", false, "message_count_not_advanced", nil
	}

	prefixHash, err := hashMessages(req.Messages[:chain.MessageCount])
	if err != nil {
		return nil, "", false, "", err
	}
	if prefixHash != chain.MessagePrefixHash {
		return req.Messages, "", false, "prefix_hash_mismatch", nil
	}
	return cloneMessages(req.Messages[chain.MessageCount:]), chain.ResponseID, true, "incremental", nil
}

func (m *responsesModel) loadResponseChain(sessionID string) (responseChainState, bool) {
	m.chainMu.Lock()
	defer m.chainMu.Unlock()
	if m.chains == nil {
		m.chains = map[string]responseChainState{}
	}
	chain, ok := m.chains[sessionID]
	return chain, ok
}

func (m *responsesModel) rememberResponseChain(plan responseRequestPlan, responseID string) {
	if m == nil || plan.SessionID == "" || responseID == "" || plan.ResponseChainDisabled {
		return
	}

	state := responseChainState{
		ResponseID:         strings.TrimSpace(responseID),
		RequestFingerprint: plan.RequestFingerprint,
		MessageCount:       plan.MessageCount,
		MessagePrefixHash:  plan.MessagePrefixHash,
	}

	m.chainMu.Lock()
	if m.chains == nil {
		m.chains = map[string]responseChainState{}
	}
	m.chains[plan.SessionID] = state
	m.chainMu.Unlock()

	log.Info().
		Str("session_id", plan.SessionID).
		Str("response_id", state.ResponseID).
		Int("message_count", state.MessageCount).
		Msg("openai.response_chain.save")
}

func (m *responsesModel) clearResponseChainOnError(plan responseRequestPlan) {
	if !plan.UsedPreviousResponse || plan.SessionID == "" {
		return
	}
	m.ResetSessionChain(plan.SessionID)
}

func buildRequestFingerprint(model string, reasoningEffort string, req *blades.ModelRequest) (string, error) {
	var toolPayload any
	if len(req.Tools) > 0 {
		tools, err := toResponseTools(req.Tools)
		if err != nil {
			return "", err
		}
		toolPayload = tools
	}
	var outputSchema any
	if req.OutputSchema != nil {
		raw, err := schemaJSONValue(req.OutputSchema)
		if err != nil {
			return "", err
		}
		outputSchema = raw
	}

	payload := map[string]any{
		"model":            model,
		"instruction":      messageText(req.Instruction),
		"reasoning_effort": reasoningEffort,
		"tools":            toolPayload,
		"output_schema":    outputSchema,
	}
	return stableHash(payload)
}

func hashMessages(messages []*blades.Message) (string, error) {
	input, err := toInputItems(messages)
	if err != nil {
		return "", err
	}
	return stableHash(input)
}

func schemaJSONValue(schema any) (any, error) {
	if schema == nil {
		return nil, nil
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func stableHash(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func cloneMessages(messages []*blades.Message) []*blades.Message {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]*blades.Message, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			cloned = append(cloned, nil)
			continue
		}
		cloned = append(cloned, msg.Clone())
	}
	return cloned
}

func toInputItems(messages []*blades.Message) (responses.ResponseInputParam, error) {
	input := make(responses.ResponseInputParam, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		switch msg.Role {
		case blades.RoleUser, blades.RoleAssistant, blades.RoleSystem:
			content, err := toInputMessageContent(msg)
			if err != nil {
				return nil, err
			}
			if len(content) == 0 {
				continue
			}
			input = append(input, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role:    inputMessageRole(msg.Role),
					Content: responses.EasyInputMessageContentUnionParam{OfInputItemContentList: content},
				},
			})
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

func inputMessageRole(role blades.Role) responses.EasyInputMessageRole {
	switch role {
	case blades.RoleAssistant:
		return responses.EasyInputMessageRoleAssistant
	case blades.RoleSystem:
		return responses.EasyInputMessageRoleSystem
	default:
		return responses.EasyInputMessageRoleUser
	}
}

func toInputMessageContent(message *blades.Message) (responses.ResponseInputMessageContentListParam, error) {
	content := make(responses.ResponseInputMessageContentListParam, 0, len(message.Parts))
	for _, part := range message.Parts {
		switch v := part.(type) {
		case modelparts.ReasoningPart:
			continue
		case blades.TextPart:
			if strings.TrimSpace(v.Text) == "" {
				continue
			}
			content = append(content, responses.ResponseInputContentParamOfInputText(v.Text))
		case blades.FilePart:
			item, err := filePartToInputContent(v)
			if err != nil {
				return nil, err
			}
			content = append(content, item)
		case blades.DataPart:
			item, err := dataPartToInputContent(v)
			if err != nil {
				return nil, err
			}
			content = append(content, item)
		case blades.ToolPart:
			continue
		default:
			return nil, fmt.Errorf("openai responses provider does not support message part %T", part)
		}
	}
	return content, nil
}

func filePartToInputContent(part blades.FilePart) (responses.ResponseInputContentUnionParam, error) {
	switch part.MIMEType.Type() {
	case "image":
		return responses.ResponseInputContentUnionParam{
			OfInputImage: &responses.ResponseInputImageParam{
				Detail:   responses.ResponseInputImageDetailAuto,
				ImageURL: param.NewOpt(part.URI),
			},
		}, nil
	case "audio", "video":
		return responses.ResponseInputContentUnionParam{}, fmt.Errorf("openai responses provider does not support %s file uri parts yet", part.MIMEType.Type())
	default:
		fileParam := &responses.ResponseInputFileParam{
			FileURL: param.NewOpt(part.URI),
		}
		if strings.TrimSpace(part.Name) != "" {
			fileParam.Filename = param.NewOpt(strings.TrimSpace(part.Name))
		}
		return responses.ResponseInputContentUnionParam{
			OfInputFile: fileParam,
		}, nil
	}
}

func dataPartToInputContent(part blades.DataPart) (responses.ResponseInputContentUnionParam, error) {
	switch part.MIMEType.Type() {
	case "image":
		base64Data := "data:" + string(part.MIMEType) + ";base64," + base64.StdEncoding.EncodeToString(part.Bytes)
		return responses.ResponseInputContentUnionParam{
			OfInputImage: &responses.ResponseInputImageParam{
				Detail:   responses.ResponseInputImageDetailAuto,
				ImageURL: param.NewOpt(base64Data),
			},
		}, nil
	case "audio", "video":
		return responses.ResponseInputContentUnionParam{}, fmt.Errorf("openai responses provider does not support %s binary parts yet", part.MIMEType.Type())
	default:
		fileParam := &responses.ResponseInputFileParam{
			FileData: param.NewOpt(base64.StdEncoding.EncodeToString(part.Bytes)),
		}
		if strings.TrimSpace(part.Name) != "" {
			fileParam.Filename = param.NewOpt(strings.TrimSpace(part.Name))
		}
		return responses.ResponseInputContentUnionParam{
			OfInputFile: fileParam,
		}, nil
	}
}

func toResponseTools(toolset []tools.Tool) ([]responses.ToolUnionParam, error) {
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
			return "", fmt.Errorf("openai provider does not support file uri instructions yet")
		case blades.DataPart:
			return "", fmt.Errorf("openai provider does not support binary instructions yet")
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
