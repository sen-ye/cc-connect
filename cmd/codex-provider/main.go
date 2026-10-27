// codex-provider wraps the native CLI with a process-local provider proxy.
// A small shell alias can select a configuration, e.g. codex-seed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/chenhg5/cc-connect/agent/codex"
	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

type launcherConfig struct {
	Binary           string                `toml:"binary"`
	ModelCatalogJSON string                `toml:"model_catalog_json"`
	Provider         config.ProviderConfig `toml:"provider"`
}

func main() {
	ctx, stop := launcherSignals()
	defer stop()
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("codex-provider", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "launcher TOML file; pass native Codex arguments after --")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, err := loadLauncherConfig(*path)
	if err == nil {
		err = validateNativeArgs(flags.Args(), cfg.Provider)
	}
	if err == nil && restoringSession(flags.Args()) {
		err = validateResumeProvider(flags.Args(), cfg.Provider)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	nativeArgs := flags.Args()
	if cfg.ModelCatalogJSON != "" {
		nativeArgs = append([]string{"-c", "model_catalog_json=" + tomlQuote(cfg.ModelCatalogJSON)}, nativeArgs...)
	}
	provider := nativeProvider(cfg.Provider)
	if restoringSession(flags.Args()) {
		provider.Model = "" // Let native resume restore its saved model.
	}
	err = codex.RunProviderCLI(ctx, cfg.Binary, nativeArgs, provider, stdin, stdout, stderr)
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		return 1
	}
	fmt.Fprintln(stderr, err)
	return 1
}

func loadLauncherConfig(path string) (launcherConfig, error) {
	var cfg launcherConfig
	if path == "" {
		return cfg, fmt.Errorf("codex-provider: --config is required")
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		// Parser diagnostics can include literal credentials from a bad config.
		return cfg, fmt.Errorf("codex-provider: cannot read or parse launcher TOML")
	}
	if cfg.Binary == "" {
		cfg.Binary = "codex"
	}
	if cfg.Provider.Name == "" || cfg.Provider.BaseURL == "" || cfg.Provider.Model == "" {
		return cfg, fmt.Errorf("codex-provider: provider name, base_url and model are required")
	}
	if !allowedModel(cfg.Provider, cfg.Provider.Model) {
		return cfg, fmt.Errorf("codex-provider: default model is missing from provider.models")
	}
	return cfg, nil
}

func nativeProvider(p config.ProviderConfig) core.ProviderConfig {
	out := core.ProviderConfig{Name: p.Name, APIKey: p.APIKey, BaseURL: p.BaseURL, Model: p.Model, Env: p.Env}
	if c := p.Codex; c != nil {
		out.CodexModelCatalogURL, out.CodexProxyURL = c.ModelCatalogURL, c.ProxyURL
		out.CodexEnvKey, out.CodexWireAPI = c.EnvKey, c.WireAPI
		out.CodexHTTPHeaders, out.CodexEnvHTTPHeaders = c.HTTPHeaders, c.EnvHTTPHeaders
		out.CodexReasoningReplayFallback = c.ReasoningReplayFallback
	}
	return out
}

func allowedModel(p config.ProviderConfig, model string) bool {
	for _, option := range p.Models {
		if option.Model == model {
			return true
		}
	}
	return len(p.Models) == 0
}

// Preserve normal CLI options and prompts while disallowing accidental routing
// overrides. A dedicated provider launcher must never silently use another one.
func validateNativeArgs(args []string, provider config.ProviderConfig) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		key, value, inline := strings.Cut(arg, "=")
		if key == "-p" || (strings.HasPrefix(key, "-p") && !strings.HasPrefix(key, "--")) || key == "--profile" || key == "--oss" || key == "--local-provider" || key == "--remote" {
			return fmt.Errorf("codex-provider: routing overrides are not supported; use the ordinary codex command")
		}
		if (strings.HasPrefix(arg, "-m") || strings.HasPrefix(arg, "-c")) && !strings.HasPrefix(arg, "--") && len(arg) > 2 {
			key, value, inline = arg[:2], strings.TrimPrefix(arg[2:], "="), true
		}
		if key != "-m" && key != "--model" && key != "-c" && key != "--config" {
			continue
		}
		if !inline {
			i++
			if i == len(args) {
				return fmt.Errorf("codex-provider: option requires a value")
			}
			value = args[i]
		}
		if key == "-m" || key == "--model" {
			if !allowedModel(provider, value) {
				return fmt.Errorf("codex-provider: model is not in provider.models")
			}
			continue
		}
		setting, _, _ := strings.Cut(value, "=")
		setting = strings.Trim(strings.TrimSpace(setting), "\"'")
		if setting == "model" || setting == "model_provider" || setting == "model_providers" ||
			strings.HasPrefix(setting, "model_providers.") || setting == "model_catalog_json" || setting == "profile" || setting == "profiles" || strings.HasPrefix(setting, "profiles.") {
			return fmt.Errorf("codex-provider: use -m to select a model; routing overrides are not supported")
		}
	}
	return nil
}

func tomlQuote(value string) string {
	quoted, _ := json.Marshal(value)
	return string(quoted)
}

func restoringSession(args []string) bool {
	return restoreCommandIndex(args) >= 0
}

func restoreCommandIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if nativeOptionTakesValue(arg) {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") || arg == "exec" || arg == "e" {
			continue
		}
		if arg == "resume" || arg == "fork" {
			return i
		}
		break // The first non-command positional argument is the prompt.
	}
	return -1
}

func nativeOptionTakesValue(arg string) bool {
	switch arg {
	case "-c", "--config", "-m", "--model", "-C", "--cd", "-i", "--image", "--add-dir",
		"--enable", "--disable", "-s", "--sandbox", "-a", "--ask-for-approval", "--color",
		"-o", "--output-last-message", "--output-schema":
		return true
	}
	return false
}
