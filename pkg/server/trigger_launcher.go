package server

import (
	"context"
	"errors"
	"fmt"

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
	resolveRetry func(botID string, higher ...retrypolicy.Layer) *store.RunRetryPolicy
	// resolveBot is the server's tiered bot resolution (platform override →
	// baked catalog), injected for the same no-*Server reason. REQUIRED: a
	// second, override-blind resolution path here is exactly what the
	// resolver sweep forbids.
	resolveBot func(ctx context.Context, botID string) (*launchBot, error)
}

func newServiceLauncher(runs *runview.Service, logger *iterlog.Logger, resolveRetry func(string, ...retrypolicy.Layer) *store.RunRetryPolicy, resolveBot func(context.Context, string) (*launchBot, error)) *serviceLauncher {
	return &serviceLauncher{runs: runs, logger: logger, resolveRetry: resolveRetry, resolveBot: resolveBot}
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
		RetryPolicy:     l.retryPolicyFor(plan),
	}
	if l.resolveBot == nil {
		return "", errors.New("trigger: no bot resolver wired for direct launch")
	}
	lb, err := l.resolveBot(ctx, plan.BotID)
	if err != nil {
		return "", fmt.Errorf("trigger: resolve bot %q: %w", plan.BotID, err)
	}
	defer lb.Cleanup()
	lb.Stamp(&spec)
	res, err := l.runs.Launch(ctx, spec)
	if err != nil {
		return "", err
	}
	return res.RunID, nil
}

// retryPolicyFor resolves the run's retry contract, tolerating a launcher
// built without a resolver (tests) by leaving the field nil — the consumer
// then applies the package defaults.
func (l *serviceLauncher) retryPolicyFor(plan trigger.LaunchPlan) *store.RunRetryPolicy {
	if l.resolveRetry == nil {
		return nil
	}
	return l.resolveRetry(plan.BotID, retrypolicy.Layer{
		Source: retrypolicy.SourceTrigger,
		Policy: plan.Retry,
	})
}

var _ trigger.Launcher = (*serviceLauncher)(nil)
