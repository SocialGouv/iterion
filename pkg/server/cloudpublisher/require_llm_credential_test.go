package cloudpublisher

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// A run whose every LLM route pins a provider the deployment knows, and for
// which no tier holds a credential, cannot start: under
// Config.RequireLLMCredential the publisher refuses it with the typed
// runview.ErrNoLLMCredential instead of queueing a run that fails at its
// first call (#841). The rule is per route, read through the same walk the
// credential stamp uses: a route the walk cannot attribute is never refused,
// and one funded pinned provider is enough — the other route's failure is
// the run's to report.

func compileTestSource(t *testing.T, src string) *ir.Workflow {
	t.Helper()
	pr := parser.Parse("test.bot", src)
	if pr.File == nil {
		t.Fatalf("parse failed: %+v", pr.Diagnostics)
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatalf("compile failed: %+v", cr.Diagnostics)
	}
	return cr.Workflow
}

const (
	anthropicPinnedBot = "agent draft:\n  model: \"anthropic/claude-opus-5\"\n  backend: \"claw\"\n\nworkflow main:\n  draft -> done\n"
	openaiPinnedBot    = "agent draft:\n  model: \"openai/gpt-5.4-mini\"\n  backend: \"claw\"\n\nworkflow main:\n  draft -> done\n"
	twoRoutesBot       = "agent draft:\n  model: \"anthropic/claude-opus-5\"\n  backend: \"claw\"\n\njudge verify:\n  model: \"openai/gpt-5.4-mini\"\n  backend: \"claw\"\n\nworkflow main:\n  draft -> verify\n  verify -> done\n"
	unattributableBot  = "agent draft:\n  backend: \"claude_code\"\n\nworkflow main:\n  draft -> done\n"
	unknownVocabBot    = "agent draft:\n  model: \"kimi-code/kimi-for-coding\"\n  backend: \"kimi\"\n\nworkflow main:\n  draft -> done\n"
	toolOnlyBot        = "schema out:\n  ok: string\n\ntool noop:\n  command: `printf '{\"ok\":\"yes\"}'`\n  output: out\n\nworkflow main:\n  entry: noop\n  noop -> done\n"
)

func TestResolve_RequireLLMCredentialRefusesOnlyARunThatCannotStart(t *testing.T) {
	cases := []struct {
		name    string
		require bool
		bot     string
		seed    []secrets.Provider
		refused bool
	}{
		{"knob off: nothing funded, nothing refused", false, anthropicPinnedBot, nil, false},
		{"pinned anthropic, nothing funded", true, anthropicPinnedBot, nil, true},
		{"pinned anthropic, anthropic key", true, anthropicPinnedBot, []secrets.Provider{secrets.ProviderAnthropic}, false},
		{"pinned anthropic, only an openai key", true, anthropicPinnedBot, []secrets.Provider{secrets.ProviderOpenAI}, true},
		{"pinned openai, only an anthropic key", true, openaiPinnedBot, []secrets.Provider{secrets.ProviderAnthropic}, true},
		{"two routes, one funded", true, twoRoutesBot, []secrets.Provider{secrets.ProviderAnthropic}, false},
		{"two routes, neither funded", true, twoRoutesBot, nil, true},
		{"unattributable route (no model prefix)", true, unattributableBot, nil, false},
		{"hint outside the vocabulary", true, unknownVocabBot, nil, false},
		{"tool-only workflow", true, toolOnlyBot, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			keys := secrets.NewMemoryApiKeyStore()
			for i, prov := range tc.seed {
				seedKeyFP(t, keys, sealer, "team1", prov, "sk-"+string(prov), "fp-"+string(prov)+string(rune('a'+i)))
			}
			var buf bytes.Buffer
			p := &Publisher{apiKeys: keys, usageCaps: usagecap.NewMemStore(),
				runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
				logger: iterlog.New(iterlog.LevelInfo, &buf), requireLLMCredential: tc.require}
			wf := compileTestSource(t, tc.bot)
			ctx := store.WithTenant(context.Background(), "team1")
			_, err = p.resolveAndSealCredentials(ctx, "run-req", "", "team1", "owner1", "",
				wf, nil, nil, model.ModelOverrides{}, nil)
			if tc.refused {
				if !errors.Is(err, runview.ErrNoLLMCredential) {
					t.Fatalf("resolveAndSealCredentials = %v, want runview.ErrNoLLMCredential", err)
				}
				msg := err.Error()
				if !strings.Contains(msg, "byok") || !strings.Contains(msg, "platform") {
					t.Errorf("the refusal does not name the tiers consulted: %q", msg)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAndSealCredentials = %v, want nil", err)
			}
		})
	}
}

// The refusal names what it could not fund, so the operator reads which
// provider to provision — not just that "something" was missing.
func TestResolve_RequireLLMCredentialNamesTheUnfundedProviders(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	p := &Publisher{apiKeys: secrets.NewMemoryApiKeyStore(), usageCaps: usagecap.NewMemStore(),
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: iterlog.Nop(), requireLLMCredential: true}
	ctx := store.WithTenant(context.Background(), "team1")
	_, err = p.resolveAndSealCredentials(ctx, "run-req", "", "team1", "owner1", "",
		compileTestSource(t, twoRoutesBot), nil, nil, model.ModelOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "anthropic") || !strings.Contains(err.Error(), "openai") {
		t.Fatalf("refusal = %v, want it to name both pinned providers", err)
	}
}
