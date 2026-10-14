package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

func skipIfNoConfig(t *testing.T) *config.Config {
	t.Helper()
	if os.Getenv("CC_SKIP_INTEGRATION") == "1" {
		t.Skip("CC_SKIP_INTEGRATION=1")
	}
	if os.Getenv("CC_RUN_PROVIDER_INTEGRATION") != "1" {
		t.Skip("set CC_RUN_PROVIDER_INTEGRATION=1 to run provider integration tests")
	}
	cfgPath := os.ExpandEnv("$HOME/.cc-connect/config.toml")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		t.Skipf("config not found at %s", cfgPath)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	cfg.ResolveProviderRefs()
	return cfg
}

func configToCoreProv(p config.ProviderConfig) core.ProviderConfig {
	cp := core.ProviderConfig{
		Name:    p.Name,
		APIKey:  p.APIKey,
		BaseURL: p.BaseURL,
		Model:   p.Model,
		Env:     p.Env,
	}
	if p.Codex != nil {
		cp.CodexWireAPI = p.Codex.WireAPI
		cp.CodexHTTPHeaders = p.Codex.HTTPHeaders
		cp.CodexEnvKey = p.Codex.EnvKey
		cp.CodexEnvHTTPHeaders = p.Codex.EnvHTTPHeaders
		cp.CodexProxyURL = p.Codex.ProxyURL
		cp.CodexModelCatalogURL = p.Codex.ModelCatalogURL
	}
	for _, m := range p.Models {
		cp.Models = append(cp.Models, core.ModelOption{Name: m.Model})
	}
	return cp
}

func envSliceToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		out[key] = value
	}
	return out
}

func findCodexProject(cfg *config.Config) (name string, providers []core.ProviderConfig, workDir, codexHome string) {
	for i := range cfg.Projects {
		proj := &cfg.Projects[i]
		if proj.Agent.Type != "codex" || len(proj.Agent.Providers) == 0 {
			continue
		}
		for _, p := range proj.Agent.Providers {
			providers = append(providers, configToCoreProv(p))
		}
		name = proj.Name
		if wd, ok := proj.Agent.Options["work_dir"].(string); ok {
			workDir = wd
		}
		if ch, ok := proj.Agent.Options["codex_home"].(string); ok {
			codexHome = ch
		}
		return
	}
	return
}

func TestIntegration_Codex_ProviderSwitch_EnvVars(t *testing.T) {
	cfg := skipIfNoConfig(t)

	name, providers, _, _ := findCodexProject(cfg)
	if name == "" {
		t.Skip("no codex project with providers found")
	}
	if len(providers) < 2 {
		t.Skipf("project %q has only %d provider(s), need at least 2", name, len(providers))
	}

	a := &Agent{
		providers: providers,
		activeIdx: -1,
	}

	p0, p1 := providers[0], providers[1]
	t.Logf("provider[0]: name=%s model=%s base_url=%s", p0.Name, p0.Model, p0.BaseURL)
	t.Logf("provider[1]: name=%s model=%s base_url=%s", p1.Name, p1.Model, p1.BaseURL)

	a.SetActiveProvider(p0.Name)
	a.mu.RLock()
	env0 := envSliceToMap(a.providerEnvLocked())
	a.mu.RUnlock()

	if p0.BaseURL != "" {
		if got := env0["OPENAI_BASE_URL"]; got != p0.BaseURL {
			t.Errorf("provider[0] OPENAI_BASE_URL = %q, want %q", got, p0.BaseURL)
		}
	}
	if p0.APIKey != "" {
		if got := env0["OPENAI_API_KEY"]; got == "" {
			t.Error("provider[0] OPENAI_API_KEY not set")
		}
	}

	a.SetActiveProvider(p1.Name)
	a.mu.RLock()
	env1 := envSliceToMap(a.providerEnvLocked())
	a.mu.RUnlock()

	if p1.BaseURL != "" {
		if got := env1["OPENAI_BASE_URL"]; got != p1.BaseURL {
			t.Errorf("provider[1] OPENAI_BASE_URL = %q, want %q", got, p1.BaseURL)
		}
	}
	if p1.APIKey != "" {
		if got := env1["OPENAI_API_KEY"]; got == "" {
			t.Error("provider[1] OPENAI_API_KEY not set")
		}
	}

	if p0.BaseURL != p1.BaseURL {
		if env0["OPENAI_BASE_URL"] == env1["OPENAI_BASE_URL"] {
			t.Error("providers have different base_urls but OPENAI_BASE_URL didn't change after switch")
		}
	}
}

func TestIntegration_Codex_ProviderSwitch_SessionArgs(t *testing.T) {
	cfg := skipIfNoConfig(t)

	name, providers, workDir, codexHome := findCodexProject(cfg)
	if name == "" {
		t.Skip("no codex project with providers found")
	}
	if workDir == "" {
		workDir = t.TempDir()
	}

	for _, prov := range providers {
		t.Run(prov.Name, func(t *testing.T) {
			a := &Agent{
				model:     "default-model",
				providers: providers,
				activeIdx: -1,
				workDir:   workDir,
				codexHome: codexHome,
				mode:      "suggest",
				backend:   "exec",
			}
			a.SetActiveProvider(prov.Name)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			sess, err := a.StartSession(ctx, "")
			if err != nil {
				t.Fatalf("StartSession failed: %v", err)
			}
			defer sess.Close()

			cs := sess.(*codexSession)

			if prov.Model != "" && cs.model != prov.Model {
				t.Errorf("session model = %q, want %q", cs.model, prov.Model)
			}
			if prov.BaseURL != "" && cs.baseURL != prov.BaseURL {
				t.Errorf("session baseURL = %q, want %q", cs.baseURL, prov.BaseURL)
			}

			envMap := envSliceToMap(cs.extraEnv)
			if prov.APIKey != "" {
				if got := envMap["OPENAI_API_KEY"]; got == "" {
					t.Error("OPENAI_API_KEY not set in session extraEnv")
				}
			}
			if prov.BaseURL != "" {
				if got := envMap["OPENAI_BASE_URL"]; got != prov.BaseURL {
					t.Errorf("OPENAI_BASE_URL = %q, want %q", got, prov.BaseURL)
				}
			}

			t.Logf("session OK: model=%s baseURL=%s modelProvider=%s",
				cs.model, cs.baseURL, cs.modelProvider)
		})
	}
}

func TestIntegration_Codex_ProviderConfig_ProcessOverrides(t *testing.T) {
	cfg := skipIfNoConfig(t)
	name, providers, _, _ := findCodexProject(cfg)
	if name == "" {
		t.Skip("no codex project with providers found")
	}
	for _, provider := range providers {
		t.Run(provider.Name, func(t *testing.T) {
			a := &Agent{backend: "exec", codexHome: t.TempDir(), activeIdx: -1}
			a.SetProviders([]core.ProviderConfig{provider})
			session, err := a.StartSessionWithProvider(context.Background(), "", provider.Name)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			cs := session.(*codexSession)
			if cs.modelProvider != provider.Name {
				t.Fatal("wrong selected provider")
			}
			if provider.APIKey != "" && envSliceToMap(cs.extraEnv)[providerAPIKeyEnv] != provider.APIKey {
				t.Fatal("missing process-local credential")
			}
			if _, err := os.Stat(filepath.Join(a.codexHome, "auth.json")); !os.IsNotExist(err) {
				t.Fatal("unexpected shared auth write")
			}
		})
	}
}

func TestIntegration_Codex_ProviderSwitch_SendMessage(t *testing.T) {
	cfg := skipIfNoConfig(t)
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex CLI not in PATH")
	}

	name, providers, workDir, codexHome := findCodexProject(cfg)
	if name == "" {
		t.Skip("no codex project with providers found")
	}
	if codexHome == "" {
		home, _ := os.UserHomeDir()
		codexHome = filepath.Join(home, ".codex")
	}

	if workDir == "" {
		workDir = t.TempDir()
		gitInit := exec.Command("git", "init", "-q")
		gitInit.Dir = workDir
		if err := gitInit.Run(); err != nil {
			t.Fatalf("git init: %v", err)
		}
	}

	for _, prov := range providers {
		t.Run(prov.Name, func(t *testing.T) {
			if prov.APIKey == "" || prov.BaseURL == "" {
				t.Skip("provider missing api_key or base_url")
			}

			a := &Agent{
				model:     "default-model",
				providers: providers,
				activeIdx: -1,
				workDir:   workDir,
				codexHome: codexHome,
				mode:      "full-auto",
				backend:   "exec",
			}
			a.SetActiveProvider(prov.Name)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := a.StartSession(ctx, "")
			if err != nil {
				t.Fatalf("StartSession failed: %v", err)
			}
			defer sess.Close()

			err = sess.Send("reply with exactly 'codex-provider-ok' and nothing else", "", nil, nil)
			if err != nil {
				t.Fatalf("Send failed: %v", err)
			}

			timer := time.NewTimer(25 * time.Second)
			defer timer.Stop()

			var allText strings.Builder
			for {
				select {
				case ev, ok := <-sess.Events():
					if !ok {
						if allText.Len() > 0 && strings.Contains(allText.String(), "codex-provider-ok") {
							t.Logf("provider %s: send+receive OK (from text events)", prov.Name)
							return
						}
						t.Fatalf("event channel closed, collected text: %s", allText.String())
						return
					}
					t.Logf("event: type=%s content_len=%d", ev.Type, len(ev.Content))
					if ev.Type == core.EventText || ev.Type == core.EventResult {
						allText.WriteString(ev.Content)
					}
					if ev.Type == core.EventResult {
						if strings.Contains(allText.String(), "codex-provider-ok") {
							t.Logf("provider %s: send+receive OK", prov.Name)
						} else {
							t.Errorf("expected 'codex-provider-ok' in output, got: %s", allText.String())
						}
						return
					}
					if ev.Type == core.EventError {
						t.Fatalf("agent error: %s", ev.Content)
					}
				case <-timer.C:
					if strings.Contains(allText.String(), "codex-provider-ok") {
						t.Logf("provider %s: send+receive OK (timeout but got result)", prov.Name)
						return
					}
					t.Fatalf("timeout waiting for codex response, collected: %s", allText.String())
				}
			}
		})
	}
}
