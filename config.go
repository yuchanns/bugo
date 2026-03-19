package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultModelRef      = "openai:gpt-4o-mini"
	defaultMaxIterations = int(^uint(0) >> 1)
)

type Config struct {
	TelegramToken      string
	TelegramProxy      string
	TelegramWorkers    int
	AllowChats         map[int64]struct{}
	AllowFrom          map[string]struct{}
	DebounceSeconds    int
	MessageDelay       int
	ActiveWindow       int
	Provider           string
	Model              string
	ModelContextWindow int
	ModelMaxIterations int
	MaxOutputTokens    int
	APIKey             string
	APIBase            string
	CodexAuthFile      string
	WorkDir            string
	HomeDir            string
}

func LoadConfig() (Config, error) {
	modelRef := firstNonEmpty(env("BUGO_MODEL"), defaultModelRef)
	provider, model, err := parseModelRef(modelRef)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		TelegramToken:      env("BUGO_TELEGRAM_TOKEN"),
		TelegramProxy:      env("BUGO_TELEGRAM_PROXY"),
		TelegramWorkers:    intEnv("BUGO_TELEGRAM_WORKERS", 4),
		DebounceSeconds:    intEnv("BUGO_DEBOUNCE_SECONDS", 1),
		MessageDelay:       intEnv("BUGO_MESSAGE_DELAY_SECONDS", 10),
		ActiveWindow:       intEnv("BUGO_ACTIVE_WINDOW_SECONDS", 60),
		Provider:           provider,
		Model:              model,
		ModelContextWindow: intEnv("BUGO_MODEL_CONTEXT_WINDOW", 128000),
		ModelMaxIterations: intEnv("BUGO_MAX_ITERATIONS", defaultMaxIterations),
		MaxOutputTokens:    intEnv("BUGO_MAX_OUTPUT_TOKENS", 1024),
		WorkDir:            env("BUGO_WORKDIR"),
		HomeDir:            resolveHomeDir(firstNonEmpty(env("BUGO_HOME"), "~/.bugo")),
	}
	cfg.APIKey = env("BUGO_API_KEY")
	cfg.APIBase = env("BUGO_API_BASE")
	cfg.CodexAuthFile = resolveHomeDir(firstNonEmpty(
		env("BUGO_CODEX_AUTH_FILE"),
		filepath.Join(cfg.HomeDir, "providers", "openai-codex-auth.json"),
	))

	if cfg.TelegramToken == "" {
		return Config{}, fmt.Errorf("missing telegram token, set BUGO_TELEGRAM_TOKEN")
	}
	if cfg.Provider == "openai" && cfg.APIKey == "" {
		return Config{}, fmt.Errorf("missing model api key, set BUGO_API_KEY")
	}
	if cfg.Provider != "openai" && cfg.Provider != "codex" {
		return Config{}, fmt.Errorf("unsupported provider %q", cfg.Provider)
	}
	if cfg.ModelContextWindow <= 0 {
		return Config{}, fmt.Errorf("missing model context window, set BUGO_MODEL_CONTEXT_WINDOW to a positive integer")
	}

	allowChats, err := parseInt64Set(env("BUGO_TELEGRAM_ALLOW_CHATS"))
	if err != nil {
		return Config{}, fmt.Errorf("parse allow chats: %w", err)
	}
	cfg.AllowChats = allowChats

	cfg.AllowFrom = parseStringSet(env("BUGO_TELEGRAM_ALLOW_FROM"))
	return cfg, nil
}

func parseModelRef(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("BUGO_MODEL is required")
	}
	provider, model, ok := strings.Cut(raw, ":")
	if !ok {
		return "openai", raw, nil
	}
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return "", "", fmt.Errorf("BUGO_MODEL must be provider:model, got %q", raw)
	}
	return provider, model, nil
}

func env(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

func resolveHomeDir(raw string) string {
	if raw == "" {
		return "."
	}
	if strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(raw, "~/"))
		}
	}
	return raw
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func intEnv(key string, defaultValue int) int {
	raw := env(key)
	if raw == "" {
		return defaultValue
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return defaultValue
	}
	return v
}

func parseStringSet(raw string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, v := range splitAnyList(raw) {
		x := strings.TrimSpace(v)
		if x == "" {
			continue
		}
		out[strings.ToLower(x)] = struct{}{}
	}
	return out
}

func parseInt64Set(raw string) (map[int64]struct{}, error) {
	out := map[int64]struct{}{}
	for _, item := range splitAnyList(raw) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		id, err := strconv.ParseInt(item, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid int64 value %q", item)
		}
		out[id] = struct{}{}
	}
	return out, nil
}

func splitAnyList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var jsonList []string
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		if err := json.Unmarshal([]byte(raw), &jsonList); err == nil {
			return jsonList
		}
	}
	return strings.Split(raw, ",")
}
