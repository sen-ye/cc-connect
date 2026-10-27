package codex

import (
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

const providerAPIKeyEnv = "CC_CONNECT_CODEX_API_KEY"

// The caller holds a.mu while selecting and copying the provider.
func (a *Agent) sessionProviderLocked(name *string) (*core.ProviderConfig, error) {
	idx := a.activeIdx
	if name != nil {
		idx = -1
		if *name != "" {
			for i, p := range a.providers {
				if p.Name == *name {
					idx = i
					break
				}
			}
			if idx == -1 {
				return nil, fmt.Errorf("codex: session provider %q is not configured", *name)
			}
		}
	}
	if idx < 0 || idx >= len(a.providers) {
		return nil, nil
	}
	p := cloneProvider(a.providers[idx])
	return &p, nil
}

type processProviderConfig struct {
	baseURL   string
	args, env []string
	proxy     *providerProxy
}

func prepareProviderRuntime(provider *core.ProviderConfig, extraEnv []string) (processProviderConfig, error) {
	cfg := processProviderConfig{env: extraEnv}
	if provider == nil {
		return cfg, nil
	}
	cfg.baseURL = provider.BaseURL
	if provider.CodexProxyURL != "" || provider.CodexReasoningReplayFallback || provider.CodexMaxOutputTokens != 0 {
		var err error
		cfg.proxy, cfg.baseURL, err = newProviderProxyWithOptions(provider.BaseURL, provider.CodexProxyURL, providerProxyOptions{
			ModelCatalogURL: provider.CodexModelCatalogURL, ReasoningReplayFallback: provider.CodexReasoningReplayFallback,
			MaxOutputTokens: provider.CodexMaxOutputTokens, ProviderName: provider.Name,
		})
		if err != nil {
			return cfg, err
		}
		provider.BaseURL = cfg.baseURL
		if provider.CodexModelCatalogURL != "" {
			provider.CodexModelCatalogURL = cfg.baseURL + providerCatalogPath
		}
		cfg.env = append(cfg.env, providerProxyBypassEnv(cfg.env)...)
	}
	args, env := providerRuntimeConfig(*provider)
	cfg.args = args
	cfg.env = append(cfg.env, env...)
	return cfg, nil
}

// providerRuntimeConfig supplies a complete, process-local provider table.
// Keys and literal headers travel through the child's environment, never argv
// or shared config/auth files. A table override also supports aliases with
// dots, spaces, or Unicode without interpreting them as dotted TOML paths.
func providerRuntimeConfig(provider core.ProviderConfig) (args, env []string) {
	fields := []string{"name=" + tomlString(provider.Name), "requires_openai_auth=false"}
	wireAPI := provider.CodexWireAPI
	if wireAPI == "" {
		wireAPI = "responses"
	}
	fields = append(fields, "wire_api="+tomlString(wireAPI))
	if provider.BaseURL != "" {
		fields = append(fields, "base_url="+tomlString(provider.BaseURL))
	}
	if provider.CodexModelCatalogURL != "" {
		fields = append(fields, "model_catalog_url="+tomlString(provider.CodexModelCatalogURL))
	}
	envKey := provider.CodexEnvKey
	if provider.APIKey != "" {
		envKey = providerAPIKeyEnv
		env = append(env, providerAPIKeyEnv+"="+provider.APIKey, "OPENAI_API_KEY="+provider.APIKey)
	} else if envKey == "" && provider.Env["OPENAI_API_KEY"] != "" {
		envKey = "OPENAI_API_KEY"
	}
	if envKey != "" {
		fields = append(fields, "env_key="+tomlString(envKey))
	}

	headers := make(map[string]string, len(provider.CodexHTTPHeaders)+len(provider.CodexEnvHTTPHeaders))
	keys := make([]string, 0, len(provider.CodexHTTPHeaders))
	for key := range provider.CodexHTTPHeaders {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for i, key := range keys {
		name := fmt.Sprintf("CC_CONNECT_CODEX_HEADER_%d", i)
		headers[key] = name
		env = append(env, name+"="+provider.CodexHTTPHeaders[key])
	}
	for key, name := range provider.CodexEnvHTTPHeaders {
		headers[key] = name
	}
	if len(headers) > 0 {
		keys = keys[:0]
		for key := range headers {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		values := make([]string, 0, len(keys))
		for _, key := range keys {
			values = append(values, tomlString(key)+"="+tomlString(headers[key]))
		}
		fields = append(fields, "env_http_headers={"+strings.Join(values, ",")+"}")
	}
	value := "model_providers={" + tomlString(provider.Name) + "={" + strings.Join(fields, ",") + "}}"
	return []string{"-c", value}, env
}

func tomlString(value string) string {
	quoted, _ := json.Marshal(value)
	return string(quoted)
}

func cloneProvider(provider core.ProviderConfig) core.ProviderConfig {
	provider.Env = maps.Clone(provider.Env)
	provider.CodexHTTPHeaders = maps.Clone(provider.CodexHTTPHeaders)
	provider.CodexEnvHTTPHeaders = maps.Clone(provider.CodexEnvHTTPHeaders)
	provider.Models = append([]core.ModelOption(nil), provider.Models...)
	return provider
}
