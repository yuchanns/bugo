package wireapi

import (
	"fmt"
	"strings"
)

const (
	Chat      = "chat"
	Responses = "responses"
)

func Normalize(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", Chat, "chat-completion", "chat-completions", "chat_completion", "chat_completions":
		return Chat, nil
	case Responses, "response":
		return Responses, nil
	default:
		return "", fmt.Errorf("BUGO_WIRE_API must be %q or %q, got %q", Chat, Responses, raw)
	}
}
