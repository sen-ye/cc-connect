package codex

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// providerProxy fixes the upstream network proxy for one Codex session. Codex
// talks to loopback, so CLI wrappers that clear HTTP_PROXY cannot override the
// selection. Only model API traffic is proxied; tool subprocesses are unchanged.
type providerProxy struct {
	server    *http.Server
	transport *http.Transport
	once      sync.Once
}

const providerCatalogPath = "/_cc_connect_models"

type providerProxyOptions struct {
	ModelCatalogURL         string
	ReasoningReplayFallback bool
}

func newProviderProxy(baseURL, proxyURL string, catalogURLs ...string) (*providerProxy, string, error) {
	options := providerProxyOptions{}
	if len(catalogURLs) > 0 {
		options.ModelCatalogURL = catalogURLs[0]
	}
	return newProviderProxyWithOptions(baseURL, proxyURL, options)
}

func newProviderProxyWithOptions(baseURL, proxyURL string, options providerProxyOptions) (*providerProxy, string, error) {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	target, err := parseProviderProxyURL(baseURL)
	if err != nil {
		return nil, "", fmt.Errorf("codex: provider base_url must be an absolute HTTP(S) URL")
	}
	var upstreamProxy *url.URL
	if proxyURL != "" || !options.ReasoningReplayFallback {
		upstreamProxy, err = parseProviderProxyURL(proxyURL)
		if err != nil {
			return nil, "", fmt.Errorf("codex: provider proxy_url must be an absolute HTTP(S) URL")
		}
	}
	var catalog *url.URL
	if options.ModelCatalogURL != "" {
		catalog, err = parseProviderProxyURL(options.ModelCatalogURL)
		if err != nil {
			return nil, "", fmt.Errorf("codex: model_catalog_url must be an absolute HTTP(S) URL")
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("codex: listen for provider proxy: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A replay-only proxy is explicitly direct. Do not inherit the bridge's
	// corporate HTTP_PROXY when reaching an internal model gateway.
	transport.Proxy = nil
	if upstreamProxy != nil {
		transport.Proxy = http.ProxyURL(upstreamProxy)
	}
	handler := providerProxyHandler(target, catalog, transport, options.ReasoningReplayFallback)
	p := &providerProxy{transport: transport, server: &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}}
	go func() {
		if err := p.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("codex: provider proxy stopped", "error", err)
		}
	}()
	return p, "http://" + listener.Addr().String(), nil
}

func providerProxyHandler(target, catalog *url.URL, transport *http.Transport, replayFallback bool) http.Handler {
	reverse := httputil.NewSingleHostReverseProxy(target)
	originalDirector := reverse.Director
	reverse.Director = func(r *http.Request) { originalDirector(r); r.Host = target.Host }
	reverse.Transport = transport
	if replayFallback {
		reverse.Transport = &reasoningReplayTransport{base: transport}
	}
	reverse.FlushInterval = -1
	reverse.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Network errors may embed a proxy URL with credentials. Keep URLs
		// and credentials out of both the client response and runtime log.
		slog.Warn("codex: provider proxy request failed", "target_host", target.Host, "error_type", fmt.Sprintf("%T", err))
		http.Error(w, "provider proxy request failed", http.StatusBadGateway)
	}
	var handler http.Handler = reverse
	if catalog != nil {
		catalogProxy := httputil.NewSingleHostReverseProxy(catalog)
		catalogProxy.Transport = transport
		catalogProxy.ErrorHandler = reverse.ErrorHandler
		catalogProxy.Director = func(r *http.Request) {
			query := r.URL.RawQuery
			target := *catalog
			r.URL = &target
			r.Host = target.Host
			if query != "" {
				if r.URL.RawQuery != "" {
					r.URL.RawQuery += "&"
				}
				r.URL.RawQuery += query
			}
		}
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == providerCatalogPath {
				catalogProxy.ServeHTTP(w, r)
			} else {
				reverse.ServeHTTP(w, r)
			}
		})
	}
	return handler
}

func parseProviderProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) {
		return nil, fmt.Errorf("invalid HTTP(S) URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	return u, nil
}

func providerProxyBypassEnv(extraEnv []string) []string {
	values := map[string]string{"NO_PROXY": os.Getenv("NO_PROXY"), "no_proxy": os.Getenv("no_proxy")}
	for _, entry := range extraEnv {
		key, value, ok := strings.Cut(entry, "=")
		if ok && (key == "NO_PROXY" || key == "no_proxy") {
			values[key] = value
		}
	}
	bypass := strings.Join([]string{values["NO_PROXY"], values["no_proxy"], "127.0.0.1", "localhost", "::1"}, ",")
	return []string{"NO_PROXY=" + bypass, "no_proxy=" + bypass}
}

func (p *providerProxy) Close() {
	if p == nil {
		return
	}
	p.once.Do(func() { _ = p.server.Close(); p.transport.CloseIdleConnections() })
}
