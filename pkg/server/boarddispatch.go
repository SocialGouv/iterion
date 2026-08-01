package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// boardCoordinator is the cross-tenant board view the cloud dispatcher needs.
// *boardmongo.Coordinator satisfies it; tests pass a fake.
type boardCoordinator interface {
	ListEligible(ctx context.Context, eligible []string, limit int) ([]boardmongo.Candidate, error)
	Claim(ctx context.Context, tenant, id, marker string) error
	SetState(ctx context.Context, tenant, id, state string) error
	Release(ctx context.Context, tenant, id, marker string) error
}

// errCardPaused marks a processBoardCard error whose run parked on a
// human/operator gate (paused_waiting_human / paused_operator) rather than
// failing. processCard routes such a card to the awaiting-input column, not
// blocked.
var errCardPaused = errors.New("board dispatcher: run paused awaiting input")

// boardDispatcher polls the cloud board for eligible cards and runs each via
// the injected process func (launch + poll-to-terminal). Multi-replica-safe
// WITHOUT leader election: the per-card Claim is a CAS, so each card is claimed
// by exactly one replica; the rest skip. In-flight cards are bounded by a
// semaphore shared across ticks, so a slow run never starves polling.
type boardDispatcher struct {
	coord   boardCoordinator
	process func(ctx context.Context, tenant string, iss native.Issue) error
	marker  string

	eligible        []string
	inProgressState string
	doneState       string
	blockedState    string
	awaitingState   string

	// statusFor + clearBadge power the parked-card sweep (sweepParked).
	// statusFor reads a run's persisted status for a tenant; clearBadge
	// clears the card's denormalized awaiting-input hint. Both optional —
	// a nil statusFor disables the sweep.
	statusFor  func(ctx context.Context, tenant, runID string) (store.RunStatus, error)
	clearBadge func(tenant, id string)

	interval time.Duration
	sem      chan struct{}
	logger   *iterlog.Logger
	wg       sync.WaitGroup // tracks in-flight processCard goroutines (for tests + drain)
}

// newBoardDispatcher wires a cloud board dispatcher with sensible defaults.
func newBoardDispatcher(coord boardCoordinator, process func(context.Context, string, native.Issue) error, marker string, concurrency int, logger *iterlog.Logger) *boardDispatcher {
	if concurrency <= 0 {
		concurrency = 4
	}
	return &boardDispatcher{
		coord:           coord,
		process:         process,
		marker:          marker,
		eligible:        []string{native.StateReady},
		inProgressState: native.StateInProgress,
		doneState:       native.StateDone,
		blockedState:    native.StateBlocked,
		awaitingState:   native.StateAwaitingInput,
		interval:        5 * time.Second,
		sem:             make(chan struct{}, concurrency),
		logger:          logger,
	}
}

// tick claims as many eligible cards as there are free slots and dispatches
// each in a detached goroutine. Returns the number it claimed this tick.
func (d *boardDispatcher) tick(ctx context.Context) int {
	cands, err := d.coord.ListEligible(ctx, d.eligible, cap(d.sem)*2)
	if err != nil {
		d.warn("list eligible: %v", err)
		return 0
	}
	claimed := 0
	for _, c := range cands {
		select {
		case d.sem <- struct{}{}: // acquired a slot
		default:
			return claimed // no free slots; the rest wait for the next tick
		}
		if err := d.coord.Claim(ctx, c.Tenant, c.Issue.ID, d.marker); err != nil {
			<-d.sem // claim lost (another replica won, or conflict) — release the slot
			continue
		}
		claimed++
		d.wg.Add(1)
		go d.processCard(ctx, c)
	}
	return claimed
}

func (d *boardDispatcher) processCard(ctx context.Context, c boardmongo.Candidate) {
	defer d.wg.Done()
	defer func() { <-d.sem }()
	// Move to in-progress for board visibility (best-effort).
	if err := d.coord.SetState(ctx, c.Tenant, c.Issue.ID, d.inProgressState); err != nil {
		d.warn("card %s/%s → in_progress: %v", c.Tenant, c.Issue.ID, err)
	}
	runErr := d.process(ctx, c.Tenant, c.Issue)
	final := d.doneState
	if runErr != nil {
		// A pause is not a failure: route the card to the awaiting-input
		// column so the operator answers it there, not to blocked.
		if errors.Is(runErr, errCardPaused) {
			final = d.awaitingState
		} else {
			final = d.blockedState
			d.warn("card %s/%s run failed: %v", c.Tenant, c.Issue.ID, runErr)
		}
	}
	if err := d.coord.SetState(ctx, c.Tenant, c.Issue.ID, final); err != nil {
		d.warn("card %s/%s → %s: %v", c.Tenant, c.Issue.ID, final, err)
	}
	if err := d.coord.Release(ctx, c.Tenant, c.Issue.ID, d.marker); err != nil {
		d.warn("card %s/%s release: %v", c.Tenant, c.Issue.ID, err)
	}
}

// sweepParked reconciles cards parked in the awaiting-input column whose
// runs have since reached a terminal status. A cloud card parks UNCLAIMED
// (processCard moved it to awaitingState and released), and every resume
// surface — answer-from-board, run console, CLI — completes the run outside
// this dispatcher's poll loop (which returned at the pause), so without the
// sweep the card strands in awaiting_input forever. Mirrors the local
// dispatcher's reconcileParked (pkg/dispatcher/parked.go): finished →
// doneState, hard-failed → blockedState; resumable statuses (paused_*,
// failed_resumable, cancelled) and in-flight ones stay parked. Multi-replica
// safe without claims: a concurrent double-move lands on the same final
// state, and awaiting_input is not in `eligible` so no replica re-dispatches.
func (d *boardDispatcher) sweepParked(ctx context.Context) {
	if d.statusFor == nil {
		return
	}
	cands, err := d.coord.ListEligible(ctx, []string{d.awaitingState}, 200)
	if err != nil {
		d.warn("parked sweep list: %v", err)
		return
	}
	for _, c := range cands {
		runID := c.Issue.LastRunID
		if runID == "" {
			continue
		}
		st, err := d.statusFor(ctx, c.Tenant, runID)
		if err != nil {
			continue // best-effort: unreadable run → leave the card alone
		}
		var target string
		switch st {
		case store.RunStatusFinished:
			target = d.doneState
			d.log("card %s/%s resumed out-of-band and finished (run=%s) — moving to %s", c.Tenant, c.Issue.ID, runID, target)
		case store.RunStatusFailed:
			target = d.blockedState
			d.warn("card %s/%s resumed out-of-band and failed hard (run=%s) — moving to %s", c.Tenant, c.Issue.ID, runID, target)
		default:
			continue // still paused / resumable / in flight — genuinely awaiting the operator
		}
		if d.clearBadge != nil {
			d.clearBadge(c.Tenant, c.Issue.ID)
		}
		if err := d.coord.SetState(ctx, c.Tenant, c.Issue.ID, target); err != nil {
			d.warn("parked sweep move %s/%s → %s: %v", c.Tenant, c.Issue.ID, target, err)
		}
	}
}

// run loops tick every interval until ctx is cancelled, then drains in-flight
// cards. Start one per replica.
func (d *boardDispatcher) run(ctx context.Context) {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		d.tick(ctx)
		d.sweepParked(ctx)
		select {
		case <-ctx.Done():
			d.wg.Wait() // let in-flight cards finish their state transition
			return
		case <-t.C:
		}
	}
}

func (d *boardDispatcher) warn(format string, args ...any) {
	if d.logger != nil {
		d.logger.Warn("board dispatcher: "+format, args...)
	}
}

func (d *boardDispatcher) log(format string, args ...any) {
	if d.logger != nil {
		d.logger.Info("board dispatcher: "+format, args...)
	}
}

// processBoardCard is the cloud board dispatcher's process func: launch the
// card's bot for its tenant through the run service (→ publisher), then poll
// the run record until it terminates. Returns nil on a clean finish, an error
// on failure or pause (the dispatcher then moves the card to blocked). The
// tenant identity is stamped on ctx so the publisher seals credentials.
// Reserved BotArgs keys carrying the launch context a webhook-launched card
// targets — the repo to clone AND the webhook's BYOK key / secret overrides.
// The board coordinator launches from the card and otherwise has NONE of this
// (the webhook's own SecretOverrides/KeyOverrides never reach it). ensureBoardCard
// stamps them; liftBoardLaunchContext extracts them into the LaunchSpec so they
// don't leak to the bot as vars. Secret resolution ALSO works via a (tenant,bot)
// binding — the override is the belt to that braces, and the only route for a
// per-webhook KeyOverride (BYOK billing), which has no binding equivalent.
const (
	boardRepoURLKey         = "__iterion_repo_url"
	boardRepoRefKey         = "__iterion_repo_ref"
	boardKeyOverridesKey    = "__iterion_key_overrides"
	boardSecretOverridesKey = "__iterion_secret_overrides"
)

// boardLaunchContext is the non-var launch state lifted off a card's BotArgs.
type boardLaunchContext struct {
	Vars            map[string]string
	RepoURL         string
	RepoRef         string
	KeyOverrides    map[string]string
	SecretOverrides map[string]string
}

// liftBoardLaunchContext splits a card's BotArgs into the bot's vars and the
// reserved launch context (repo + overrides), removing the reserved keys from
// the vars so they never leak to the bot. A malformed override blob is an
// error: silently dropping it would launch the run WITHOUT the webhook's
// BYOK key / secret overrides, so the failure must surface instead.
func liftBoardLaunchContext(botArgs map[string]string) (boardLaunchContext, error) {
	lc := boardLaunchContext{
		RepoURL: botArgs[boardRepoURLKey],
		RepoRef: botArgs[boardRepoRefKey],
	}
	if blob := botArgs[boardKeyOverridesKey]; blob != "" {
		if err := json.Unmarshal([]byte(blob), &lc.KeyOverrides); err != nil {
			return boardLaunchContext{}, fmt.Errorf("malformed %s bot-arg (edit the card's bot args to fix): %w", boardKeyOverridesKey, err)
		}
	}
	if blob := botArgs[boardSecretOverridesKey]; blob != "" {
		if err := json.Unmarshal([]byte(blob), &lc.SecretOverrides); err != nil {
			return boardLaunchContext{}, fmt.Errorf("malformed %s bot-arg (edit the card's bot args to fix): %w", boardSecretOverridesKey, err)
		}
	}
	lc.Vars = make(map[string]string, len(botArgs))
	for k, v := range botArgs {
		switch k {
		case boardRepoURLKey, boardRepoRefKey, boardKeyOverridesKey, boardSecretOverridesKey:
			continue
		}
		lc.Vars[k] = v
	}
	return lc, nil
}

// stampCardLastRun records the launched run on the tenant's board card via the
// same SetLastRun seam the local dispatcher uses, resolved through CloudBoardFor
// (the Mongo-backed store in cloud, a native store in tests). Best-effort: a
// stamp failure never fails the run — the card simply lacks its live-run link.
func (s *Server) stampCardLastRun(tenant, cardID, runID string) {
	if s.cfg.CloudBoardFor == nil || runID == "" {
		return
	}
	store := s.cfg.CloudBoardFor(tenant)
	if store == nil {
		return
	}
	if err := store.SetLastRun(cardID, runID, ""); err != nil && s.logger != nil {
		s.logger.Warn("board dispatcher: stamp run %s on card %s/%s: %v", runID, tenant, cardID, err)
	}
}

// setCardAwaitingInput denormalizes the pause hint onto the tenant's board
// card via the same CloudBoardFor seam as stampCardLastRun, so the studio
// grid can badge a paused card without an N+1 run fetch. Best-effort: a
// write failure never fails the run. It is a HINT — the modal's answer
// affordance still keys off getRun(last_run_id).status.
func (s *Server) setCardAwaitingInput(tenant, cardID string, v bool) {
	if s.cfg.CloudBoardFor == nil {
		return
	}
	store := s.cfg.CloudBoardFor(tenant)
	if store == nil {
		return
	}
	if err := store.SetAwaitingInput(cardID, v); err != nil && s.logger != nil {
		s.logger.Warn("board dispatcher: set awaiting-input=%v on card %s/%s: %v", v, tenant, cardID, err)
	}
}

// boardRunStatus reads a run's persisted status for the parked-card sweep,
// tenant-scoped exactly like processBoardCard's launch context.
func (s *Server) boardRunStatus(ctx context.Context, tenant, runID string) (store.RunStatus, error) {
	if s.runs == nil {
		return "", errors.New("run service unavailable")
	}
	ctx = store.WithIdentity(ctx, tenant, "board-dispatcher")
	run, err := s.runs.LoadRunCtx(ctx, runID)
	if err != nil {
		return "", err
	}
	return run.Status, nil
}

func (s *Server) processBoardCard(ctx context.Context, tenant string, iss native.Issue) error {
	if s.runs == nil {
		return errors.New("run service unavailable")
	}
	if iss.Bot == "" {
		return fmt.Errorf("card %s has no bot", iss.ID)
	}
	ctx = store.WithIdentity(ctx, tenant, "board-dispatcher")
	path, source, err := s.resolveBotSource(iss.Bot)
	if err != nil {
		return err
	}
	// A webhook-launched card carries its launch context (repo + the webhook's
	// BYOK key / secret overrides) in reserved BotArgs keys (ensureBoardCard) —
	// the coordinator otherwise has none of it. Lift it into the LaunchSpec so
	// the runner clones + the publisher applies the overrides, and strip the
	// reserved keys from the bot's vars.
	lc, err := liftBoardLaunchContext(iss.BotArgs)
	if err != nil {
		return fmt.Errorf("card %s: %w", iss.ID, err)
	}
	// A card that targets a pull request also needs the repo's launch policy
	// and a publish grant, neither of which can ride the card itself.
	lc.Vars = s.applyPRLaunchContext(ctx, tenant, "", iss.Bot, lc.Vars, nil)
	res, err := s.runs.Launch(ctx, runview.LaunchSpec{
		FilePath:        path,
		Source:          source,
		BotID:           iss.Bot,
		Vars:            lc.Vars,
		RepoURL:         lc.RepoURL,
		RepoRef:         lc.RepoRef,
		KeyOverrides:    lc.KeyOverrides,
		SecretOverrides: lc.SecretOverrides,
	})
	if err != nil {
		return err
	}
	runID := res.RunID
	// Stamp the launched run onto the card immediately (not after the run
	// terminates) so the studio can link the LIVE run while it executes. The
	// local dispatcher already does this via SetLastRun; the cloud coordinator
	// launches through runs.Launch and must stamp the same seam itself.
	s.stampCardLastRun(tenant, iss.ID, runID)
	// A fresh dispatch supersedes any prior pause — clear the denormalized
	// awaiting-input badge so the card doesn't show a stale ⏸.
	s.setCardAwaitingInput(tenant, iss.ID, false)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		if run, lerr := s.runs.LoadRunCtx(ctx, runID); lerr == nil {
			switch st := run.Status; {
			case st == store.RunStatusFinished:
				return nil
			case st.IsTerminal():
				return fmt.Errorf("run %s ended %s", runID, st)
			case st.IsPaused():
				// Parked on a human/operator gate — stop waiting; the operator
				// resumes the run. Denormalize the pause hint so the grid can
				// badge the card without a per-run fetch, and wrap errCardPaused
				// so processCard routes the card to the awaiting-input column
				// instead of blocked (a pause is not a failure).
				s.setCardAwaitingInput(tenant, iss.ID, true)
				return fmt.Errorf("run %s paused (%s): %w", runID, st, errCardPaused)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
