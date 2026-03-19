package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/go-kratos/blades"
	"github.com/go-kratos/blades/middleware"
	"github.com/go-kratos/blades/skills"
	"github.com/go-kratos/blades/tools"
	"github.com/go-kratos/kit/retry"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/openai/openai-go/v3"
	log "github.com/yuchanns/bugo/internal/logging"
)

func AgentRetryMiddleware() blades.Middleware {
	return middleware.Retry(
		5,
		retry.WithBaseDelay(300*time.Millisecond),
		retry.WithMaxDelay(2*time.Second),
		retry.WithRetryable(isRetryableAgentError),
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

func SkillToolLoggingMiddleware() blades.Middleware {
	return func(next blades.Handler) blades.Handler {
		return blades.HandleFunc(func(ctx context.Context, invocation *blades.Invocation) blades.Generator[*blades.Message, error] {
			if invocation == nil || len(invocation.Tools) == 0 {
				return next.Handle(ctx, invocation)
			}
			for i, tool := range invocation.Tools {
				if tool == nil {
					continue
				}
				if _, ok := tool.(*skillLoggedTool); ok {
					continue
				}
				if !isSkillToolName(tool.Name()) {
					continue
				}
				invocation.Tools[i] = &skillLoggedTool{Tool: tool}
			}
			return next.Handle(ctx, invocation)
		})
	}
}

func isSkillToolName(name string) bool {
	switch name {
	case skills.ToolListSkillsName,
		skills.ToolLoadSkillName,
		skills.ToolLoadSkillResourceName,
		skills.ToolRunSkillScriptName:
		return true
	default:
		return false
	}
}

type skillLoggedTool struct {
	tools.Tool
}

func (t *skillLoggedTool) Handle(ctx context.Context, input string) (string, error) {
	logSkillToolStart(ctx, t.Name(), input)
	start := time.Now()
	output, err := t.Tool.Handle(ctx, input)
	elapsed := time.Since(start)
	if err != nil {
		log.Error().
			Str("name", t.Name()).
			Float64("elapsed_ms", float64(elapsed.Microseconds())/1000.0).
			Err(err).
			Msg("skill.tool.error")
		return output, err
	}
	logSkillToolSuccess(ctx, t.Name(), input, output, elapsed)
	return output, nil
}

func logSkillToolStart(ctx context.Context, name string, input string) {
	event := log.Info().
		Str("name", name).
		Str("params", log.PrettifyText(input))
	appendSessionID(event, ctx)
	appendSkillRunFields(event, name, input, "")
	event.Msg("skill.tool.start")
}

func logSkillToolSuccess(ctx context.Context, name string, input string, output string, elapsed time.Duration) {
	event := log.Info().
		Str("name", name).
		Float64("elapsed_ms", float64(elapsed.Microseconds())/1000.0)
	appendSessionID(event, ctx)
	appendSkillRunFields(event, name, input, output)
	event.Msg("skill.tool.success")
}

func appendSessionID(event *log.Event, ctx context.Context) {
	if event == nil {
		return
	}
	if session, ok := blades.FromSessionContext(ctx); ok && session != nil {
		event.Str("session_id", session.ID())
	}
}

func appendSkillRunFields(event *log.Event, name string, input string, output string) {
	if event == nil || name != skills.ToolRunSkillScriptName {
		return
	}
	var req struct {
		SkillName      string   `json:"skill_name"`
		ScriptPath     string   `json:"script_path"`
		Args           []string `json:"args"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}
	if err := json.Unmarshal([]byte(input), &req); err == nil {
		event.Str("skill_name", req.SkillName)
		event.Str("script_path", req.ScriptPath)
		if len(req.Args) > 0 {
			event.Str("args", log.RenderValue(req.Args))
		}
		if req.TimeoutSeconds > 0 {
			event.Int("timeout_seconds", req.TimeoutSeconds)
		}
	}

	var resp struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
	}
	if output != "" && json.Unmarshal([]byte(output), &resp) == nil {
		if strings.TrimSpace(resp.Status) != "" {
			event.Str("status", resp.Status)
		}
		event.Int("exit_code", resp.ExitCode)
	}
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
	tools.Tool
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
