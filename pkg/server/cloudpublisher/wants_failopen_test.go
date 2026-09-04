package cloudpublisher

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The wants derivation reads `provider:` the way the executor does and
// FAILS OPEN on anything it cannot resolve. Each row below is a shape the
// round-1 A/B probe ran against the real catalog bots, where a verbatim
// read of `provider:` narrowed the pool request from the full order to
// NOTHING and every credential-less run of those bots skipped the pool
// tier in silence.

func wantKeys(wants []credpool.Credential) []string {
	out := make([]string, 0, len(wants))
	for _, w := range wants {
		out = append(out, string(w.Source)+":"+w.Ref)
	}
	return out
}

func hasWant(wants []credpool.Credential, key string) bool {
	for _, k := range wantKeys(wants) {
		if k == key {
			return true
		}
	}
	return false
}

func node(id, backend, provider, mdl string) *ir.AgentNode {
	return &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: id},
		LLMFields: ir.LLMFields{Backend: backend, Provider: provider, Model: mdl},
	}
}

// securedRenovacyShape is bots/secured-renovacy: thirteen claude_code
// nodes pinned `provider: "${RESCUE_PROVIDER:-zai}"` next to nodes that
// pin nothing under `default_backend: claude_code`.
func securedRenovacyShape() *ir.Workflow {
	return &ir.Workflow{
		DefaultBackend: "claude_code",
		Nodes: map[string]ir.Node{
			"detect_stack":  node("detect_stack", "claude_code", "${RESCUE_PROVIDER:-zai}", ""),
			"plan_upgrades": node("plan_upgrades", "claude_code", "${RESCUE_PROVIDER:-zai}", ""),
			"campaign":      node("campaign", "", "", ""),
		},
	}
}

func TestWantsFor_ABRows_FailOpenOnUnresolvedAndNarrowOnResolved(t *testing.T) {
	t.Setenv("RESCUE_PROVIDER", "")
	t.Setenv("ITERION_SEC_AUDIT_BACKEND", "")
	t.Setenv("ITERION_SEC_AUDIT_PROVIDER_CHAIN", "")
	full := len(poolWantOrder)
	anthropicWire := []string{"oauth:claude_code", "api_key:anthropic", "api_key:zai"}

	rows := []struct {
		name      string
		wf        *ir.Workflow
		wantCount int
		wantHas   []string
	}{
		{
			name:      "secured-renovacy: zai-pinned nodes next to unpinned peers → full order",
			wf:        securedRenovacyShape(),
			wantCount: full,
			wantHas:   []string{"oauth:claude_code", "api_key:zai"},
		},
		{
			name: "sec-audit-source triage: ${…_CHAIN:-} expands to an empty chain → full order",
			wf: &ir.Workflow{Nodes: map[string]ir.Node{
				"triage": node("triage", "${ITERION_SEC_AUDIT_BACKEND:-claude_code}", "${ITERION_SEC_AUDIT_PROVIDER_CHAIN:-}", ""),
			}},
			wantCount: full,
			wantHas:   []string{"oauth:claude_code"},
		},
		{
			name:      "chain zai,anthropic → the anthropic wire, exactly",
			wf:        &ir.Workflow{Nodes: map[string]ir.Node{"a": node("a", "claude_code", "zai,anthropic", "")}},
			wantCount: len(anthropicWire),
			wantHas:   anthropicWire,
		},
		{
			name:      "chain zai:glm-5.2,anthropic:claude-opus-4-8 → the anthropic wire, exactly",
			wf:        &ir.Workflow{Nodes: map[string]ir.Node{"a": node("a", "claude_code", "zai:glm-5.2,anthropic:claude-opus-4-8", "")}},
			wantCount: len(anthropicWire),
			wantHas:   anthropicWire,
		},
		{
			name:      "explicit auto → full order",
			wf:        &ir.Workflow{Nodes: map[string]ir.Node{"a": node("a", "claude_code", "auto", "")}},
			wantCount: full,
		},
		{
			name: "MIXED: unpinned claude_code node + openai-pinned peer → full order, both wires kept",
			wf: &ir.Workflow{Nodes: map[string]ir.Node{
				"implement": node("implement", "claude_code", "", ""),
				"review":    node("review", "claw", "openai", "openai/gpt-5.6-sol"),
			}},
			wantCount: full,
			wantHas:   []string{"oauth:claude_code", "api_key:openai"},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got, res := wantsFor(row.wf, model.ModelOverrides{}, nil)
			if len(got) != row.wantCount {
				t.Fatalf("wants = %v (%d), want %d — resolution %+v", wantKeys(got), len(got), row.wantCount, res)
			}
			for _, k := range row.wantHas {
				if !hasWant(got, k) {
					t.Errorf("wants %v lack %s — resolution %+v", wantKeys(got), k, res)
				}
			}
		})
	}
}

// The end-to-end half of the same regression, on the real broker: the
// secured-renovacy shape, a tenant with no credential, one claude_code
// donor. Before the fix the wants were empty, the pool was never asked,
// and the run fell to the platform tier.
func TestPoolTier_securedRenovacyShapeIsServedByADonor(t *testing.T) {
	t.Setenv("RESCUE_PROVIDER", "")
	var buf bytes.Buffer
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	f.pub.logger = iterlog.New(iterlog.LevelInfo, &buf)

	bundle, creds := f.resolve(t, "run-renovacy", securedRenovacyShape())
	if creds.grant == nil {
		t.Fatalf("no grant — the pool was not asked or refused; log:\n%s", buf.String())
	}
	if len(bundle.OAuthCredentials["claude_code"]) == 0 {
		t.Fatal("the donor's claude_code forfait is not in the sealed bundle")
	}
	if log := buf.String(); !strings.Contains(log, "credential pool consulted for run run-renovacy") {
		t.Fatalf("want the consulted line; got:\n%s", log)
	}
}

// G3: a node's `fallbacks:` routes are paths the run may take. A run
// primary on claw/openai whose rescue route is anthropic must be able to
// spend an anthropic credential, or the route is unreachable on a
// pool-funded run.
func TestWantsFor_NodeFallbacksWidenTheWants(t *testing.T) {
	primaryOnly := &ir.Workflow{Nodes: map[string]ir.Node{"a": node("a", "claw", "openai", "openai/gpt-5.6-sol")}}
	got, _ := wantsFor(primaryOnly, model.ModelOverrides{}, nil)
	if hasWant(got, "api_key:anthropic") || hasWant(got, "oauth:claude_code") {
		t.Fatalf("openai-only run must not ask for anthropic: %v", wantKeys(got))
	}

	withRescue := &ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "a"},
		LLMFields: ir.LLMFields{Backend: "claw", Provider: "openai", Model: "openai/gpt-5.6-sol"},
		Fallbacks: []ir.Fallback{{Name: "rescue", Backend: "claude_code", Provider: "anthropic"}},
	}}}
	got, _ = wantsFor(withRescue, model.ModelOverrides{}, nil)
	for _, k := range []string{"api_key:openai", "oauth:codex", "api_key:anthropic", "oauth:claude_code"} {
		if !hasWant(got, k) {
			t.Errorf("wants %v lack %s — the rescue route's provider was dropped", wantKeys(got), k)
		}
	}
	if hasWant(got, "api_key:zai") {
		t.Errorf("wants %v include zai, which no route pins", wantKeys(got))
	}
}

// G3, the run-level half through the REAL launch path: `spec.Fallback`
// (the operator's --fallback) must reach the wants derivation, not only
// the IR the runner applies later. The oracle is the consulted line the
// publisher logs, which names the wants it asked for.
func TestSubmitLaunch_RunLevelFallbackReachesTheWants(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var buf bytes.Buffer
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	f.pub.store = st
	f.pub.logger = iterlog.New(iterlog.LevelInfo, &buf)
	f.pub.publishRun = func(context.Context, *queue.RunMessage) error { return nil }

	ctx := store.WithIdentity(context.Background(), poolTeam, "requester")
	wf := &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{"a": node("a", "claw", "openai", "openai/gpt-5.6-sol")}}
	spec := runview.LaunchSpec{
		FilePath: "wf.bot",
		Source:   "workflow wf:\n  a -> done\n",
		Fallback: []runview.FallbackEntry{{Backend: "claude_code", Provider: "anthropic"}},
	}
	if _, err := f.pub.SubmitLaunch(ctx, "run-fb-1", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	log := buf.String()
	line := ""
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, "credential pool consulted for run run-fb-1") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no consulted line for run-fb-1; log:\n%s", log)
	}
	if !strings.Contains(line, "api_key:anthropic") || !strings.Contains(line, "oauth:claude_code") {
		t.Fatalf("the run-level fallback's provider did not reach the wants: %s", line)
	}
}
