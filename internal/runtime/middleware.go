package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/go-kratos/blades"
	bladesmiddleware "github.com/go-kratos/blades/middleware"
	bladestools "github.com/go-kratos/blades/tools"
	kitretry "github.com/go-kratos/kit/retry"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/openai/openai-go/v3"
	tiktoken "github.com/pkoukk/tiktoken-go"
	log "github.com/yuchanns/bugo/internal/logging"
)

const tapeContextMessageLimit = 10

const (
	contextBudgetWarnRatio     = 0.80
	contextBudgetCriticalRatio = 0.90
)

func AgentRetryMiddleware() blades.Middleware {
	return bladesmiddleware.Retry(
		5,
		kitretry.WithBaseDelay(300*time.Millisecond),
		kitretry.WithMaxDelay(2*time.Second),
		kitretry.WithRetryable(isRetryableAgentError),
	)
}

func isRetryableAgentError(err error) bool {
	if err == nil {
		return false
	}

	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusRequestTimeout ||
			apiErr.StatusCode == http.StatusConflict ||
			apiErr.StatusCode == http.StatusTooManyRequests ||
			apiErr.StatusCode >= http.StatusInternalServerError
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}

	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH)
}

type promptBudgetEstimator struct {
	limit   int
	encoder *tiktoken.Tiktoken
}

func ContextBudgetMiddleware(model string, limit int) blades.Middleware {
	estimator := newPromptBudgetEstimator(model, limit)
	if estimator == nil {
		return func(next blades.Handler) blades.Handler {
			return next
		}
	}
	return func(next blades.Handler) blades.Handler {
		return blades.HandleFunc(func(ctx context.Context, invocation *blades.Invocation) blades.Generator[*blades.Message, error] {
			if invocation == nil {
				return next.Handle(ctx, invocation)
			}

			tokenCount := estimator.approximateInvocationTokens(invocation)
			ratio := float64(tokenCount) / float64(estimator.limit)
			logContextBudget(invocation, tokenCount, estimator.limit, ratio)

			note := contextBudgetNote(ratio)
			if note == "" {
				return next.Handle(ctx, invocation)
			}

			cloned := invocation.Clone()
			budgetMessage := blades.SystemMessage(note)
			if cloned.Instruction == nil {
				cloned.Instruction = budgetMessage
			} else {
				cloned.Instruction = blades.MergeParts(cloned.Instruction, budgetMessage)
			}
			return next.Handle(ctx, cloned)
		})
	}
}

func newPromptBudgetEstimator(model string, limit int) *promptBudgetEstimator {
	if limit <= 0 {
		return nil
	}

	encoder, err := tiktoken.EncodingForModel(model)
	if err != nil {
		encoder, err = tiktoken.GetEncoding("cl100k_base")
	}
	if err != nil {
		log.Warn().
			Str("model", model).
			Err(err).
			Msg("context.budget.encoder.unavailable")
		return nil
	}

	return &promptBudgetEstimator{
		limit:   limit,
		encoder: encoder,
	}
}

func (e *promptBudgetEstimator) approximateInvocationTokens(invocation *blades.Invocation) int {
	if e == nil || e.encoder == nil || invocation == nil {
		return 0
	}

	var buf strings.Builder
	appendMessageToBudgetText(&buf, invocation.Instruction)
	for _, msg := range invocation.History {
		appendMessageToBudgetText(&buf, msg)
	}
	appendMessageToBudgetText(&buf, invocation.Message)

	return len(e.encoder.Encode(buf.String(), nil, nil))
}

func appendMessageToBudgetText(buf *strings.Builder, msg *blades.Message) {
	if buf == nil || msg == nil {
		return
	}
	buf.WriteString("role=")
	buf.WriteString(string(msg.Role))
	buf.WriteByte('\n')
	for _, part := range msg.Parts {
		switch v := part.(type) {
		case blades.TextPart:
			buf.WriteString(v.Text)
		case blades.FilePart:
			buf.WriteString(v.Name)
			buf.WriteByte(' ')
			buf.WriteString(string(v.MIMEType))
			buf.WriteByte(' ')
			buf.WriteString(v.URI)
		case blades.DataPart:
			buf.WriteString(v.Name)
			buf.WriteByte(' ')
			buf.WriteString(string(v.MIMEType))
			buf.WriteString(" bytes=")
			buf.WriteString(fmt.Sprintf("%d", len(v.Bytes)))
		case blades.ToolPart:
			buf.WriteString("tool ")
			buf.WriteString(v.Name)
			buf.WriteString(" request ")
			buf.WriteString(v.Request)
			buf.WriteString(" response ")
			buf.WriteString(v.Response)
		}
		buf.WriteByte('\n')
	}
}

func logContextBudget(invocation *blades.Invocation, tokenCount int, limit int, ratio float64) {
	if limit <= 0 || invocation == nil || ratio < contextBudgetWarnRatio {
		return
	}

	event := log.Info()
	if ratio >= contextBudgetCriticalRatio {
		event = log.Warn()
	}

	sessionID := ""
	if invocation.Session != nil {
		sessionID = invocation.Session.ID()
	}

	event.
		Str("session_id", sessionID).
		Int("prompt_tokens", tokenCount).
		Int("prompt_limit", limit).
		Float64("prompt_ratio", ratio).
		Msg("context.budget")
}

func contextBudgetNote(ratio float64) string {
	switch {
	case ratio >= contextBudgetCriticalRatio:
		return strings.TrimSpace(`
<context_budget>
Context budget is very tight.
Avoid unnecessary retrieval and use tape_handoff proactively before continuing long or multi-step work.
Keep working context compact.
</context_budget>
`)
	case ratio >= contextBudgetWarnRatio:
		return strings.TrimSpace(`
<context_budget>
Context budget is getting tight.
Prefer concise reasoning, avoid unnecessary retrieval, and use tape_handoff before long or multi-step work when a compact checkpoint would help.
</context_budget>
`)
	default:
		return ""
	}
}

func TapeContextMiddleware(tapes *TapeStore) blades.Middleware {
	return func(next blades.Handler) blades.Handler {
		return blades.HandleFunc(func(ctx context.Context, invocation *blades.Invocation) blades.Generator[*blades.Message, error] {
			if invocation == nil || invocation.Session == nil || tapes == nil {
				return next.Handle(ctx, invocation)
			}
			sessionID := invocation.Session.ID()
			if err := tapes.EnsureBootstrapAnchor(sessionID); err != nil {
				log.Error().
					Str("session_id", sessionID).
					Err(err).
					Msg("tape.bootstrap.ensure.failed")
				return next.Handle(ctx, invocation)
			}
			history, err := tapes.HistoryMessages(sessionID)
			if err != nil {
				if !errors.Is(err, ErrTapeAnchorNotFound) {
					log.Error().
						Str("session_id", sessionID).
						Err(err).
						Msg("tape.context.load.failed")
				}
				return next.Handle(ctx, invocation)
			}
			if invocation.Message != nil {
				filtered := make([]*blades.Message, 0, len(history))
				for _, m := range history {
					if m == nil || m.ID == invocation.Message.ID {
						continue
					}
					filtered = append(filtered, m)
				}
				history = filtered
			}
			history = tailMessages(history, tapeContextMessageLimit)
			if len(history) == 0 {
				return next.Handle(ctx, invocation)
			}
			cloned := invocation.Clone()
			cloned.History = history
			return next.Handle(ctx, cloned)
		})
	}
}

func tailMessages(history []*blades.Message, limit int) []*blades.Message {
	if limit <= 0 || len(history) <= limit {
		return history
	}
	return history[len(history)-limit:]
}

// patchToolSchemas patches tool input schemas for gateways that reject
// object schemas with empty properties.
func PatchToolSchemas() blades.Middleware {
	return func(next blades.Handler) blades.Handler {
		return blades.HandleFunc(func(ctx context.Context, invocation *blades.Invocation) blades.Generator[*blades.Message, error] {
			if invocation == nil || len(invocation.Tools) == 0 {
				return next.Handle(ctx, invocation)
			}
			for i, tool := range invocation.Tools {
				if tool == nil {
					continue
				}
				if _, ok := tool.(*patchedSchemaTool); ok {
					continue
				}
				invocation.Tools[i] = &patchedSchemaTool{Tool: tool}
			}
			return next.Handle(ctx, invocation)
		})
	}
}

type patchedSchemaTool struct {
	bladestools.Tool
}

func (t *patchedSchemaTool) InputSchema() *jsonschema.Schema {
	schema := t.Tool.InputSchema()
	if schema == nil {
		return nil
	}
	cloned := schema.CloneSchemas()
	patchEmptyObjectProperties(cloned, map[*jsonschema.Schema]struct{}{})
	return cloned
}

func patchEmptyObjectProperties(schema *jsonschema.Schema, visited map[*jsonschema.Schema]struct{}) {
	if schema == nil {
		return
	}
	if _, ok := visited[schema]; ok {
		return
	}
	visited[schema] = struct{}{}

	if schemaIsObject(schema) && len(schema.Properties) == 0 {
		schema.Properties = map[string]*jsonschema.Schema{
			"request_id": {
				Type:        "string",
				Description: "Optional request id. Ignored by this tool.",
			},
		}
	}

	for _, sub := range schema.Defs {
		patchEmptyObjectProperties(sub, visited)
	}
	for _, sub := range schema.Definitions {
		patchEmptyObjectProperties(sub, visited)
	}
	for _, sub := range schema.PrefixItems {
		patchEmptyObjectProperties(sub, visited)
	}
	patchEmptyObjectProperties(schema.Items, visited)
	patchEmptyObjectProperties(schema.AdditionalItems, visited)
	patchEmptyObjectProperties(schema.Contains, visited)
	patchEmptyObjectProperties(schema.UnevaluatedItems, visited)
	for _, sub := range schema.Properties {
		patchEmptyObjectProperties(sub, visited)
	}
	for _, sub := range schema.PatternProperties {
		patchEmptyObjectProperties(sub, visited)
	}
	patchEmptyObjectProperties(schema.AdditionalProperties, visited)
	patchEmptyObjectProperties(schema.PropertyNames, visited)
	patchEmptyObjectProperties(schema.UnevaluatedProperties, visited)
	for _, sub := range schema.AllOf {
		patchEmptyObjectProperties(sub, visited)
	}
	for _, sub := range schema.AnyOf {
		patchEmptyObjectProperties(sub, visited)
	}
	for _, sub := range schema.OneOf {
		patchEmptyObjectProperties(sub, visited)
	}
	patchEmptyObjectProperties(schema.Not, visited)
	patchEmptyObjectProperties(schema.If, visited)
	patchEmptyObjectProperties(schema.Then, visited)
	patchEmptyObjectProperties(schema.Else, visited)
	for _, sub := range schema.DependentSchemas {
		patchEmptyObjectProperties(sub, visited)
	}
	patchEmptyObjectProperties(schema.ContentSchema, visited)
}

func schemaIsObject(schema *jsonschema.Schema) bool {
	if schema == nil {
		return false
	}
	if strings.EqualFold(schema.Type, "object") {
		return true
	}
	for _, t := range schema.Types {
		if strings.EqualFold(t, "object") {
			return true
		}
	}
	return false
}
