package main

import (
	"testing"

	"github.com/chenhg5/cc-connect/config"
)

func TestProviderSettingsFileSurvivesConfigAndManagementConversions(t *testing.T) {
	provider := config.ProviderConfig{Name: "overlay", SettingsFile: "/private/settings.overlay.json"}
	if got := configProviderToCore(provider); got.SettingsFile != provider.SettingsFile {
		t.Fatal("agent wiring lost the provider settings file")
	}
	global := configProviderToGlobal(provider)
	if global.SettingsFile != provider.SettingsFile {
		t.Fatal("management read lost the provider settings file")
	}
	if got := globalProviderToConfig(global); got.SettingsFile != provider.SettingsFile {
		t.Fatal("management save lost the provider settings file")
	}
}
