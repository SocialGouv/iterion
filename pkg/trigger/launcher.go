package trigger

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// LaunchPlan is the resolved intent the Evaluator hands to an effect: launch
// (or promote a board card for) this bot, with these vars, on behalf of this
// event. It is the source-agnostic translation of (Subscription, Event) — the
// production Launcher maps it onto a runview.LaunchSpec, the BoardEffect maps
// it onto a native card stamp.
type LaunchPlan struct {
	BotID    string
	TenantID string
	Repo     string
	Mode     bundle.ExecutionMode
	// Vars are the fully-resolved launch vars (subscription Vars + the
	// ArgsVar payload), ready to stamp on the run / card.
	Vars            map[string]string
	KeyOverrides    map[string]string
	SecretOverrides map[string]string
	// RepoURL/RepoRef target a cloud clone when the event carries them.
	RepoURL string
	RepoRef string
	// Event is the originating event (provenance, run→source back-link).
	Event Event
}

// Launcher launches a run directly (ExecutionDirect). The production impl
// wraps runview.Service.Launch; it lives in a separate wiring package so the
// trigger package stays free of a runview import (runview emits events back
// into the bus, which would otherwise cycle).
type Launcher interface {
	Launch(ctx context.Context, plan LaunchPlan) (runID string, err error)
}

// BoardEffect realises an ExecutionBoard plan by promoting a native board
// card (stamping Bot/BotArgs) so the dispatcher's existing Claim picks it up.
// It never launches directly — the
// dispatcher stays the sole launch authority, which is what makes the
// event fast-path and the poll safety-net structurally unable to double-launch.
type BoardEffect interface {
	Promote(ctx context.Context, plan LaunchPlan) (issueID string, err error)
}

// Nudger asks a consumer to act on a just-promoted card immediately instead
// of waiting for its next poll. *dispatcher.Dispatcher satisfies it via
// Refresh(). Optional — when absent, the dispatcher's 30s poll still picks
// up the promoted card (the safety net).
type Nudger interface {
	Refresh()
}
