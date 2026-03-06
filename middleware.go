package main

import (
	"context"
	"log"

	"github.com/go-kratos/blades"
	bladestools "github.com/go-kratos/blades/tools"
	"github.com/google/jsonschema-go/jsonschema"
)

func tapeContextMiddleware(tapes *TapeStore, maxTokens int) blades.Middleware {
	return func(next blades.Handler) blades.Handler {
		return blades.HandleFunc(func(ctx context.Context, invocation *blades.Invocation) blades.Generator[*blades.Message, error] {
			if invocation == nil || invocation.Session == nil || tapes == nil {
				return next.Handle(ctx, invocation)
			}
			history, err := tapes.HistoryMessages(invocation.Session.ID(), maxTokens)
			if err != nil {
				log.Printf("load tape context failed session=%s err=%v", invocation.Session.ID(), err)
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
			cloned := invocation.Clone()
			cloned.History = history
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
