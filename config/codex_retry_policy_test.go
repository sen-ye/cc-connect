package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestCodexProviderRetryCap_TOMLRoundTripAndInheritance(t *testing.T) {
	for _, seconds := range []int{0, 32, -1, 3601} {
		var output strings.Builder
		if err := toml.NewEncoder(&output).Encode(CodexProviderConfig{AutoRetryMaxDelaySeconds: seconds, RateLimitBackoffMaxSeconds: seconds}); err != nil {
			t.Fatal(err)
		}
		var decoded CodexProviderConfig
		if _, err := toml.Decode(output.String(), &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.AutoRetryMaxDelaySeconds != seconds || decoded.RateLimitBackoffMaxSeconds != seconds {
			t.Fatal("provider retry cap changed during TOML round-trip")
		}
	}
}
