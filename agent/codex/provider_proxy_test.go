package codex

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProviderProxy_FixedRoutesKeepKeysAndBodiesSeparate(t *testing.T) {
	// Explicit provider routing must not be bypassed by inherited NO_PROXY.
	t.Setenv("NO_PROXY", "*")
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b"} {
		name := name
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if r.URL.Host != "api-"+name+".invalid" || r.URL.Path != "/v1/responses" || r.URL.Query().Get("version") != "1" {
				t.Errorf("wrong route for %s: %s", name, r.URL)
			}
			if r.Header.Get("Authorization") != "Bearer test-key-"+name || string(body) != "unchanged-encrypted-input-"+name {
				t.Errorf("proxy %s received another account or changed payload", name)
			}
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, "rate-limit-"+name)
		}))
		t.Cleanup(upstream.Close)
		proxy, local, err := newProviderProxy("http://api-"+name+".invalid/v1?version=1", upstream.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(proxy.Close)
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, local+"/responses", strings.NewReader("unchanged-encrypted-input-"+name))
			req.Header.Set("Authorization", "Bearer test-key-"+name)
			client := &http.Client{Transport: &http.Transport{}, Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "2" || string(body) != "rate-limit-"+name {
				t.Error("proxy changed error response or retry headers")
			}
		}()
	}
	wg.Wait()
}

func TestProviderProxy_StreamsAndCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(3 * time.Second):
		}
	}))
	defer upstream.Close()
	proxy, local, err := newProviderProxy("http://api.invalid/v1", upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	client := &http.Client{Transport: &http.Transport{}, Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get(local + "/responses")
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "data: first\n" {
		t.Fatalf("stream buffered or changed: %q", line)
	}
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream continued after client cancellation")
	}
}

func TestProviderProxy_InvalidURLDoesNotExposeCredentials(t *testing.T) {
	for _, value := range []string{"socks5://user:secret-password@localhost:1234", "not a URL", "://secret-password"} {
		_, _, err := newProviderProxy("http://api.invalid/v1", value)
		if err == nil || strings.Contains(err.Error(), "secret-password") {
			t.Fatal("invalid proxy accepted or credential exposed")
		}
	}
}

func TestProviderProxy_ModelCatalogUsesSameProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "catalog.invalid" || r.URL.Path != "/v1/models" || r.URL.Query().Get("source") != "configured" || r.URL.Query().Get("client_version") != "test" {
			t.Errorf("wrong catalog URL: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer catalog-test-key" {
			t.Error("catalog lost authentication")
		}
		io.WriteString(w, `{"models":[]}`)
	}))
	defer upstream.Close()
	p, local, err := newProviderProxy("http://api.invalid/v1", upstream.URL, "http://catalog.invalid/v1/models?source=configured")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	req, _ := http.NewRequest("GET", local+providerCatalogPath+"?client_version=test", nil)
	req.Header.Set("Authorization", "Bearer catalog-test-key")
	client := &http.Client{Transport: &http.Transport{}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("catalog status %d", resp.StatusCode)
	}
}
