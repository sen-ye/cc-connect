package codex

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/chenhg5/cc-connect/core"
)

func validateProviderListenAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || (host != "localhost" && !net.ParseIP(host).IsLoopback()) {
		return fmt.Errorf("codex: provider proxy listen address must be loopback with a port")
	}
	return nil
}

// ServeProviderProxy exposes the same request defaults and transparent streaming
// used by bridge sessions to native CLI configurations. It never resolves or
// injects credentials: each caller supplies its own auth and affinity headers.
func ServeProviderProxy(ctx context.Context, address string, provider core.ProviderConfig, ready func(string) error) error {
	if err := validateProviderListenAddress(address); err != nil {
		return err
	}
	if provider.APIKey != "" || provider.CodexEnvKey != "" || len(provider.CodexHTTPHeaders) != 0 || len(provider.CodexEnvHTTPHeaders) != 0 || len(provider.Env) != 0 {
		return fmt.Errorf("codex: standalone proxy uses caller credentials; remove launcher auth and environment settings")
	}
	p, url, err := newProviderProxyWithOptions(provider.BaseURL, provider.CodexProxyURL, providerProxyOptions{
		ProviderName: provider.Name, ModelCatalogURL: provider.CodexModelCatalogURL,
		MaxOutputTokens: provider.CodexMaxOutputTokens, ReasoningReplayFallback: provider.CodexReasoningReplayFallback,
		MirrorDeveloperInstructions: provider.MirrorsCodexDeveloperInstructions(), ListenAddress: address,
	})
	if err != nil {
		return err
	}
	defer p.Close()
	if ready != nil {
		if err := ready(url); err != nil {
			return fmt.Errorf("codex: report provider proxy address: %w", err)
		}
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-p.done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
