package trigger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrSubscriptionNotFound is returned by Get/Update/Delete for an unknown id.
var ErrSubscriptionNotFound = fmt.Errorf("trigger: subscription not found")

// ErrSubscriptionConflict is returned by Update when the row moved since the
// caller's copy was read — its version no longer matches the stored one. The
// caller re-reads and reapplies; nothing it held was written.
var ErrSubscriptionConflict = errors.New("trigger: subscription changed concurrently; re-read and retry")

// SubscriptionStore persists trigger subscriptions. It mirrors
// forge.RepoIntegrationStore: an interface with an in-memory impl (tests /
// local single-host) and a Mongo impl (cloud multitenant). The three query
// primitives are the product surfaces — ListByRepo and ListByBot power the
// studio "by repo" / "by bot" views, ListCandidates is the Evaluator hot path.
type SubscriptionStore interface {
	Create(ctx context.Context, s Subscription) error
	Get(ctx context.Context, id string) (Subscription, error)
	// Update replaces the row as a full document, committing only against
	// the version the caller's copy carries (a read of the row returns that
	// version): ErrSubscriptionConflict when the row moved since — the
	// caller re-reads and reapplies, never writes over what it did not see.
	// A version-0 copy also matches a document written before the field
	// existed, so a deployed row's first versioned write lands. The health
	// stamps (LastError*, LastRun*) are the marks' and survive the write:
	// the marks bump no version, so a copy read before a fire passes the
	// CAS, and the store keeps the stored stamps over the copy's.
	Update(ctx context.Context, s Subscription) error
	Delete(ctx context.Context, id string) error

	// MarkLaunchError records the verdict of the subscription's last direct
	// launch: a non-empty message raises last_error with its instant, "" clears
	// both. A TARGETED field write rather than a read-modify-replace — the
	// recorder holds a copy the evaluator matched, which on an outbox retry is
	// minutes old, and an operator editing the row in between must not lose the
	// edit to a health write.
	MarkLaunchError(ctx context.Context, id, lastError string, at time.Time) error

	// MarkRunOutcome stamps the OUTCOME of the last run this subscription
	// launched — the run-terminal counterpart of MarkLaunchError, and the
	// twin of cloudsched.Store.MarkRunOutcome. Called by the run-outcome
	// spine, which holds no copy of the row, so it is a TARGETED field
	// write for the same reason MarkLaunchError is.
	//
	// errMsg/errCode empty CLEAR the stored failure: a successful run must
	// not leave the previous one's error on the record. `at` is the run's
	// terminal instant and carries a MONOTONICITY GUARD — an out-of-order
	// delivery is a silent no-op, never a backwards move. ErrSubscriptionNotFound
	// when the row is gone.
	MarkRunOutcome(ctx context.Context, id, runID, status, errMsg, errCode string, at time.Time) error

	// ClearCredentialPins removes, from the subscription as the store holds
	// it NOW, every key_overrides / secret_overrides entry whose value
	// names recordID, reporting whether it removed any. The judgment
	// happens at write time against the stored row — never against the
	// caller's listing copy — so a health stamp, a pin edit, or a second
	// record's sweep landing between the caller's list and this clear
	// survives untouched. Every real removal also bumps the row's version,
	// so a full-document editor holding a pre-sweep snapshot conflicts and
	// re-reads instead of writing the swept pin back — no update lane
	// resurrects a swept pin. The one writer left that can store a pin
	// naming a record deleted mid-request is a row BORN mid-delete (a
	// write whose validation beat the revocation and landed after the
	// sweep's listing): the write route itself re-checks the records its
	// landed pins name and calls this clear for a revoked one — the
	// delete's listing never saw the row, so nothing on the delete side
	// can. A clear that removes nothing writes nothing.
	// ErrSubscriptionNotFound when the row is gone — to the sweep, a row
	// that died mid-flight has no pins left to clear.
	ClearCredentialPins(ctx context.Context, id, recordID string) (bool, error)

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
