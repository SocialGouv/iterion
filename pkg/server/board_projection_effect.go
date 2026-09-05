package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// The reflect's FAST path (ADR-097 §10's named follow-up).
//
// A native card move materializes a `projection` row in the durable effect
// outbox, and this is what executes it: the card's current column reaches the
// bound forge board in seconds instead of at the next reconciliation pass —
// while inheriting the outbox's leased claim, bounded retry and visible
// dead-letter, none of which a bespoke worker would have had.
//
// It calls reflectNativeState, the SAME reflect the pass calls, with the same
// binding and the same option vocabulary. One implementation, two callers, so
// the two paths cannot disagree about what "already there" means — and the
// periodic pass stays the reconciliation net underneath, unchanged.

// boardProjectionEffect answers BOTH trigger seams — "is a projection owed?"
// and "execute it" — from one type, so a deployment can never materialize
// projection rows it has no way to execute.
type boardProjectionEffect struct {
	// Bindings is the team ⇄ board registry. Required.
	Bindings forge.BoardBindingStore
	// BoardClientFor resolves a binding's forge credential into a board
	// client — the sync worker's resolver, re-asserting ownership at use time.
	BoardClientFor func(ctx context.Context, b forge.BoardBinding) (forge.BoardClient, error)
	// CardsFor resolves a tenant's native card store.
	CardsFor func(ctx context.Context, tenantID string) (native.BoardStore, error)
	// Now is the clock, injected for tests.
	Now    func() time.Time
	Logger *iterlog.Logger
}

var (
	_ trigger.ProjectionBindings = (*boardProjectionEffect)(nil)
	_ trigger.ProjectionEffect   = (*boardProjectionEffect)(nil)
)

// HasBoardBinding reports whether this tenant has an external board to reflect
// onto. A tenant with none is not an error — it is most tenants.
func (p *boardProjectionEffect) HasBoardBinding(ctx context.Context, tenantID string) (bool, error) {
	if p == nil || p.Bindings == nil || tenantID == "" {
		return false, nil
	}
	switch _, err := p.Bindings.GetByTenant(ctx, tenantID); {
	case errors.Is(err, forge.ErrBoardBindingNotFound):
		return false, nil
	case err != nil:
		// Surfaced, never swallowed: the cloud tail materializes before
		// advancing its cursor, so a skipped projection is a reflect only the
		// periodic pass would ever make good.
		return false, fmt.Errorf("board projection: read binding for team %s: %w", tenantID, err)
	}
	return true, nil
}

// ReflectCard pushes one card's current column onto the bound board.
//
// Error semantics are the outbox's: nil = nothing more is owed (either the
// reflect wrote, or there was legitimately nothing to write), non-nil = the
// effect did not happen and the row must retry. That is why a forge refusal
// returns here where the 300-card pass merely counts it: a pass abandoning one
// card must not abandon the rest, while a single-card effect owns its own
// retry budget and a swallowed refusal would retire the row on a board that
// stayed diverged.
func (p *boardProjectionEffect) ReflectCard(ctx context.Context, ev trigger.Event) error {
	if err := p.validate(); err != nil {
		return err
	}
	tenantID, cardID := ev.TenantID, ev.Subject.ID
	if tenantID == "" || cardID == "" {
		return fmt.Errorf("board projection: event %s carries team %q and card %q, need both", ev.ID, tenantID, cardID)
	}
	binding, err := p.Bindings.GetByTenant(ctx, tenantID)
	switch {
	case errors.Is(err, forge.ErrBoardBindingNotFound):
		return nil // unbound between materialization and execution — the operator's call
	case err != nil:
		return fmt.Errorf("board projection: read binding for team %s: %w", tenantID, err)
	}

	cards, err := p.CardsFor(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("board projection: card store for team %s: %w", tenantID, err)
	}
	card, err := cards.Get(cardID)
	switch {
	case errors.Is(err, tracker.ErrNotFound) || (err == nil && card == nil):
		return nil // deleted between the move and the reflect — definitive
	case err != nil:
		return fmt.Errorf("board projection: read card %s: %w", cardID, err)
	}
	sync, ok := boundProjectSync(card, binding)
	if !ok {
		// Either the card has never been joined to this board — the project
		// import owns the join and never creates a card from an item (§6) — or
		// it was last synced against a DIFFERENT board, whose item id a write
		// here would land on. Both are the pass's to settle.
		return nil
	}
	if !p.boardStillHoldsWhatWeRecorded(ev, binding, sync) {
		return nil
	}

	bc, err := p.BoardClientFor(ctx, binding)
	if err != nil {
		return fmt.Errorf("board projection: board client for team %s: %w", tenantID, err)
	}
	// The recorded status IS what the board says, under the precondition
	// checked just above — which is exactly what the pass passes after
	// establishing the same fact by reading the board.
	var res ProjectImportResult
	opts := &ProjectImportOptions{Binding: &binding, Now: p.Now, Logger: p.Logger}
	switch reflectNativeState(ctx, bc, card, forge.ProjectItem{ID: sync.ItemID}, &sync, sync.Status, opts, &res) {
	case reflectFailed:
		return fmt.Errorf("board projection: the forge refused the status write for card %s on board %s (see the logged reason)", cardID, binding.Ref())
	case reflectWrote:
		return p.recordReflected(cards, card, sync)
	}
	return nil // already there, unmapped state, or no column for it — all inert
}

// boardStillHoldsWhatWeRecorded is the precondition reflectNativeState rests
// on, checked the only way a path that does NOT read the board can check it.
//
// The pass establishes "the board still says what iterion last recorded" by
// reading the item. The fast path cannot, so it reads the column the card
// LEFT: when that column maps to the recorded status, the two sides agreed
// right up to this move and the only divergence is the move itself. When it
// does NOT, someone moved the card on the board since the last pass — and
// pushing here would silently overwrite them, with no timestamp to arbitrate
// on. The pass reads both sides and applies §9's conflict rule; leave it to
// the pass, whose stale record is exactly what makes it re-derive the case.
//
// A previous state the map does not carry (`review`, …) is not a divergence:
// an unmapped state is inert (§2), so the recorded status is still the last
// true thing the board was told, and the precondition holds.
func (p *boardProjectionEffect) boardStillHoldsWhatWeRecorded(ev trigger.Event, b forge.BoardBinding, sync native.ExternalProject) bool {
	from, _ := ev.Payload["from_state"].(string)
	if from == "" {
		return true
	}
	was, mapped := forge.StatusForState(b.Mapping(), from)
	if !mapped {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(was), strings.TrimSpace(sync.Status))
}

// recordReflected persists what the reflect wrote, so the next pass — and the
// next projection — read "already equal" and do nothing. Without it the board
// is rewritten on every tick, burning API budget and stamping a fresh
// updatedAt that then wins every conflict against the operator.
//
// It writes ONLY when something changed, for the reason the pass does: an
// unconditional External write emits an EvtIssueUpdated the trigger spine
// consumes as `card.updated`, relaunching every label-matching subscription.
func (p *boardProjectionEffect) recordReflected(cards native.BoardStore, card *native.Issue, sync native.ExternalProject) error {
	if card.External != nil && card.External.Project.Equal(sync) {
		return nil
	}
	ext := card.External.Clone()
	if ext == nil {
		// Unreachable: boundProjectSync already refused a card with no
		// external ref. Named rather than assumed, so a future caller that
		// loosens that gate fails loudly instead of writing a naked ref.
		return fmt.Errorf("board projection: card %s has no external ref to record the reflect on", card.ID)
	}
	ext.Project = &sync
	if _, err := cards.Update(card.ID, native.Patch{External: ext}); err != nil {
		// The board write LANDED. Failing here re-runs an idempotent reflect
		// (the option comparison makes the retry a no-op on the forge) and
		// keeps the record honest, which beats reporting success on a card
		// whose sync state still names the old column.
		return fmt.Errorf("board projection: record the reflected status on card %s: %w", card.ID, err)
	}
	return nil
}

// boundProjectSync returns the card's sync state with THIS board, or false
// when the card is not joined to it.
func boundProjectSync(card *native.Issue, b forge.BoardBinding) (native.ExternalProject, bool) {
	if card.External == nil || card.External.Project == nil {
		return native.ExternalProject{}, false
	}
	sync := *card.External.Project
	if sync.ItemID == "" || !strings.EqualFold(sync.Owner, b.Owner) || sync.Number != b.Number {
		return native.ExternalProject{}, false
	}
	return sync, true
}

func (p *boardProjectionEffect) validate() error {
	switch {
	case p == nil || p.Bindings == nil:
		return errors.New("board projection: no binding store")
	case p.BoardClientFor == nil:
		return errors.New("board projection: no board-client resolver")
	case p.CardsFor == nil:
		return errors.New("board projection: no card-store resolver")
	}
	return nil
}
