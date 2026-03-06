package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultModel = "gpt-4o-mini"
)

type Config struct {
	TelegramToken      string
	TelegramProxy      string
	TelegramWorkers    int
	AllowChats         map[int64]struct{}
	AllowFrom          map[string]struct{}
	ProactiveResponse  bool
	DebounceSeconds    int
	MessageDelay       int
	ActiveWindow       int
	Model              string
	ModelMaxIterations int
	ModelTimeout       time.Duration
	MaxOutputTokens    int
	APIKey             string
	APIBase            string
	HomeDir            string
	ExtraSkillsDir     string
	HistoryMaxTokens   int
}

func LoadConfig() (Config, error) {
	cfg := Config{
		TelegramToken:      env("BUGO_TELEGRAM_TOKEN"),
		TelegramProxy:      env("BUGO_TELEGRAM_PROXY"),
		TelegramWorkers:    intEnv("BUGO_TELEGRAM_WORKERS", 4),
		ProactiveResponse:  boolEnvAny([]string{"BUGO_PROACTIVE_RESPONSE"}, false),
		DebounceSeconds:    intEnv("BUGO_DEBOUNCE_SECONDS", 1),
		MessageDelay:       intEnv("BUGO_MESSAGE_DELAY_SECONDS", 10),
		ActiveWindow:       intEnv("BUGO_ACTIVE_WINDOW_SECONDS", 60),
		Model:              firstNonEmpty(env("BUGO_MODEL"), defaultModel),
		ModelMaxIterations: intEnv("BUGO_MAX_ITERATIONS", 20),
		ModelTimeout:       time.Duration(intEnv("BUGO_MODEL_TIMEOUT_SECONDS", 90)) * time.Second,
		MaxOutputTokens:    intEnv("BUGO_MAX_OUTPUT_TOKENS", 1024),
		HomeDir:            resolveHomeDir(firstNonEmpty(env("BUGO_HOME"), "~/.bugo")),
		ExtraSkillsDir:     env("BUGO_EXTRA_SKILLS_DIR"),
		HistoryMaxTokens:   intEnv("BUGO_HISTORY_MAX_TOKENS", 24000),
	}

	cfg.APIKey = firstNonEmpty(env("BUGO_API_KEY"), env("OPENROUTER_API_KEY"), env("OPENAI_API_KEY"))
	cfg.APIBase = env("BUGO_API_BASE")
	if cfg.APIBase == "" && env("OPENROUTER_API_KEY") != "" {
		cfg.APIBase = "https://openrouter.ai/api/v1"
	}

	if cfg.TelegramToken == "" {
		return Config{}, fmt.Errorf("missing telegram token, set BUGO_TELEGRAM_TOKEN")
	}
	if cfg.APIKey == "" {
		return Config{}, fmt.Errorf("missing model api key, set BUGO_API_KEY (or OPENROUTER_API_KEY/OPENAI_API_KEY)")
	}

	allowChats, err := parseInt64Set(env("BUGO_TELEGRAM_ALLOW_CHATS"))
	if err != nil {
		return Config{}, fmt.Errorf("parse allow chats: %w", err)
	}
	cfg.AllowChats = allowChats

	cfg.AllowFrom = parseStringSet(env("BUGO_TELEGRAM_ALLOW_FROM"))
	return cfg, nil
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

func boolEnvAny(keys []string, defaultValue bool) bool {
	for _, key := range keys {
		raw := env(key)
		if raw == "" {
			continue
		}
		v, err := strconv.ParseBool(raw)
		if err == nil {
			return v
		}
	}
	return defaultValue
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
