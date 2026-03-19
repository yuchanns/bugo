package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-kratos/blades"
	"github.com/pkoukk/tiktoken-go"
	"github.com/yuchanns/bugo/internal/modelparts"
)

const (
	autoHandoffBatchSize = 20

	autoHandoffInstruction = "Please provide a concise summary of the following conversation transcript. " +
		"Preserve key facts, decisions, and outcomes. Output only the summary."
)

type AutoHandoffConfig struct {
	Model      string
	MaxTokens  int
	Summarizer blades.ModelProvider
	Tapes      *TapeStore
}

type AutoHandoffContextManager struct {
	maxTokens  int
	summarizer blades.ModelProvider
	tapes      *TapeStore
	encoder    *tiktoken.Tiktoken
}

func NewAutoHandoffContextManager(cfg AutoHandoffConfig) (*AutoHandoffContextManager, error) {
	if cfg.MaxTokens <= 0 {
		return nil, fmt.Errorf("auto handoff token limit must be positive")
	}
	if cfg.Summarizer == nil {
		return nil, fmt.Errorf("auto handoff summarizer is required")
	}
	if cfg.Tapes == nil {
		return nil, fmt.Errorf("auto handoff tape store is required")
	}

	encoder, err := tiktoken.EncodingForModel(strings.TrimSpace(cfg.Model))
	if err != nil {
		encoder, err = tiktoken.GetEncoding("cl100k_base")
	}
	if err != nil {
		return nil, fmt.Errorf("auto handoff encoder: %w", err)
	}

	return &AutoHandoffContextManager{
		maxTokens:  cfg.MaxTokens,
		summarizer: cfg.Summarizer,
		tapes:      cfg.Tapes,
		encoder:    encoder,
	}, nil
}

func (m *AutoHandoffContextManager) Prepare(ctx context.Context, messages []*blades.Message) ([]*blades.Message, error) {
	if m == nil || m.maxTokens <= 0 {
		return messages, nil
	}
	session, ok := blades.FromSessionContext(ctx)
	if !ok || session == nil {
		return messages, nil
	}

	history := session.History()
	if len(history) == 0 {
		return messages, nil
	}
	if m.countTokens(history) <= m.maxTokens {
		return cloneMessages(history), nil
	}

	splitAt, ok := findActiveSuffixStart(history, messages)
	if !ok || splitAt <= 0 {
		return cloneMessages(history), nil
	}

	archive := cloneMessages(history[:splitAt])
	active := cloneMessages(history[splitAt:])

	summary, err := m.summarize(ctx, archive)
	if err != nil {
		return nil, err
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return nil, fmt.Errorf("auto handoff summary is empty")
	}

	if err := m.tapes.AppendHandoff(session.ID(), HandoffPayload{
		Name:    "handoff",
		Summary: summary,
	}); err != nil {
		return nil, err
	}
	for _, msg := range active {
		if err := session.Append(ctx, msg); err != nil {
			return nil, err
		}
	}

	return active, nil
}

func (m *AutoHandoffContextManager) summarize(ctx context.Context, history []*blades.Message) (string, error) {
	summary := ""
	for start := 0; start < len(history); start += autoHandoffBatchSize {
		end := min(start+autoHandoffBatchSize, len(history))
		nextSummary, err := m.extendSummary(ctx, summary, history[start:end])
		if err != nil {
			return "", err
		}
		summary = strings.TrimSpace(nextSummary)
	}
	return summary, nil
}

func (m *AutoHandoffContextManager) extendSummary(ctx context.Context, existing string, batch []*blades.Message) (string, error) {
	instruction := autoHandoffInstruction
	if existing != "" {
		instruction += "\n\nExisting summary:\n" + existing
	}
	resp, err := m.summarizer.Generate(ctx, &blades.ModelRequest{
		Messages:    batch,
		Instruction: blades.SystemMessage(instruction),
	})
	if err != nil {
		return "", err
	}
	if resp == nil || resp.Message == nil {
		return "", fmt.Errorf("auto handoff summarizer returned empty response")
	}
	return resp.Message.Text(), nil
}

func (m *AutoHandoffContextManager) countTokens(messages []*blades.Message) int {
	if m == nil || m.encoder == nil || len(messages) == 0 {
		return 0
	}
	var buf strings.Builder
	for _, msg := range messages {
		appendMessageTokenText(&buf, msg)
	}
	return len(m.encoder.Encode(buf.String(), nil, nil))
}

func appendMessageTokenText(buf *strings.Builder, msg *blades.Message) {
	if buf == nil || msg == nil {
		return
	}
	buf.WriteString("role=")
	buf.WriteString(string(msg.Role))
	buf.WriteByte('\n')
	for _, part := range msg.Parts {
		switch v := part.(type) {
		case modelparts.ReasoningPart:
			continue
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

func findActiveSuffixStart(history []*blades.Message, active []*blades.Message) (int, bool) {
	if len(active) == 0 || len(active) > len(history) {
		return 0, false
	}
	start := len(history) - len(active)
	for i := range active {
		if !sameMessage(history[start+i], active[i]) {
			return 0, false
		}
	}
	return start, true
}

func sameMessage(a, b *blades.Message) bool {
	if a == nil || b == nil {
		return a == b
	}
	if strings.TrimSpace(a.ID) != "" || strings.TrimSpace(b.ID) != "" {
		return a.ID == b.ID
	}
	return a.Role == b.Role && a.Text() == b.Text()
}

func cloneMessages(messages []*blades.Message) []*blades.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]*blades.Message, 0, len(messages))
	for _, msg := range messages {
		out = append(out, msg.Clone())
	}
	return out
}
