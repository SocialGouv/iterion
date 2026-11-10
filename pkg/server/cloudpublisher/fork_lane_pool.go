package cloudpublisher

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ErrForkLanePool marks a run refused by the fork lane's pool rule (ADR-162
// Q3 and decision 10): an untrusted run executes only on the runner pool its
// fork lane named, a pool of ADR-154's registry whose runners resolve the
// kubernetes sandbox for every run. The refusal is DEFINITIVE — a run that
// names no pool, a pool its team's mapping or another team's sovereignty
// forbids — so the retry sweeper abandons a resume refused with it, as it
// abandons ErrPoolRemapped, rather than re-arming it every cycle. A store
// that cannot answer is not this error: it refuses too, but as the
// transient it is.
var ErrForkLanePool = errors.New("cloudpublisher: the fork lane's runner pool refuses this run")

// launchRunnerPool is the pool a launch is stamped with — the run
// document's and the queue message's RunnerPool, frozen for every resume,
// retry and forked child. A trusted launch executes where its team is
// mapped (runnerPoolForTeam), and names no lane pool of its own: one that
// does is refused rather than ignored, because the field exists to move an
// UNTRUSTED run and a trusted one carrying it is a surface confusing the two.
// An untrusted launch executes on the pool its fork lane named, under
// forkLanePoolRefusal's rule. A store that cannot answer refuses, as the
// team mapping always has.
func (p *Publisher) launchRunnerPool(ctx context.Context, tenantID string, trust store.RunTrust, lanePool string) (string, error) {
	teamPool, err := p.runnerPoolForTeam(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if trust.Trusted() {
		if lanePool != "" {
			return "", fmt.Errorf("%w: a trusted launch names the fork-lane pool %q — a trusted run executes on its team's mapping (%q), and the lane's pool is for untrusted runs only", ErrForkLanePool, lanePool, teamPool)
		}
		return teamPool, nil
	}
	if err := p.forkLanePoolRefusal(ctx, tenantID, teamPool, lanePool); err != nil {
		return "", err
	}
	return lanePool, nil
}

// forkLanePoolRefusal is the one rule an untrusted run's pool answers to, at
// its launch and at every resume (where pool is the run's frozen stamp and
// teamPool its team's mapping read fresh). nil admits; the registry's
// routing gate (poolRoutes) is the caller's next step, as for every pool.
//
//   - The run names a pool. The in-process engine and the shared runner do
//     not run untrusted work (decision 10): without a pool the run would
//     land on the shared consumer, whose override refuses it only at the
//     runner — after it was metered and queued.
//   - The pool is a pool name, the registry's grammar.
//   - A team mapped to a sovereign pool runs everything there (ADR-154): its
//     fork lane must name that pool, never a pool its code and sealed
//     credentials would share with other teams' fork runs.
//   - An unmapped team's fork lane names a pool no team holds — a pool held
//     by another team is that team's sovereign boundary, and a fork run
//     landing there would cross it.
func (p *Publisher) forkLanePoolRefusal(ctx context.Context, tenantID, teamPool, pool string) error {
	if pool == "" {
		return fmt.Errorf("%w: an untrusted run names no runner pool — it executes only on the pool its fork-lane config names, whose runners resolve the kubernetes sandbox (ADR-162 decision 10); recreate the fork-lane webhook naming fork_lane_pool", ErrForkLanePool)
	}
	if !queue.ValidPoolName(pool) {
		return fmt.Errorf("%w: runner pool %q is not a pool name (want 1–31 chars [a-z0-9-], starting alphanumeric)", ErrForkLanePool, pool)
	}
	if teamPool != "" {
		if teamPool != pool {
			return fmt.Errorf("%w: team %s is mapped to the sovereign pool %q, and its untrusted run names %q — a mapped team's runs execute on its own pool, so its fork lane must name %q", ErrForkLanePool, tenantID, teamPool, pool, teamPool)
		}
		return nil
	}
	// No identity seam means no mapping table: no team can hold the pool.
	if p.identity == nil {
		return nil
	}
	holder, err := p.identity.GetTeamByRunnerPool(ctx, pool)
	if errors.Is(err, identity.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cloudpublisher: resolve the team holding runner pool %q: %w (refusing — who holds a pool must be known, never guessed)", pool, err)
	}
	if holder.ID != tenantID {
		return fmt.Errorf("%w: runner pool %q is the sovereign pool of team %s — an untrusted run of team %s cannot execute there", ErrForkLanePool, pool, holder.ID, tenantID)
	}
	return nil
}

// routesForTrust is the registry's routing gate (poolRoutes) as a run of
// trust answers it, at the launch, the resume and the pre-metering funding
// judge alike. For an untrusted run the registry's DECIDED refusal
// (ErrPoolNotRouted) is the fork lane's own: the run's pool is the one its
// fork-lane config names, and the config is immutable, so every gesture,
// durable retry and resume would meet the same answer — a misspelled or
// never-activated pool is refused as ErrForkLanePool (definitive, never the
// retryable registry blip), naming the remedy. A registry that cannot be read
// stays the transient it is.
func (p *Publisher) routesForTrust(ctx context.Context, pool string, trust store.RunTrust) error {
	err := p.poolRoutes(ctx, pool)
	if err != nil && !trust.Trusted() && errors.Is(err, ErrPoolNotRouted) {
		return fmt.Errorf("%w: %w — a fork-lane config's pool cannot be edited: register the pool and set it active, or recreate the fork-lane webhook naming a pool the registry routes", ErrForkLanePool, err)
	}
	return err
}
