package main

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/go-kratos/blades"
	bladestools "github.com/go-kratos/blades/tools"
	"github.com/google/jsonschema-go/jsonschema"
)

func tapeContextMiddleware(tapes *TapeStore) blades.Middleware {
	return func(next blades.Handler) blades.Handler {
		return blades.HandleFunc(func(ctx context.Context, invocation *blades.Invocation) blades.Generator[*blades.Message, error] {
			if invocation == nil || invocation.Session == nil || tapes == nil {
				return next.Handle(ctx, invocation)
			}
			sessionID := invocation.Session.ID()
			if err := tapes.EnsureBootstrapAnchor(sessionID); err != nil {
				log.Printf("ensure tape bootstrap anchor failed session=%s err=%v", sessionID, err)
				return next.Handle(ctx, invocation)
			}
			history, err := tapes.HistoryMessages(sessionID)
			if err != nil {
				if !errors.Is(err, errTapeAnchorNotFound) {
					log.Printf("load tape context failed session=%s err=%v", sessionID, err)
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
			cloned := invocation.Clone()
			cloned.History = history
			return next.Handle(ctx, cloned)
		})
	}
}

// patchToolSchemas patches tool input schemas for gateways that reject
// object schemas with empty properties.
func patchToolSchemas() blades.Middleware {
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
