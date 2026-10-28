package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/chenhg5/cc-connect/config"
)

func TestCodexProviderRetryCap_ProjectAndGlobalPersistence(t *testing.T) {
	for _, seconds := range []int{0, 32, -1, 3601} {
		want := config.ProviderConfig{Name: "limited", Codex: &config.CodexProviderConfig{AutoRetryMaxDelaySeconds: seconds, RateLimitBackoffMaxSeconds: seconds}}
		runtime := configProviderToCore(want)
		if runtime.CodexAutoRetryMaxDelaySeconds != seconds || runtime.CodexRateLimitBackoffMaxSeconds != seconds {
			t.Fatal("provider runtime lost its retry cap")
		}
		stored := coreProviderToConfig(runtime)
		if seconds != 0 && !reflect.DeepEqual(stored, want) {
			t.Fatal("provider with only a retry cap was lost during persistence")
		}
		global := configProviderToGlobal(want)
		raw, err := json.Marshal(global)
		if err != nil || json.Unmarshal(raw, &global) != nil {
			t.Fatal("management JSON could not preserve provider settings")
		}
		if got := globalProviderToConfig(global); !reflect.DeepEqual(got, want) {
			t.Fatal("global provider round-trip lost the retry cap")
		}
	}
}
