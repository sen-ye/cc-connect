package codex

import (
	"context"
	"os"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestStartSessionWithModel_ExplicitOverrideDoesNotMutateProvider(t *testing.T) {
	a := &Agent{backend: "exec", model: "global-default", activeIdx: -1}
	a.SetProviders([]core.ProviderConfig{{Name: "OpenAI", Model: "gpt-6-astra"}, {Name: "other", Model: "other-model"}})
	for _, tc := range []struct{ provider, override, want string }{
		{"OpenAI", "gpt-6.1-sol", "gpt-6.1-sol"},
		{"other", "", "other-model"},
		{"OpenAI", "", "gpt-6-astra"},
		{"", "gpt-6.1-sol", "gpt-6.1-sol"},
	} {
		s, err := a.StartSessionWithModel(context.Background(), "original-native", tc.provider, tc.override)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.(*codexSession).model; got != tc.want {
			t.Errorf("model=%q, want %q", got, tc.want)
		}
		_ = s.Close()
	}
	if a.GetModel() != "global-default" || a.ListProviders()[0].Model != "gpt-6-astra" {
		t.Fatal("changed defaults shared by other sessions")
	}
}

func TestAgent_DefaultSessionServiceTierConfiguration(t *testing.T) {
	for _, tc := range []struct{ backend, configured, want string }{
		{"app_server", "", "default"}, {"app_server", "default", "default"},
		{"app_server", "fast", "fast"}, {"exec", "", ""},
	} {
		agent, err := New(map[string]any{"cmd": os.Args[0], "backend": tc.backend, "default_service_tier": tc.configured})
		if err != nil {
			t.Fatal(err)
		}
		a := agent.(*Agent)
		if got := a.DefaultSessionServiceTier(); got != tc.want {
			t.Fatalf("default=%q, want %q", got, tc.want)
		}
		cloned, err := New(a.WorkspaceAgentOptionSnapshot())
		if err != nil {
			t.Fatal(err)
		}
		if cloned.(*Agent).DefaultSessionServiceTier() != tc.want {
			t.Fatal("workspace clone lost default tier")
		}
	}
	if _, err := New(map[string]any{"cmd": os.Args[0], "default_service_tier": "invalid"}); err == nil {
		t.Fatal("accepted invalid default tier")
	}
}
