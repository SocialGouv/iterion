package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// withheldHeadGateClient answers with a head repository the forge HAS and the
// credential could not name — the shape a GitLab fork MR takes when the read
// of its source project is refused, and a deleted fork's on every provider.
type withheldHeadGateClient struct {
	fakeGateClient
	headErr error
}

func (c *withheldHeadGateClient) GetPullRequest(_ context.Context, _ string, _ int) (forge.PullRef, error) {
	return forge.PullRef{
		HeadSHA:          "deadbeef",
		State:            "open",
		HeadRepoDeclared: true, // declared, and deliberately unnamed
		HeadRepoErr:      c.headErr,
	}, nil
}

// The API-side fork guard must read the WITHHELD flag, not just the empty
// name: the launch is refused either way, but a refusal that says "the
// provider named no head repo" when the forge did name one and refused to
// describe it sends the operator looking in the wrong place. The forge's own
// typed refusal is quoted so the cause is actionable.
func TestPRLaunchForkGuard_NamesAWithheldHeadAndItsCause(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	s.cfg.PublicURL = "https://iterion.test"
	gc := &withheldHeadGateClient{
		headErr: errors.New("gitlab: get source project 13: the credential may not read the merge request's source project"),
	}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }

	_, err := s.applyPRLaunchContext(context.Background(), "team1", "conn1", "review-pr",
		map[string]string{"pr_url": "https://github.com/o/r/pull/42"}, nil)
	if err == nil {
		t.Fatal("a head repository the forge would not name must never be launched on — the pair is <base>.CloneURL + a branch that may live elsewhere")
	}
	if !errors.Is(err, errPRLaunchForkGuard) {
		t.Fatalf("err = %v, want the typed fork-guard refusal so the launch answers 422", err)
	}
	if !strings.Contains(err.Error(), "declared") {
		t.Errorf("err = %v, want the refusal to say the head repo was DECLARED and not named", err)
	}
	if !strings.Contains(err.Error(), "source project") {
		t.Errorf("err = %v, want the forge's own refusal quoted so the operator knows what to fix", err)
	}
}

// A head the provider never declared keeps its own wording: nothing was
// refused, so there is no cause to quote.
func TestPRLaunchForkGuard_UndeclaredHeadIsRefusedWithoutInventingACause(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	s.cfg.PublicURL = "https://iterion.test"
	gc := &fakeGateClient{headSHA: "deadbeef", noHeadRepo: true}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }

	_, err := s.applyPRLaunchContext(context.Background(), "team1", "conn1", "review-pr",
		map[string]string{"pr_url": "https://github.com/o/r/pull/42"}, nil)
	if !errors.Is(err, errPRLaunchForkGuard) {
		t.Fatalf("err = %v, want the typed fork-guard refusal", err)
	}
	if strings.Contains(err.Error(), "declared") {
		t.Errorf("err = %v — an undeclared head must not be reported as withheld", err)
	}
}
