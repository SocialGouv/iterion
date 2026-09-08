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
