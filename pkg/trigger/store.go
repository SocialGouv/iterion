package trigger

import (
	"context"
	"fmt"
)

// ErrSubscriptionNotFound is returned by Get/Update/Delete for an unknown id.
var ErrSubscriptionNotFound = fmt.Errorf("trigger: subscription not found")

// SubscriptionStore persists trigger subscriptions. It mirrors
// forge.RepoIntegrationStore: an interface with an in-memory impl (tests /
// local single-host) and a Mongo impl (cloud multitenant). The three query
// primitives are the product surfaces — ListByRepo and ListByBot power the
// studio "by repo" / "by bot" views, ListCandidates is the Evaluator hot path.
type SubscriptionStore interface {
	Create(ctx context.Context, s Subscription) error
	Get(ctx context.Context, id string) (Subscription, error)
	Update(ctx context.Context, s Subscription) error
	Delete(ctx context.Context, id string) error

	// ListByTenant returns every subscription owned by a tenant
	// ("" tenant = the local single-host scope).
	ListByTenant(ctx context.Context, tenantID string) ([]Subscription, error)
	// ListByRepo returns subscriptions scoped to a specific repo plus the
	// tenant-wide (Repo == "") ones, since both can fire for a repo event.
	ListByRepo(ctx context.Context, tenantID, repo string) ([]Subscription, error)
	// ListByBot returns every subscription that launches a given bot.
	ListByBot(ctx context.Context, tenantID, botID string) ([]Subscription, error)
	// ListByOrigin returns subscriptions provisioned under an Origin marker
	// (e.g. "forge:<repo_integration_id>") so a deprovision can delete exactly
	// the rows it created.
	ListByOrigin(ctx context.Context, tenantID, origin string) ([]Subscription, error)
	// ListCandidates returns the enabled subscriptions that COULD match ev:
	// same tenant, and Repo matching ev.Repo or tenant-wide. The Evaluator
	// applies the full Matcher.Match on the returned set — this is only the
	// indexed pre-filter, kept coarse so it stays a single-index query.
	ListCandidates(ctx context.Context, ev Event) ([]Subscription, error)
}
