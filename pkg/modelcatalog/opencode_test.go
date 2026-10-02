package modelcatalog

import (
	"os"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
)

// TestOpenCodeIsOfferedOnlyForCredentialsItCanRead: iterion and opencode do
// not probe the same environment variables, so "the host holds a credential
// for this provider" is not the same question as "opencode can use it".
// Pairing a model with opencode on a credential it cannot read offers the
// studio a call that always fails — the very thing detectOpenCode refuses.
func TestOpenCodeIsOfferedOnlyForCredentialsItCanRead(t *testing.T) {
	report := func(provider, source string) detect.Report {
		return detect.Report{
			Backends:  []detect.BackendStatus{{Name: detect.BackendOpenCode, Available: true}},
			Providers: []detect.ProviderStatus{{Name: provider, Available: true, Source: source}},
		}
	}

	t.Run("a credential opencode cannot read is not offered", func(t *testing.T) {
		// iterion probes ZAI_API_KEY; opencode reads ZHIPU_API_KEY.
		t.Setenv("ZAI_API_KEY", "set")
		backends, _, _ := availability(report("zai", "ZAI_API_KEY"), "anthropic", "glm-4.6", "zai")
		if slices.Contains(backends, detect.BackendOpenCode) {
			t.Fatalf("opencode offered on ZAI_API_KEY, which it cannot read: %v", backends)
		}
	})

	t.Run("a credential opencode does read is offered", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-test")
		backends, _, _ := availability(report("anthropic", "ANTHROPIC_API_KEY"), "anthropic", "claude-sonnet-4-6", "anthropic")
		if !slices.Contains(backends, detect.BackendOpenCode) {
			t.Fatalf("opencode not offered on a credential it reads: %v", backends)
		}
	})

	t.Run("an allowlisted variable that is unset is not offered", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		if _, ok := os.LookupEnv("ANTHROPIC_API_KEY"); !ok {
			t.Skip("cannot unset the variable here")
		}
		backends, _, _ := availability(report("anthropic", "ANTHROPIC_API_KEY"), "anthropic", "claude-sonnet-4-6", "anthropic")
		if slices.Contains(backends, detect.BackendOpenCode) {
			t.Fatalf("opencode offered on an unset variable: %v", backends)
		}
	})
}
