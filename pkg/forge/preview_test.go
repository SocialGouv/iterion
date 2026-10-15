package forge

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

func TestPreviewEnable_CommandOnlyBotNotConflict(t *testing.T) {
	botFn := func(_ context.Context, _, b string) (*bundle.ForgeRequirements, error) {
		if b == "review-pr" {
			return &bundle.ForgeRequirements{Events: []string{bundle.ForgeEventPullRequest}}, nil
		}
		return nil, nil // feature-dev declares no forge: block
	}
	invFn := func(_ context.Context, _, b string) ([]bundle.Invocation, error) {
		if b == "feature-dev" {
			return []bundle.Invocation{{Kind: bundle.InvocationKindCommand, Mode: bundle.ExecutionBoard,
				Command: &bundle.InvocationCommand{Name: "featurly"}}}, nil
		}
		return nil, nil
	}
	pv := PreviewEnable(context.Background(), "t1", botFn, invFn, []string{"review-pr", "feature-dev"})
	if len(pv.Conflicts) != 0 {
		t.Fatalf("command-only bot must NOT be a conflict, got %v", pv.Conflicts)
	}
	if pv.Commands["featurly"] != "feature-dev" {
		t.Errorf("commands: %v", pv.Commands)
	}
	// pull_request (review-pr forge) + pull_request_comment (featurly command).
	if len(pv.Events) != 2 {
		t.Errorf("events: want 2 (forge + comment), got %v", pv.Events)
	}
	if pv.Binds["feature-dev"] != bundle.DefaultForgeSecretName {
		t.Errorf("command-only bot should bind the default forge_token: %v", pv.Binds)
	}
}

// TestPreviewEnable_GatesMergesFollowsTheStatusesScope: the posture the
// operator consents to in the enable dialog must be the one Provision arms,
// so the preview reads the SAME declaration (the `statuses` token scope) that
// forces review_on_sync.
func TestPreviewEnable_GatesMergesFollowsTheStatusesScope(t *testing.T) {
	invFn := func(context.Context, string, string) ([]bundle.Invocation, error) { return nil, nil }
	for _, tc := range []struct {
		name string
		bots []string
		want bool
	}{
		{"a statuses bot gates", []string{"gate-bot"}, true},
		{"co-enabled with a non-gating bot", []string{"review-pr", "gate-bot"}, true},
		{"no statuses scope anywhere", []string{"review-pr", "revi-converse"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pv := PreviewEnable(context.Background(), "t1", testBotLookup, invFn, tc.bots)
			if len(pv.Conflicts) != 0 {
				t.Fatalf("fixture bots must all be provisionable, got %v", pv.Conflicts)
			}
			if pv.GatesMerges != tc.want {
				t.Errorf("GatesMerges = %v, want %v for %v — the dialog would promise a posture "+
					"other than the one the provision installs", pv.GatesMerges, tc.want, tc.bots)
			}
		})
	}
}

// TestBotGatesMerges_IsTheOneDerivation pins the predicate every surface
// shares: the declared `statuses` scope, at any level, and nothing else.
func TestBotGatesMerges_IsTheOneDerivation(t *testing.T) {
	if BotGatesMerges(nil) {
		t.Error("a bot with no forge: block declares no status")
	}
	if BotGatesMerges(&bundle.ForgeRequirements{TokenScopes: map[string]string{"pull_requests": "write"}}) {
		t.Error("pull_requests:write is not a commit status")
	}
	if !BotGatesMerges(&bundle.ForgeRequirements{TokenScopes: map[string]string{bundle.ForgeScopeStatuses: "write"}}) {
		t.Error("a declared statuses scope is what makes a bot a required check")
	}
}

func TestPreviewEnable_NoForgeNoInvocationIsConflict(t *testing.T) {
	pv := PreviewEnable(context.Background(), "t1",
		func(context.Context, string, string) (*bundle.ForgeRequirements, error) { return nil, nil },
		func(context.Context, string, string) ([]bundle.Invocation, error) { return nil, nil },
		[]string{"orchestrator"},
	)
	if len(pv.Conflicts) != 1 {
		t.Errorf("a bot with neither forge: nor an invocation should conflict, got %v", pv.Conflicts)
	}
}
