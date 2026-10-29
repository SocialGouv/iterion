package model

import (
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/automemory"
	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// Pin both sides of the migration: backendcaps tests hold the HEAD oracle;
// this test makes an adapter that stops reading that authority observable.
func TestBackendCapabilityConsumersConform(t *testing.T) {
	t.Parallel()
	exec := NewClawExecutor(NewRegistry(), verdictWorkflow())
	delegates := delegate.DefaultRegistry(nil)
	names := append(backendcaps.Names(), "custom", "auto", "CLAUDE_CODE", " claude_code ")
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			checks := []struct {
				capability string
				got        bool
			}{
				{backendcaps.SessionIDResume, sessionResumeEligible(name)},
				{backendcaps.ProviderChain, providerFallbackEligible(name)},
				{backendcaps.MCPForwarding, mcpForwardingBackends[name]},
				{backendcaps.AutoMemory, automemory.SupportsBackend(name)},
				{backendcaps.AutoMemoryPrompt, automemory.NeedsPromptSection(name)},
				{backendcaps.ReasoningEffort, slices.Contains(ir.EffortDialBackends(), name)},
			}
			for _, check := range checks {
				if want := backendcaps.Offers(name, check.capability); check.got != want {
					t.Errorf("%s = %t, registry %t", check.capability, check.got, want)
				}
			}
			if toolcatalog.ConstrainsTools(name) != backendcaps.Offers(strings.TrimSpace(name), backendcaps.ToolResolution) ||
				toolcatalog.ReceivesToolList(name) != backendcaps.Offers(strings.TrimSpace(name), backendcaps.ToolList) {
				t.Error("toolcatalog departs from the registry or its normalization boundary")
			}
			// EffectiveBackendName resolves source syntax before persist lookup;
			// only registered literal names belong in this comparison.
			if slices.Contains(backendcaps.Names(), name) {
				_, persist := exec.SessionResumeCapability(verdictJudge(name))
				if persist != backendcaps.Offers(name, backendcaps.SessionPersist) {
					t.Error("persist eligibility departs from the registry")
				}
				if name != backendcaps.Claw {
					if _, err := delegates.Resolve(name); err != nil {
						t.Errorf("registered backend lacks its execution adapter: %v", err)
					}
				}
			}
		})
	}
	if !slices.Equal(llmroute.Harnesses, backendcaps.Names()) || !slices.Equal(detect.DefaultPreferenceOrder, backendcaps.DefaultPreference()) {
		t.Fatal("routing/detection order departs from the registry")
	}
}
