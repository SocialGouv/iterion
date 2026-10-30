package forge

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
)

// The forge mints exactly one team token: the repo's hook identity, reserved
// by reserveHookIdentity and fabricated by webhooks.MintToken. The
// derivation chokepoint is asked at that reservation — where the mint is
// DECIDED, not where the request arrives — and the verdict it grants travels
// in the context the mint runs under, which the primitive itself requires:
// the class is closed at the ask AND at the mint, so no caller shape above
// the orchestrator can carry a team-token mint past it. These probes hold
// each side of that closure on its own:
//
//   - a nil seam refuses: the orchestrator mints nothing without an
//     approval source (errors.Is ErrMintNotApproved — the forge's own
//     refusal, not the primitive's);
//   - a refusing seam aborts with ErrMintNotApproved carrying the seam's
//     reason;
//   - a granting seam's verdict is the one the mint runs under: remove the
//     ask, and the mint reads a context without the verdict and refuses —
//     which is also what reddens if the approval stops being stamped.
//
// #2687, round 6: the requirement moved into the primitive, so a caller
// that never asks (the mutant the round-4 review chased — the ask parked in
// a branch arm, a mint bound to a local, an alias of the primitive) refuses
// at the mint whatever its shape, with no AST guard above it.

func mintApprovalOrch(t *testing.T, seam func(ctx context.Context) (context.Context, error)) *Orchestrator {
	t.Helper()
	o, _, sealer := newTestOrch(t)
	o.ApproveTeamTokenMint = seam
	seedConn(t, o, sealer)
	return o
}

func mintApprovalRequest() ProvisionRequest {
	return ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	}
}

func TestProvision_ANilMintApprovalSeamRefuses(t *testing.T) {
	o := mintApprovalOrch(t, nil)
	res, err := o.Provision(context.Background(), mintApprovalRequest())
	if err == nil {
		t.Fatalf("provision with no approval seam minted %+v", res)
	}
	if !errors.Is(err, ErrMintNotApproved) {
		t.Fatalf("refusal %v does not carry ErrMintNotApproved", err)
	}
	if _, gerr := o.Integrations.GetProvisionByConnRepo(context.Background(), "t1", "conn-1", "group/api"); !errors.Is(gerr, ErrIntegrationNotFound) {
		t.Fatalf("the refused provisioning left a reservation behind: %v", gerr)
	}
}

func TestProvision_ARefusingMintApprovalSeamAbortsTheMint(t *testing.T) {
	o := mintApprovalOrch(t, func(ctx context.Context) (context.Context, error) {
		return ctx, errMintSeamMarker
	})
	_, err := o.Provision(context.Background(), mintApprovalRequest())
	if err == nil {
		t.Fatal("the refusing seam's provisioning went through")
	}
	if !errors.Is(err, ErrMintNotApproved) {
		t.Fatalf("refusal %v does not carry ErrMintNotApproved", err)
	}
	if !errors.Is(err, errMintSeamMarker) {
		t.Fatalf("refusal %v does not carry the seam's own reason", err)
	}
}

// errMintSeamMarker is the refusing seam's error, named so the probe can
// tell a refusal the seam answered from one the orchestrator reached on its
// own (a mutation that stops consulting the seam answers neither).
var errMintSeamMarker = errors.New("the credential's session was revoked")

func TestProvision_TheGrantedVerdictIsWhatTheMintRunsUnder(t *testing.T) {
	seen := make(chan context.Context, 1)
	o := mintApprovalOrch(t, func(ctx context.Context) (context.Context, error) {
		seen <- ctx
		return auth.ContextWithMintVerdict(ctx, auth.NewMintVerdict(auth.MintTeamToken, "u1")), nil
	})
	res, err := o.Provision(context.Background(), mintApprovalRequest())
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !res.Created {
		t.Fatalf("expected Created=true, got %+v", res)
	}
	cfg, err := o.Webhooks.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	if cfg.TokenHash == "" {
		t.Fatal("the reserved hook identity carries no token hash: nothing was minted")
	}
	select {
	case seamCtx := <-seen:
		if seamCtx == nil {
			t.Fatal("the seam was handed no context")
		}
	default:
		t.Fatal("the seam was never asked: the mint ran without its approval")
	}
}
