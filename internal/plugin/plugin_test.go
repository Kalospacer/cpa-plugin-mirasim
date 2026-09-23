package plugin

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestBuildDeclaresProviderCapabilities(t *testing.T) {
	built := Build(nil)
	if built.Metadata.Name != "Mirasim Provider" || built.Metadata.GitHubRepository == "" {
		t.Fatalf("metadata = %#v", built.Metadata)
	}
	caps := built.Capabilities
	if caps.AuthProvider == nil || caps.ModelProvider == nil || caps.Executor == nil || caps.ThinkingApplier == nil || caps.CommandLinePlugin == nil || caps.QuotaProvider == nil {
		t.Fatalf("capabilities are incomplete: %#v", caps)
	}
	// The plugin owns no HTTP route surface: claiming the Management API is what
	// put routes under the unauthenticated static-asset prefix the store rejects.
	if caps.ManagementAPI != nil {
		t.Fatalf("plugin still claims the Management API: %#v", caps.ManagementAPI)
	}
	if caps.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Fatalf("executor scope = %q", caps.ExecutorModelScope)
	}
	if len(caps.ExecutorInputFormats) != 5 || len(caps.ExecutorOutputFormats) != 5 {
		t.Fatalf("formats = %#v / %#v", caps.ExecutorInputFormats, caps.ExecutorOutputFormats)
	}
	for _, field := range built.Metadata.ConfigFields {
		if field.Name == "credential-dir" {
			t.Fatal("OAuth-only plugin still exposes credential-dir")
		}
	}
}
