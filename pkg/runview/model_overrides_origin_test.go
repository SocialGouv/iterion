package runview

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A refusal names the record an operator edits: a policy cell by the
// selector its level wrote — the bot part the fold strips included — and
// that level, never the bare target no record holds. Here the integration
// writes `*/work {provider: zai}` and the team `review-pr-pool/work
// {backend: claw}`; the composed pair is refused for the fork
// review-pr-pool, and each half names its own cell and level.
func TestComposedPairRefusalNamesTheWrittenCellAndItsLevel(t *testing.T) {
	resolved, sources := llmroute.Resolve(
		llmroute.Layer{Source: llmroute.SourceWebhook, Policy: llmroute.Policy{Overrides: []llmroute.Override{{Selector: "*/work", Provider: "zai"}}}},
		llmroute.Layer{Source: llmroute.SourceTeam, Policy: llmroute.Policy{Overrides: []llmroute.Override{{Selector: "review-pr-pool/work", Backend: "claw"}}}},
	)
	pol := &store.RunLLMRoutePolicy{Sources: sources}
	for _, o := range resolved.Overrides {
		pol.Overrides = append(pol.Overrides, store.RunLLMRouteOverride{Selector: o.Selector, Backend: o.Backend, Model: o.Model, Provider: o.Provider, Effort: o.Effort})
	}
	_, err := foldAndScreen(mustCompileFold(t, splitPairBot), &LaunchSpec{BotID: "review-pr-pool", BotManifestName: "review-pr", LLMRoutePolicy: pol}, "")
	if err == nil {
		t.Fatal("precondition: the composed (claw, zai) pair is refused")
	}
	for _, want := range []string{
		`the routing policy's override cell "*/work" written at level "webhook"`,
		`the routing policy's override cell "review-pr-pool/work" written at level "team"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v\nwant it to name %s", err, want)
		}
	}
	if strings.Contains(err.Error(), `override cell "work"`) {
		t.Errorf("refusal = %v, names the bare target \"work\", which no record holds", err)
	}
	// The integration's level carries the provenance label "webhook", which
	// the refusal names beside the record an operator edits; a level whose
	// label is its record's name carries no gloss.
	if !strings.Contains(err.Error(), `written at level "webhook" (the integration's routing block)`) {
		t.Errorf("refusal = %v\nwant the integration's level named as its routing block", err)
	}
	if strings.Contains(err.Error(), `written at level "team" (`) {
		t.Errorf("refusal = %v\nglosses the team's level, whose label names its record", err)
	}
}

// A launch row is named as the launch wrote it, beside a cell named by its
// record.
func TestComposedPairRefusalNamesALaunchRowAsWritten(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{{Selector: "review-pr-pool/work", Backend: "claw"}},
		Sources:   map[string]string{llmroute.OverrideCellKey("review-pr-pool/work", llmroute.OverrideFieldBackend): llmroute.SourceOrg},
	}
	_, err := foldAndScreen(mustCompileFold(t, splitPairBot), &LaunchSpec{
		BotID: "review-pr-pool", LLMRoutePolicy: pol,
		ModelOverrides: []ModelOverrideEntry{{Selector: "work", Provider: "zai"}},
	}, "")
	if err == nil {
		t.Fatal("precondition: the composed (claw, zai) pair is refused")
	}
	for _, want := range []string{
		`the launch's own override row "work"`,
		`the routing policy's override cell "review-pr-pool/work" written at level "org"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v\nwant it to name %s", err, want)
		}
	}
}
