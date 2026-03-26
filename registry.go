package main

import (
	"fmt"

	"github.com/go-kratos/blades"
	"github.com/yuchanns/bugo/contrib/codexauth"
	bugoopenai "github.com/yuchanns/bugo/contrib/openai"
)

type providerSpec struct {
	validate func(Config) error
	build    func(*App) (blades.ModelProvider, error)
}

var providerRegistry = map[string]providerSpec{
	"openai": {
		validate: validateOpenAIConfig,
		build:    (*App).buildOpenAIProvider,
	},
	"codex": {
		validate: validateCodexConfig,
		build:    (*App).buildCodexProvider,
	},
}

func lookupProviderSpec(name string) (providerSpec, error) {
	spec, ok := providerRegistry[name]
	if !ok {
		return providerSpec{}, fmt.Errorf("unsupported provider %q", name)
	}
	return spec, nil
}

func validateProviderConfig(cfg Config) error {
	spec, err := lookupProviderSpec(cfg.Provider)
	if err != nil {
		return err
	}
	if spec.validate == nil {
		return nil
	}
	return spec.validate(cfg)
}

func validateOpenAIConfig(cfg Config) error {
	if cfg.APIKey == "" {
		return fmt.Errorf("missing model api key, set BUGO_API_KEY")
	}
	return nil
}

func validateCodexConfig(Config) error {
	return nil
}

func (a *App) buildOpenAIProvider() (blades.ModelProvider, error) {
	a.codex = nil
	provider, err := bugoopenai.New(bugoopenai.Config{
		Model:           a.cfg.Model,
		BaseURL:         a.cfg.APIBase,
		APIKey:          a.cfg.APIKey,
		MaxOutputTokens: int64(a.cfg.MaxOutputTokens),
		HTTPClient:      newOpenAIHTTPClient(),
		WireAPI:         a.cfg.WireAPI,
	})
	if err != nil {
		return nil, err
	}

	a.sessionChainState = provider
	return provider, nil
}

func (a *App) buildCodexProvider() (blades.ModelProvider, error) {
	if a.codex != nil && a.codex.Name() == a.cfg.Model {
		a.sessionChainState = a.codex
		return a.codex, nil
	}

	provider, err := codexauth.New(codexauth.Config{
		Model:           a.cfg.Model,
		AuthFile:        a.cfg.CodexAuthFile,
		MaxOutputTokens: int64(a.cfg.MaxOutputTokens),
		HTTPClient:      newOpenAIHTTPClient(),
	})
	if err != nil {
		return nil, err
	}

	a.codex = provider
	a.sessionChainState = provider
	return provider, nil
}
