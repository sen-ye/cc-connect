package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestProviderSettingsFilePersistsThroughTOML(t *testing.T) {
	const source = `[[providers]]
name = "overlay"
settings_file = "/private/settings.overlay.json"
agent_types = ["claudecode"]

[[projects]]
name = "chat"
[projects.agent]
type = "claudecode"
provider_refs = ["overlay"]
`
	var cfg Config
	if _, err := toml.Decode(source, &cfg); err != nil {
		t.Fatal(err)
	}
	var encoded strings.Builder
	if err := toml.NewEncoder(&encoded).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	var reloaded Config
	if _, err := toml.Decode(encoded.String(), &reloaded); err != nil {
		t.Fatal(err)
	}
	reloaded.ResolveProviderRefs()
	if got := reloaded.Projects[0].Agent.Providers[0].SettingsFile; got != "/private/settings.overlay.json" {
		t.Fatalf("restored provider settings = %q", got)
	}
	cloned := cloneAgentConfig(reloaded.Projects[0].Agent)
	if cloned.Providers[0].SettingsFile != "/private/settings.overlay.json" {
		t.Fatal("cloning a project lost its provider settings")
	}
}
