package modelparts

import (
	"strings"

	"github.com/go-kratos/blades"
)

var _ blades.Part = (*ReasoningPart)(nil)

type ReasoningPart struct {
	blades.TextPart `json:"-"`

	ReasoningText string `json:"reasoning_text"`
}

type TelegramTextRenderState struct{}

func EscapeTelegramMarkdown(text string) string {
	if text == "" {
		return ""
	}

	var out strings.Builder
	appendEscapedText(&out, text)
	return out.String()
}

func FormatTelegramVisibleMessage(parts []blades.Part) string {
	if len(parts) == 0 {
		return ""
	}

	var out strings.Builder
	for _, part := range parts {
		v, ok := part.(blades.TextPart)
		if !ok {
			continue
		}
		appendEscapedText(&out, v.Text)
	}
	return strings.TrimSpace(out.String())
}

func FormatTelegramVisibleDelta(parts []blades.Part, _ *TelegramTextRenderState) string {
	if len(parts) == 0 {
		return ""
	}

	var out strings.Builder
	for _, part := range parts {
		v, ok := part.(blades.TextPart)
		if !ok {
			continue
		}
		appendEscapedText(&out, v.Text)
	}
	return out.String()
}

func appendEscapedText(out *strings.Builder, chunk string) {
	if out == nil || chunk == "" {
		return
	}
	for _, r := range chunk {
		writeEscapedMarkdownRune(out, r)
	}
}

func writeEscapedMarkdownRune(out *strings.Builder, r rune) {
	if out == nil {
		return
	}
	if r == '\n' {
		out.WriteRune(r)
		return
	}
	switch r {
	case '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!':
		out.WriteRune('\\')
	}
	out.WriteRune(r)
}
