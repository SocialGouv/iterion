package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The fork lane's funding, judged before metering (ADR-162 decision 7,
// ADR-154 D13). A fork run executes on its lane's pool, so the publisher
// seals no shared tier into it (no org key, no contributor pool, no platform
// key); the runner refuses its pod's environment; and a forfait cannot cross
// its sandbox. Its team's own vendor API keys are all that funds it — no
// tier seals a team gateway credential into a pool run until #2005 PR-C' —
// and only on routes decided before the run: a gateway route, or an `${…}`
// dial the pool's runner expands with its own environment, is refused. A
// team funded only by shared keys would otherwise have every gesture metered
// on the org's budget and the outside author's bound, then refused at
// publish or at the runner.
//
// The judge also answers the fork lane's POOL rule first, as the launch and
// the resume do (a team mapped since, a pool another team holds, a pool the
// registry does not route): a refusal the publisher would reach after the
// gate charged is reached before it.
//
// The judge is the publisher's own unfunded decision read through the
// credential preview (runview.CredentialFundingJudge), on the program the
// launch or the resume would judge — never a second reading of funding. The
// gate asks it after its own refusals and before its first charge.

// gateForkFunding asks the subject's funding judge, for an untrusted subject
// that carries one. nil admits the subject to the metering arms.
func (s *Server) gateForkFunding(ctx context.Context, subj launchSubject) *launchDenial {
	if subj.Trust.Trusted() || subj.Funding == nil {
		return nil
	}
	err := subj.Funding(ctx)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runview.ErrCredentialPreviewUnavailable):
		// A run service whose publisher seals no credential decides nothing
		// about funding — not here, and not at publish either, where its walk
		// seals nothing and refuses nothing. That run's floor is the runner's
		// ambient refusal (store.RunTrust.AmbientLLMRefused).
		return nil
	case errors.Is(err, runview.ErrNoLLMCredential):
		return &launchDenial{status: http.StatusUnprocessableEntity, reason: denyForkRunUnfunded, detail: err.Error()}
	case errors.Is(err, cloudpublisher.ErrForkLanePool):
		return &launchDenial{status: http.StatusUnprocessableEntity, reason: denyForkLanePoolRefused, detail: err.Error()}
	case errors.Is(err, runview.ErrFundingUnjudgeable):
		return &launchDenial{status: http.StatusUnprocessableEntity, reason: denyForkFundingUnjudgeable, detail: err.Error()}
	default:
		return s.forkClosedDenial(denyForkFundingUnreadable,
			fmt.Sprintf("whether this fork run is funded could not be judged: %v — an untrusted launch is not metered on a verdict nobody reached", err))
	}
}

// webhookForkFunding is the funding judge of a fork-lane target, nil for a
// trusted one: the launch the tail would hand the run service, read without
// writing anything — the bot resolved raw (no cloud snapshot, the preview's
// resolution), the webhook's routing layer and pool attachment resolved as
// launchWebhookBot resolves them, the target's own vars, trust and lane pool,
// the config's key pins, the webhook's actor as owner and the delivery's
// repository as the keys' audience.
func (s *Server) webhookForkFunding(cfg webhooks.Config, meta webhookEventMeta, t forgeLaunchTarget) func(context.Context) error {
	if t.Trust.Trusted() {
		return nil
	}
	return func(ctx context.Context) error {
		if s.runs == nil {
			return runview.ErrCredentialPreviewUnavailable
		}
		ctx = secrets.WithLaunchRepo(store.WithTenant(ctx, cfg.TenantID), meta.ProjectPath)
		lb, err := s.resolveBotTieredRaw(ctx, cfg.TenantID, t.BotID, "")
		if err != nil {
			return fmt.Errorf("resolve bot %q: %w", t.BotID, err)
		}
		if lb == nil {
			return fmt.Errorf("%w: bot %q not found", runview.ErrFundingUnjudgeable, t.BotID)
		}
		defer lb.Cleanup()
		routePolicy, err := s.resolveRunLLMRoutePolicy(ctx, cfg.TenantID, t.BotID, llmroute.Layer{
			Source: llmroute.SourceWebhook,
			Policy: cfg.RoutingPolicy(),
		})
		if err != nil {
			return err
		}
		poolAttachment, err := s.resolveRunPoolAttachment(ctx, cfg.TenantID, t.BotID)
		if err != nil {
			return err
		}
		spec := runview.CredentialPreviewSpec{
			Context: runview.CredentialPreviewContext{
				TeamID: cfg.TenantID, BotID: lb.BotID,
				Source: runview.CredentialPreviewSource{Kind: "webhook", ID: cfg.ID},
			},
			OwnerID: webhookActor(cfg.ID),
			Launch: runview.LaunchSpec{
				Vars:               t.Vars,
				ProjectPath:        meta.ProjectPath,
				TrustedVarNames:    trustedVarNamesFor(t.Trust, cfg.OperatorLaunchVars),
				AllowUnknownInputs: true,
				Trust:              t.Trust,
				ForkLanePool:       forkLanePoolFor(cfg, t.Trust),
				KeyOverrides:       cfg.KeyOverrides,
			},
			LLMRoutePolicy: routePolicy,
			PoolAttachment: poolAttachment,
		}
		lb.Stamp(&spec.Launch)
		// The catalog tier compiles from its path, with its bundle prompts —
		// the credential preview's own promotion of a catalog source.
		if lb.Origin == "catalog" {
			spec.Launch.Source = ""
		}
		return s.runs.FundingRefusal(ctx, spec)
	}
}

// forkResumeFunding is the funding judge of a fork run's resume: the run's
// own source, resolved exactly as a bare resume resolves it
// (resumeSourceFiller), judged by the publisher against the run's frozen
// program, trust and pool stamp. A team that lost its own key since the
// launch refuses here, before the resume re-meters the org and the author.
func (s *Server) forkResumeFunding(run *store.Run) func(context.Context) error {
	return func(ctx context.Context) error {
		if s.runs == nil {
			return runview.ErrCredentialPreviewUnavailable
		}
		spec := runview.ResumeSpec{RunID: run.ID, FilePath: run.FilePath}
		cleanup, err := s.resumeSourceFiller(ctx, run, &spec)
		if err != nil {
			// The retry sweeper's own reading of the same failure: a store
			// blip re-arms, a bot gone from its catalog will never resolve.
			if errors.Is(err, errResumeResolveTransient) {
				return fmt.Errorf("resolve the run's resume source: %w", err)
			}
			return fmt.Errorf("%w: resolve the run's resume source: %w", runview.ErrFundingUnjudgeable, err)
		}
		if cleanup != nil {
			defer cleanup()
		}
		return s.runs.ResumeFundingRefusal(ctx, spec)
	}
}
