package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/auth"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// serviceLauncher is the trigger.Launcher for direct-mode subscriptions: it
// resolves the plan's bot to its .bot path and launches via the canonical
// runview.Service.Launch (cloud queue or local spawn, transparently). It lives
// in pkg/server — not pkg/trigger — so the trigger package stays free of a
// runview import (runview emits run-completion events back into the bus, which
// would otherwise cycle). Board-mode subscriptions never reach here; they go
// through NativeBoardEffect.Promote.
type serviceLauncher struct {
	runs   *runview.Service
	logger *iterlog.Logger
	// resolveRetry folds the plan's binding-level retry policy together
	// with the bot manifest, the machine default and the platform ceiling.
	// Injected rather than reached for, because the launcher deliberately
	// holds no *Server.
	resolveRetry func(ctx context.Context, teamID, botID string, higher ...retrypolicy.Layer) *store.RunRetryPolicy
	// resolveBot is the server's tiered bot resolution (the subscription's
	// team → platform override → baked catalog), injected for the same
	// no-*Server reason. REQUIRED: a second, override-blind resolution path
	// here is exactly what the resolver sweep forbids.
	resolveBot func(ctx context.Context, teamID, botID string) (*launchBot, error)
	// gate is the server's shared launch admission (suspend → concurrency →
	// launch rate → monthly caps), injected for the same no-*Server reason.
	// REQUIRED: a direct launch that skipped it would be the one cloud
	// launch nobody metered.
	gate func(ctx context.Context) (*launchAdmission, *launchDenial)
}

// triggerSpineActor is the identity the trigger spine launches under: the
// store owner on the ctx it stamps and the auth principal the launch gate
// meters on the subscription's team.
const triggerSpineActor = "trigger-spine"

// triggerLauncher builds the spine's direct-mode launcher — the ONE
// construction site, shared by the local and the cloud coordinator, so a
// capability wired here (the launch gate) cannot be wired on one spine and
// missing on the other.
func (s *Server) triggerLauncher() *serviceLauncher {
	return &serviceLauncher{
		runs:         s.runs,
		logger:       s.logger,
		resolveRetry: s.resolveRunRetryPolicy,
		resolveBot:   s.resolveBotSource,
		gate:         s.gateLaunch,
	}
}

func (l *serviceLauncher) Launch(ctx context.Context, plan trigger.LaunchPlan) (string, error) {
	if l.runs == nil {
		return "", errors.New("trigger: no run service wired for direct launch")
	}
	spec := runview.LaunchSpec{
		Vars:            plan.Vars,
		RepoURL:         plan.RepoURL,
		RepoRef:         plan.RepoRef,
		ProjectPath:     plan.Repo,
		KeyOverrides:    plan.KeyOverrides,
		SecretOverrides: plan.SecretOverrides,
		SourceRef:       plan.SourceRef,
		RetryPolicy:     l.retryPolicyFor(ctx, plan),
	}
	if l.resolveBot == nil {
		return "", errors.New("trigger: no bot resolver wired for direct launch")
	}
	if l.gate == nil {
		return "", errors.New("trigger: no launch gate wired for direct launch")
	}
	// The subscription owns the team, so the spine launches AS that team: the
	// store identity scopes the run and seals its credentials, and the auth
	// identity is what the gate reads to find the caps to apply.
	ctx = store.WithIdentity(ctx, plan.TenantID, triggerSpineActor)
	lb, err := l.resolveBot(ctx, plan.TenantID, plan.BotID)
	if err != nil {
		return "", fmt.Errorf("trigger: resolve bot %q: %w", plan.BotID, err)
	}
	defer lb.Cleanup()
	lb.Stamp(&spec)
	// The SAME admission every other launch surface passes — suspend →
	// concurrency → launch rate → monthly caps. The denial travels up as the
	// effect's error: the evaluator records it on the subscription (its
	// last_error) and the outbox retries it on the backoff.
	adm, deny := l.gate(auth.WithIdentity(ctx, auth.Identity{TeamID: plan.TenantID, UserID: triggerSpineActor}))
	if deny != nil {
		return "", deny.err()
	}
	res, err := l.runs.Launch(ctx, spec)
	if err != nil {
		// Every error out of Launch means no run started, so the metered slot
		// goes back — as the HTTP handler does.
		adm.rollback(l.logger)
		return "", err
	}
	return res.RunID, nil
}

// retryPolicyFor resolves the run's retry contract, tolerating a launcher
// built without a resolver (tests) by leaving the field nil — the consumer
// then applies the package defaults.
func (l *serviceLauncher) retryPolicyFor(ctx context.Context, plan trigger.LaunchPlan) *store.RunRetryPolicy {
	if l.resolveRetry == nil {
		return nil
	}
	return l.resolveRetry(ctx, plan.TenantID, plan.BotID, retrypolicy.Layer{
		Source: retrypolicy.SourceTrigger,
		Policy: plan.Retry,
	})
}

var _ trigger.Launcher = (*serviceLauncher)(nil)
