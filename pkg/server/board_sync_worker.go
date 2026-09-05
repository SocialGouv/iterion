package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The project board's reconciliation net (ADR-097 §10).
//
// The two-way sync's fast paths are lossy by construction — a native card
// moves in one process and a human drags a card in a browser — so the pass
// below is what makes the two boards CONVERGE rather than merely usually
// agree. A net nobody runs is a comment, which is why it has a named owner
// here instead of a sentence in a doc.
//
// It is safe on N replicas because the run is ELECTED, not coordinated: each
// replica CAS-advances the binding's own watermark AND takes a bounded lease
// on it, and only the winner executes. No in-process global is the authority,
// and a replica dying mid-pass costs one lease TTL, not a stuck board.
//
// The lease is what the watermark alone cannot give: a pass slower than the
// binding's interval (floor 1m) makes the board due again while it is still
// running, and the next tick presents exactly the watermark that pass wrote —
// so it matches. Without the lease, two replicas then reconcile the same board
// at once and issue duplicate writes on the same cards.

// BoardSyncWorker reconciles every bound team's project board on its own
// interval.
type BoardSyncWorker struct {
	// Bindings is the team ⇄ board registry. Required.
	Bindings forge.BoardBindingStore
	// BoardClientFor resolves a binding's forge credential into a board
	// client. Required — it is what keeps this worker free of the connection
	// store's shape.
	BoardClientFor func(ctx context.Context, b forge.BoardBinding) (forge.BoardClient, error)
	// CardsFor resolves a tenant's native card store. Required.
	CardsFor func(ctx context.Context, tenantID string) (native.BoardStore, error)
	// Interval is the TICK cadence — how often the worker looks for due
	// bindings, not how often a board is synced (that is the binding's own
	// SyncEvery). Zero uses defaultBoardSyncTick.
	Interval time.Duration
	// Now is the clock, injected for tests.
	Now func() time.Time

	Logger *iterlog.Logger
}

// defaultBoardSyncTick is how often the worker looks for due bindings. It is
// deliberately finer than the smallest allowed SyncEvery so a binding is never
// systematically late by a whole tick.
const defaultBoardSyncTick = 30 * time.Second

func (w *BoardSyncWorker) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *BoardSyncWorker) tickInterval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return defaultBoardSyncTick
}

func (w *BoardSyncWorker) validate() error {
	switch {
	case w == nil || w.Bindings == nil:
		return errors.New("board sync: no binding store")
	case w.BoardClientFor == nil:
		return errors.New("board sync: no board-client resolver")
	case w.CardsFor == nil:
		return errors.New("board sync: no card-store resolver")
	}
	return nil
}

// Run ticks until the context is cancelled. Blocking; callers start it in a
// goroutine at server boot.
func (w *BoardSyncWorker) Run(ctx context.Context) {
	if err := w.validate(); err != nil {
		w.warn("board sync: not started: %v", err)
		return
	}
	t := time.NewTicker(w.tickInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := w.Tick(ctx); err != nil {
				// A failed tick must never stop the loop: the next one is the
				// recovery, and a worker that exits on a Mongo blip is a net
				// that silently stops catching.
				w.warn("board sync: tick failed: %v", err)
			}
		}
	}
}

// Tick runs one sweep and returns how many passes THIS replica executed.
//
// It fails only on an error that makes the sweep itself impossible (the
// binding store is unreachable). A single tenant's failure — a revoked token,
// an unreachable board — is logged and skipped: one team must never stop every
// other team's reconciliation, and the next tick retries it anyway.
func (w *BoardSyncWorker) Tick(ctx context.Context) (int, error) {
	if err := w.validate(); err != nil {
		return 0, err
	}
	now := w.now()
	due, err := w.Bindings.DueBindings(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("board sync: list due bindings: %w", err)
	}
	ran := 0
	for _, b := range due {
		// Elect: exactly one replica advances the watermark and takes the
		// lease, and only it runs. The owner token is per PASS, not per
		// replica: a replica whose earlier pass overran and lost the board
		// must not be able to release the lease its own next pass took.
		owner := uuid.NewString()
		won, err := w.Bindings.ClaimSync(ctx, b.TenantID, b.LastSyncedAt, now, owner)
		if err != nil {
			if errors.Is(err, forge.ErrBoardBindingNotFound) {
				continue // unbound between the list and the claim
			}
			w.warn("board sync: claim for team %s failed: %v", b.TenantID, err)
			continue
		}
		if !won {
			continue // another replica owns this pass
		}
		if w.runPass(ctx, b, owner) {
			ran++
		}
	}
	return ran, nil
}

// runPass reconciles one bound team, logging exactly one line either way: an
// Info carrying the counters and the duration, or a Warn naming the failure.
// A board drifting quietly must be visible in the logs, not in someone's
// confusion three days later.
//
// It reports whether the pass RAN — a claim taken and then abandoned on a
// revoked token is not a reconciliation, and counting it as one would make
// Tick's return value useless as a health signal.
func (w *BoardSyncWorker) runPass(ctx context.Context, b forge.BoardBinding, owner string) bool {
	// Hand the board back however this ends, so the next tick may claim
	// immediately and the lease TTL is only ever reached by a replica that
	// died. Released on a context that may already be cancelled — a shutdown
	// must not leave the board held for the whole TTL — so the release gets
	// its own short-lived context.
	//
	// The release is a CAS on this pass's own token: a pass that outlived the
	// TTL no longer owns the board, and clearing the lease its successor took
	// would re-admit the concurrent pass the lease exists to refuse. That
	// refusal is the ONE moment the overrun is knowable, so it is logged as
	// such rather than swallowed.
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		switch err := w.Bindings.ReleaseSync(rctx, b.TenantID, owner); {
		case errors.Is(err, forge.ErrBoardSyncLeaseLost):
			w.warn("board sync: team=%s board=%s: this pass outlived its %s lease and another replica has since claimed the board — it may have run concurrently; consider a longer interval",
				b.TenantID, b.Ref(), forge.BoardSyncLeaseTTL)
		case err != nil && !errors.Is(err, forge.ErrBoardBindingNotFound):
			// Not fatal: the lease expires on its own. Visible, though — a
			// board that keeps needing its TTL to come free is a symptom.
			w.warn("board sync: team=%s: release lease: %v", b.TenantID, err)
		}
	}()
	started := w.now()
	bc, err := w.BoardClientFor(ctx, b)
	if err != nil {
		w.warn("board sync: team=%s board=%s: no board client: %v", b.TenantID, b.Ref(), err)
		return false
	}
	cards, err := w.CardsFor(ctx, b.TenantID)
	if err != nil {
		w.warn("board sync: team=%s board=%s: no card store: %v", b.TenantID, b.Ref(), err)
		return false
	}
	res, err := ImportProjectBoard(ctx, bc, b.Ref(), b.Provider, cards, &ProjectImportOptions{
		Binding: &b,
		Now:     w.Now,
		Logger:  w.Logger,
	})
	took := w.now().Sub(started)
	if err != nil {
		// Warn, and DO NOT block the next tick: the watermark already
		// advanced and the deferred release hands the lease back, so a
		// persistently failing board retries on its own interval instead of
		// pinning the sweep on one bad tenant.
		w.warn("board sync: team=%s board=%s failed after %s: %v", b.TenantID, b.Ref(), took.Round(time.Millisecond), err)
		return false
	}
	w.info("board sync: team=%s board=%s items=%d moved=%d reflected=%d labelled=%d conflicts=%d refused_terminal=%d reflect_failed=%d skipped_no_card=%d skipped_archived=%d skipped=%d took=%s",
		b.TenantID, b.Ref(), res.Items, res.Moved, res.Reflected, res.Labelled,
		res.Conflicts, res.RefusedTerminal, res.ReflectFailed, res.SkippedNoCard, res.SkippedArchived, res.Skipped,
		took.Round(time.Millisecond))
	return true
}

func (w *BoardSyncWorker) info(format string, args ...any) {
	if w.Logger != nil {
		w.Logger.Info(format, args...)
	}
}

func (w *BoardSyncWorker) warn(format string, args ...any) {
	if w.Logger != nil {
		w.Logger.Warn(format, args...)
	}
}
