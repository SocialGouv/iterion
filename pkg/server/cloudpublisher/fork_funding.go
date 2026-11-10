package cloudpublisher

import (
	"context"
	"errors"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The funding judges: the live launch's unfunded refusal, answered BEFORE a
// launch or a resume is metered. A fork run is funded by its team's own API
// keys alone (ADR-154 D13 on its lane's pool, ADR-162 decision 7 on its
// sandbox), and its gesture meters the org's budget and the outside author's
// bound before the publisher ever walks a tier — so a team funded only by
// shared keys would pay a metered unit for a run the publisher refuses, or
// that the runner refuses ambient. The launch gate (pkg/server) asks these
// judges first and refuses, typed, without charging anything.
//
// Both judges ARE the credential preview: the same metadata walk, the same
// postures and the same unfundedRefusalFor, read on the program the live
// launch (FundingRefusal) or the live resume (ResumeFundingRefusal) judges.
// A second judgement of funding here would be a gate free to admit what the
// publisher refuses, or to refuse what it seals.

// The launch gate reaches both judges through the run service, which holds
// this publisher as its LaunchPublisher.
var _ runview.CredentialFundingJudge = (*Publisher)(nil)

// FundingRefusal answers whether the launch spec describes is funded: nil
// when it starts, the typed *UnfundedProvidersError (unwrapping to
// runview.ErrNoLLMCredential) when the live launch would refuse it, an
// untyped error when the verdict is undecided (a tier's metadata could not
// be read) or the walk failed, and runview.ErrCredentialPreviewUnavailable
// when this publisher seals no credential at all — the live walk decides
// nothing there either. Nothing is acquired, sealed, probed or persisted.
func (p *Publisher) FundingRefusal(ctx context.Context, spec runview.CredentialPreviewSpec, wf *ir.Workflow) error {
	_, verdict, err := p.previewCredentials(ctx, spec, wf, launchPreviewProgram(spec, wf))
	if err != nil {
		return err
	}
	return verdict.err()
}

// ResumeFundingRefusal is FundingRefusal for a resume of prior: the program
// SubmitResume hands the walk — the dispatch view over the run's own inputs,
// its frozen pinned set, model overrides, fallback chain, routing policy and
// pool attachment, its owner, key pins and repository — and the run's frozen
// trust and pool stamp, never its team's current mapping. A key the team
// removed since the launch is therefore a refusal here, before the resume is
// metered, exactly as the resume's own walk would refuse it at publish.
func (p *Publisher) ResumeFundingRefusal(ctx context.Context, prior *store.Run, wf *ir.Workflow) error {
	if prior == nil {
		return errors.New("cloudpublisher: resume funding: no run document to judge")
	}
	view := model.RouteViewForDispatch(wf, buildModelOverridesFromRun(prior.ModelOverrides), runFallbackEntriesFromRun(prior.Fallback), prior.Inputs)
	view.AllowedProviders = policyProviders(prior.LLMRoutePolicy)
	spec := runview.CredentialPreviewSpec{
		Context: runview.CredentialPreviewContext{
			TeamID: prior.TenantID, BotID: prior.BotID,
			Source: runview.CredentialPreviewSource{Kind: "resume", ID: prior.ID},
		},
		OwnerID: prior.OwnerID,
		Launch: runview.LaunchSpec{
			BotID:        prior.BotID,
			Vars:         launchVarsAsStrings(prior.Inputs),
			KeyOverrides: prior.KeyOverrides,
			Trust:        prior.Trust,
		},
		LLMRoutePolicy: prior.LLMRoutePolicy,
		PoolAttachment: prior.PoolAttachment,
	}
	if !prior.Trust.Trusted() {
		spec.Launch.ForkLanePool = prior.RunnerPool
	}
	ctx = secrets.WithLaunchRepo(store.WithOwner(store.WithTenant(ctx, prior.TenantID), prior.OwnerID), prior.ProjectPath)
	_, verdict, err := p.previewCredentials(ctx, spec, wf, previewProgram{view: view, pinned: prior.PinnedProviders})
	if err != nil {
		return err
	}
	return verdict.err()
}
