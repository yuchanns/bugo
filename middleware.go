package main

import (
	"context"
	"slices"

	"github.com/go-kratos/blades"
	bladestools "github.com/go-kratos/blades/tools"
	"github.com/google/jsonschema-go/jsonschema"
)

func historyMiddleware(limit int) blades.Middleware {
	return func(next blades.Handler) blades.Handler {
		return blades.HandleFunc(func(ctx context.Context, invocation *blades.Invocation) blades.Generator[*blades.Message, error] {
			if invocation == nil || invocation.Session == nil {
				return next.Handle(ctx, invocation)
			}

			history := invocation.Session.History()
			// Runner appends current user message into session before invocation.
			// Exclude that message from injected history to avoid duplication.
			if invocation.Message != nil {
				filtered := history[:0]
				for _, m := range history {
					if m == nil || m.ID == invocation.Message.ID {
						continue
					}
					filtered = append(filtered, m)
				}
				history = filtered
			}

			if limit > 0 && len(history) > limit {
				history = history[len(history)-limit:]
			}

			cloned := invocation.Clone()
			cloned.History = slices.Clone(history)
			return next.Handle(ctx, cloned)
		})
	}
}

// WithPatchedListSkill patches list_skills input schema for gateways that reject empty object properties.
func WithPatchedListSkill() blades.Middleware {
	return func(next blades.Handler) blades.Handler {
		return blades.HandleFunc(func(ctx context.Context, invocation *blades.Invocation) blades.Generator[*blades.Message, error] {
			if invocation == nil || len(invocation.Tools) == 0 {
				return next.Handle(ctx, invocation)
			}
			for i, tool := range invocation.Tools {
				if tool == nil || tool.Name() != "list_skills" {
					continue
				}
				if _, ok := tool.(*patchedListSkillTool); ok {
					continue
				}
				invocation.Tools[i] = &patchedListSkillTool{Tool: tool}
			}
			return next.Handle(ctx, invocation)
		})
	}
}

type patchedListSkillTool struct {
	bladestools.Tool
}

func (t *patchedListSkillTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"request_id": {
				Type:        "string",
				Description: "Optional request id. Ignored by this tool.",
			},
		},
	}
}
