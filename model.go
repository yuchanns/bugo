package main

import (
	"context"

	"github.com/go-kratos/blades"
	log "github.com/yuchanns/bugo/internal/logging"
)

type model struct {
	base    blades.ModelProvider
	inboxes *inboxHub
}

func newModel(base blades.ModelProvider, inboxes *inboxHub) blades.ModelProvider {
	if base == nil {
		return nil
	}
	return &model{
		base:    base,
		inboxes: inboxes,
	}
}

func (m *model) Name() string {
	return m.base.Name()
}

func (m *model) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	enriched := m.enrichRequest(ctx, req)
	return m.base.Generate(ctx, enriched)
}

func (m *model) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	enriched := m.enrichRequest(ctx, req)
	return m.base.NewStreaming(ctx, enriched)
}

func (m *model) enrichRequest(ctx context.Context, req *blades.ModelRequest) *blades.ModelRequest {
	if m == nil || req == nil {
		return req
	}
	session, ok := blades.FromSessionContext(ctx)
	if !ok || session == nil || m.inboxes == nil {
		return req
	}
	inbox := m.inboxes.Find(session.ID())
	if inbox == nil {
		return req
	}

	prompts, segmentVersion := inbox.consumeInterrupts()
	if len(prompts) == 0 {
		return req
	}

	cloned := *req
	cloned.Messages = append([]*blades.Message(nil), req.Messages...)
	for _, prompt := range prompts {
		msg := blades.UserMessage(prompt)
		cloned.Messages = append(cloned.Messages, msg)
		if err := session.Append(ctx, msg); err != nil {
			log.Error().
				Str("session_id", session.ID()).
				Err(err).
				Msg("session.interrupt.append.failed")
		}
	}

	log.Info().
		Str("session_id", session.ID()).
		Int("count", len(prompts)).
		Int("segment_version", segmentVersion).
		Msg("session.interrupt.injected")
	return &cloned
}
