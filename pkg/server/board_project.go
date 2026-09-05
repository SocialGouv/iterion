package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The forge PROJECT board → native board import (ADR-097).
//
// It is the second half of the forge→board sync, and deliberately not folded
// into the first: `syncForgeIssuesToBoard` mirrors a REPO's issues (title,
// body, labels, assignees, state) while this mirrors a cross-repo PROJECT's
// fields (Status → column, Area/Mode/Priority → labels). They join on the
// card id, which both derive from `<provider>:<repo>#<number>`.
//
// It never CREATES a card. A project item carries no body, no labels, no
// author — so a card built from one would be degraded, and worse, would bypass
// the author-trust gate that runs at issue ingest and decides whether a card
// may spend LLM budget at all. Hydrating cards the issue import created keeps
// that boundary in exactly one place.

// ProjectImportOptions tunes one project pass. The zero value is a READ-ONLY
// import with the shipped vocabulary; supplying a Binding turns on the reflect
// half, making the pass two-way.
type ProjectImportOptions struct {
	// Binding, when set, gives the pass WRITE authority on the board: a card
	// that moved natively while the board still says what iterion last
	// recorded has its Status field updated. Nil = read-only.
	//
	// It is also where the option ids come from, so nothing is discovered
	// per-card. Its StatusMapping/LabelFields win over the fields below.
	Binding *forge.BoardBinding
	// Bindings persists a Binding the pass had to REPAIR against the live
	// board — a renamed or re-created column — and its degraded readout. Nil
	// keeps the repair in memory for this pass only, which is what the local
	// one-shot `iterion issue import --project` wants: it converges, it just
	// does not remember.
	Bindings forge.BoardBindingStore
	// StatusMapping overrides the (board status ⇄ native state) pairs.
	StatusMapping []forge.StatusMapping
	// LabelFields overrides which single-select fields land as labels.
	LabelFields []forge.LabelField
	// Now is the clock, injected for tests.
	Now func() time.Time
	// Logger receives one Warn per resolved conflict — a silent overwrite of
	// somebody's decision is the one outcome that must never be invisible.
	Logger *iterlog.Logger
}

func (o *ProjectImportOptions) statusMapping() []forge.StatusMapping {
	if o == nil {
		return forge.DefaultStatusMapping()
	}
	if len(o.StatusMapping) > 0 {
		return o.StatusMapping
	}
	if o.Binding != nil {
		return o.Binding.Mapping()
	}
	return forge.DefaultStatusMapping()
}

func (o *ProjectImportOptions) labelFields() []forge.LabelField {
	if o == nil {
		return forge.DefaultLabelFields()
	}
	if len(o.LabelFields) > 0 {
		return o.LabelFields
	}
	if o.Binding != nil {
		return o.Binding.Fields()
	}
	return forge.DefaultLabelFields()
}

// binding returns the write-authority binding, or nil for a read-only pass.
func (o *ProjectImportOptions) binding() *forge.BoardBinding {
	if o == nil {
		return nil
	}
	return o.Binding
}

// bindingStore returns where a repaired binding is persisted, or nil.
func (o *ProjectImportOptions) bindingStore() forge.BoardBindingStore {
	if o == nil {
		return nil
	}
	return o.Bindings
}

func (o *ProjectImportOptions) now() time.Time {
	if o != nil && o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

// ProjectImportResult counts what one import did. Every item is accounted for
// in exactly one bucket, so "nothing happened" is always explainable.
type ProjectImportResult struct {
	// Items is every item the board returned.
	Items int `json:"items"`
	// Moved is the cards whose column the board's Status changed.
	Moved int `json:"moved"`
	// Reflected is the board items whose Status a native move updated — the
	// other direction of the same pass.
	Reflected int `json:"reflected"`
	// ReflectNoColumn is the cards whose native state the bound board has no
	// column for — a mapped column the board never carried (reported as
	// `missing_statuses` at bind time), or one deleted since (which degrades
	// the binding, once). Counted rather than warned per card: the same cards
	// are unreflectable on every pass until the binding is repaired, and a
	// per-card Warn on a 300-item board buries everything else.
	ReflectNoColumn int `json:"reflect_no_column,omitempty"`
	// ReflectFailed is the reflects the forge refused. Counted rather than
	// fatal: one card's failed write must not abandon the rest of the board.
	ReflectFailed int `json:"reflect_failed,omitempty"`
	// Labelled is the cards whose project-derived labels changed.
	Labelled int `json:"labelled"`
	// Conflicts is the items where both sides had moved since the last sync.
	// Counted whichever side won — the number an operator needs is "how often
	// are people and bots fighting over this board".
	Conflicts int `json:"conflicts"`
	// SkippedNoCard is items backed by an issue with no native card yet: run
	// the issue import for that repo first.
	SkippedNoCard int `json:"skipped_no_card"`
	// MissingRepos breaks SkippedNoCard down by repository, most-missing
	// first. A bare count tells an operator nothing they can act on; the
	// repository names ARE the next command they have to run.
	MissingRepos []MissingRepo `json:"missing_repos,omitempty"`
	// RefusedTerminal is the moves the native board's terminal-state sink
	// refused. Leaving done/blocked is a REOPEN — an operator gesture with its
	// own audit trail and dependents check — and automation never reopens. The
	// two boards stay legitimately divergent until someone reopens the card.
	RefusedTerminal int `json:"refused_terminal,omitempty"`
	// SkippedArchived is the items the operator archived. The forge removes
	// them from every board view but PRESERVES their field values, so one
	// archived mid-column keeps reading as that column forever — driving a
	// card from a value nobody can see, and reflecting onto a row nobody can
	// read. Counted rather than silent: a card that stops following has to be
	// explainable from the pass's own numbers.
	SkippedArchived int `json:"skipped_archived,omitempty"`
	// Skipped is items with nothing to join on — drafts and pull requests.
	Skipped int `json:"skipped"`
}

// MissingRepo is one repository the board references and the native board has
// no cards for, with how many of its items were skipped.
type MissingRepo struct {
	Repo  string `json:"repo"`
	Count int    `json:"count"`
}

// ImportProjectBoard mirrors a forge project board's Status and label fields
// onto the native cards its items are backed by.
//
// Status is applied under the ADR-097 conflict rule: an unchanged board status
// changes nothing (the echo suppressor), and when both sides moved the newer
// state change wins with ties going to the board.
func ImportProjectBoard(
	ctx context.Context,
	bc forge.BoardClient,
	ref forge.ProjectRef,
	provider forge.Provider,
	board native.BoardStore,
	opts *ProjectImportOptions,
) (ProjectImportResult, error) {
	var res ProjectImportResult
	if bc == nil {
		return res, fmt.Errorf("project import: no board client")
	}
	if err := ref.Validate(); err != nil {
		return res, err
	}
	project, err := bc.GetProject(ctx, ref)
	if err != nil {
		return res, fmt.Errorf("project import: get project %s: %w", ref, err)
	}
	repair := repairBinding(ctx, project, opts)

	missing := map[string]int{}
	cursor := ""
	for {
		page, err := bc.ListProjectItems(ctx, ref, forge.ProjectItemListOptions{Cursor: cursor})
		if err != nil {
			return res, fmt.Errorf("project import: list items of %s: %w", ref, err)
		}
		for _, it := range page.Items {
			if err := applyProjectItem(ctx, bc, project, ref, provider, board, it, opts, &res, missing, repair); err != nil {
				res.MissingRepos = rankMissingRepos(missing)
				return res, err
			}
		}
		if !page.HasNext || page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	res.MissingRepos = rankMissingRepos(missing)
	return res, nil
}

// repairBinding re-resolves the binding's cached status vocabulary — the
// (state → option id) map and the column NAMES those ids carried — against the
// board schema this pass has already read, and returns the renames it found.
//
// It costs NO extra API call: the pass reads the project for its label fields
// anyway. That is what makes it the right place. The alternative — discovering
// the drift on a failed write — discovers it once per card per pass and never
// repairs it, which is how a renamed column produced one redundant mutation per
// bound card, on every pass, indefinitely.
//
// Persisting is best-effort: a store outage must not abandon the reconciliation
// the operator is watching. The repair still applies in memory for this pass.
//
// The health readout is decided here too, and from the binding's CURRENT shape
// rather than from what this pass happened to observe. A flag derived from an
// EVENT ("we noticed the column go") only holds while the evidence survives:
// the first pass reports the loss, and every pass after it — reading a binding
// where nothing changed — reports nothing, so the next unrelated repair reads
// as "it resolves again" and clears a degradation that is still true. Level,
// not edge: `reason` is recomputed each pass, and only an EMPTY one clears.
func repairBinding(ctx context.Context, project forge.Project, opts *ProjectImportOptions) bindingRepair {
	binding := opts.binding()
	if binding == nil {
		return bindingRepair{} // read-only pass: nothing is cached, so nothing is stale
	}
	if binding.StatusFieldID == "" {
		// A labels-only binding has no status vocabulary, so this pass
		// re-derived NOTHING about it. `ReconcileStatusOptions` would hand back
		// the zero repair, whose empty `Reason()` is indistinguishable from
		// "every column resolves" — and the clear arm below would act on it.
		// Nothing reaches here degraded today, but the switch must never be
		// able to clear a flag on the absence of the evidence that would have
		// kept it: that is the whole thesis of the level-triggered readout.
		return bindingRepair{}
	}
	was := binding.DegradedReason
	rep := binding.ReconcileStatusOptions(project)
	store := opts.bindingStore()
	if rep.Changed() && store != nil {
		if err := store.SaveStatusVocabulary(ctx, binding.TenantID, binding.Vocabulary()); err != nil {
			logProjectWarn(opts, "project board: the repaired status vocabulary could not be persisted",
				"team", binding.TenantID, "error", err.Error())
		}
	}

	switch reason := rep.Reason(); {
	case reason != "":
		binding.DegradedReason = reason
		if was == reason {
			break // unchanged standing state: no store write, no second Warn
		}
		// ONCE, on the transition. The state itself lives on the binding
		// (GET /api/teams/{id}/board-binding), which is where an operator can
		// act on it — a Warn repeated every two minutes is not a signal, it is
		// noise that buries the pass's real lines.
		logProjectWarn(opts, "project board: a bound status column no longer exists on the board",
			"team", binding.TenantID, "board", binding.Ref().String(), "reason", reason)
		if store != nil {
			if err := store.MarkDegraded(ctx, binding.TenantID, reason); err != nil {
				logProjectWarn(opts, "project board: the binding could not be flagged degraded",
					"team", binding.TenantID, "error", err.Error())
			}
		}
	case was != "":
		// Nothing is lost any more — every mapped column the binding resolved
		// answers again. Only THAT clears the flag; an unrelated adoption
		// elsewhere on the board never did mean the missing column came back.
		binding.DegradedReason, binding.DegradedAt = "", nil
		if store != nil {
			if err := store.ClearDegraded(ctx, binding.TenantID); err != nil {
				logProjectWarn(opts, "project board: the binding's degraded flag could not be cleared",
					"team", binding.TenantID, "error", err.Error())
			}
		}
	}
	return bindingRepair{renamed: rep.Renames(), lost: rep.LostStates()}
}

// bindingRepair is what one pass's reconciliation tells the item loop: the
// column renames a card's recorded status must be read through, and the
// columns no card can be reflected onto.
type bindingRepair struct {
	renamed map[string]string
	lost    map[string]bool
}

// rename resolves a recorded status name through this pass's renames.
func (r bindingRepair) rename(status string) (string, bool) {
	if len(r.renamed) == 0 {
		return "", false
	}
	to, ok := r.renamed[strings.ToLower(strings.TrimSpace(status))]
	return to, ok
}

// lostState reports whether the board carries no column for this native state.
func (r bindingRepair) lostState(state string) bool { return r.lost[state] }

// rankMissingRepos orders the skipped repositories most-missing first, ties
// broken by name, so the operator's first move is the first line.
func rankMissingRepos(missing map[string]int) []MissingRepo {
	if len(missing) == 0 {
		return nil
	}
	out := make([]MissingRepo, 0, len(missing))
	for repo, n := range missing {
		out = append(out, MissingRepo{Repo: repo, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Repo < out[j].Repo
	})
	return out
}

// applyProjectItem reconciles ONE card with ONE project item, in both
// directions, accumulating into res. An item that cannot be joined is counted,
// never guessed at.
//
// It returns an error only for a failure of the CARD STORE itself, which is
// not this item's problem but the whole pass's: reading a store outage as
// per-item noise would report a board of hundreds of items as "never imported"
// and hide the database. Per-item forge failures stay counted and logged.
func applyProjectItem(
	ctx context.Context,
	bc forge.BoardClient,
	project forge.Project,
	ref forge.ProjectRef,
	provider forge.Provider,
	board native.BoardStore,
	it forge.ProjectItem,
	opts *ProjectImportOptions,
	res *ProjectImportResult,
	missing map[string]int,
	repair bindingRepair,
) error {
	res.Items++
	if it.Archived {
		// The operator took this item off the board. Its field values survive
		// archiving, so both directions would keep working on a row nobody can
		// see: the import would drive the card from a frozen column, and the
		// reflect would push into a view that renders nothing.
		res.SkippedArchived++
		return nil
	}
	if it.Content.Kind != forge.ProjectContentIssue || it.Content.Repo == "" || it.Content.Number <= 0 {
		// A draft has no issue; a pull request surfaces through the card's PR
		// panel, not as a card of its own.
		res.Skipped++
		return nil
	}
	cardID := forgeCardID(provider, it.Content.Repo, it.Content.Number)
	card, err := board.Get(cardID)
	switch {
	case errors.Is(err, tracker.ErrNotFound) || (err == nil && card == nil):
		// The item's issue has no card yet — the ONE reading that means "run
		// the issue import for this repo". Both store twins answer a missing
		// card with this sentinel; the Mongo one wraps everything else.
		res.SkippedNoCard++
		missing[it.Content.Repo]++
		return nil
	case err != nil:
		return fmt.Errorf("project import: read card %s: %w", cardID, err)
	}

	sync := projectSyncState(card, ref, it)
	// A card's recorded status is a column NAME, so a rename repaired above
	// leaves every card of that column naming something the vocabulary no
	// longer knows. Reading the record through the rename keeps ONE operator
	// edit from reading as "the board moved to an unknown column" on every
	// card of that column at once.
	if to, ok := repair.rename(sync.Status); ok {
		sync.Status = to
	}
	statusName, statusAt := projectStatusValue(it)

	patch := native.Patch{}
	if labels, changed := applyProjectLabels(project, card.Labels, it, opts); changed {
		patch.Labels = &labels
		res.Labelled++
	}

	nativeAt := nativeStateAt(card, sync)
	targetState, decision := decideProjectStatus(statusName, statusAt, nativeAt, card.State, sync, opts)
	switch decision {
	case projectStatusConflictGitHub, projectStatusConflictNative:
		res.Conflicts++
		logProjectConflict(opts, cardID, it, statusName, statusAt, nativeAt, sync, decision)
	}

	applied, refused := false, false
	if targetState != "" && targetState != card.State {
		// CAS on the snapshot: an operator who moved the card between our read
		// and this write wins — the board must not clobber a fresh decision
		// with a fact it read a moment ago.
		//
		// Losing that CAS is NOT an error: SetStateFrom answers a drifted card
		// with (issue, changed=false, nil). Reading only the error counted a
		// transition the store never made and recorded the board's status as
		// synchronized, which turns every later pass into a no-op — and on the
		// one-shot `iterion issue import --project` path nothing ever repairs
		// it. A refused write and a lost CAS are the same fact here: nothing
		// landed.
		switch _, changed, err := board.SetStateFrom(cardID, card.State, targetState); {
		case err != nil:
			refused = true
			if errors.Is(err, tracker.ErrTerminalStateExit) {
				res.RefusedTerminal++
			}
			logProjectWarn(opts, "project import: state write refused", "card", cardID,
				"from", card.State, "to", targetState, "error", err.Error())
		case !changed:
			refused = true
			logProjectWarn(opts, "project import: state write skipped, the card moved first",
				"card", cardID, "from", card.State, "to", targetState)
		default:
			applied = true
			res.Moved++
		}
	}

	// The sync state records what the board said, and — when we acted on it —
	// when the native state changed.
	//
	// A native-wins conflict is the exception: recording the board's status
	// here would tell the reflect below "the board already agrees", and it
	// would push nothing. The reflect writes these two fields itself with what
	// it actually wrote — and when it writes NOTHING the caller closes them
	// with what it observed, so the divergence is derived once rather than on
	// every tick (see the reflect call below).
	//
	// A REFUSED write is the other exception, and the sharper one: recording a
	// status iterion could not apply makes the NEXT pass read "the board
	// already agrees", which fires the reflect and writes the card's own column
	// back onto the board — silently undoing the operator's move. Declining the
	// record keeps every pass re-deriving the same divergence, which is the
	// documented outcome for a terminal card (the two boards stay divergent
	// until someone reopens it).
	if decision != projectStatusConflictNative && !refused {
		sync.Status = statusName
		sync.StatusAt = statusAt
	}
	if applied {
		sync.StateAt = opts.now()
	}

	// The OTHER direction, on the same pass and the same board read. It runs
	// in exactly the two cases where the native side holds the truth:
	//
	//   - projectStatusNoop — the board still says what we recorded, so any
	//     divergence from the card's column is a native move;
	//   - projectStatusConflictNative — both moved and iterion's move is
	//     NEWER, which is precisely what the reflect exists for. Skipping it
	//     here left the two boards divergent forever: the branch also declines
	//     to advance the recorded status, so the next pass recomputed the same
	//     inputs and re-derived the same conflict, warning on every tick and
	//     changing nothing on either side.
	//
	// It does NOT run when the board won (`applied`, or a GitHub-wins
	// conflict): pushing there would overwrite the decision we just read.
	if !applied && (decision == projectStatusNoop || decision == projectStatusConflictNative) {
		// A native-wins conflict declined to record the board's status above,
		// on the promise that the reflect overwrites it with what it WROTE.
		// The reflect has exits that write nothing — the two sides already
		// agree, the native state is unmapped, the bound board has no column
		// for it, the pass is read-only — and there the promise is unkept: the
		// record stays stale, the next pass recomputes identical inputs and
		// re-derives the same conflict, warning and counting on every tick for
		// a divergence nothing is going to resolve. Recording what was
		// OBSERVED closes it, and is not a false claim: the board says this
		// value now, and it is exactly what "the status last synchronized"
		// means. A FAILED write is the exception — the next pass must retry it.
		if out := reflectNativeState(ctx, bc, card, it, &sync, statusName, opts, res, repair); out == reflectNothingToWrite &&
			decision == projectStatusConflictNative {
			sync.Status = statusName
			sync.StatusAt = statusAt
		}
	}
	// Write ONLY what changed. This pass runs over every card on its interval,
	// and native.Store.Update treats a non-nil Patch.External as a change with
	// no equality check of its own: rewriting unconditionally bumped UpdatedAt
	// on every card every tick and emitted an EvtIssueUpdated — which the
	// trigger spine consumes as `card.updated`, relaunching every
	// label-matching board subscription. A quiet pass has to be silent.
	if card.External == nil || !card.External.Project.Equal(sync) {
		ext := card.External.Clone()
		if ext == nil {
			ext = &native.ExternalRef{
				Provider: string(provider), Repo: it.Content.Repo,
				Number: it.Content.Number, URL: it.Content.URL, State: it.Content.State,
			}
		}
		ext.Project = &sync
		patch.External = ext
	}
	if patch.Labels == nil && patch.External == nil {
		return nil
	}
	if _, err := board.Update(cardID, patch); err != nil {
		logProjectWarn(opts, "project import: card update failed", "card", cardID, "error", err.Error())
	}
	return nil
}

// reflectNativeState pushes a native move onto the board (ADR-097 §2, the
// second direction).
//
// The precondition it rests on is the caller's: the import decided NOTHING to
// do, which means the board's status is exactly what iterion last recorded —
// so nobody but iterion put it there, and a divergence from the card's current
// column can only be a native move. That single comparison is both the
// "who moved?" oracle and the echo suppressor, which is why neither direction
// needs a per-write "last mover" flag.
//
// A failed write is counted and logged, never fatal: one card's 403 must not
// abandon the rest of the board.
//
// The outcome is the caller's business: it is what tells a native-wins
// conflict whether the record it declined to write was overwritten here, or
// has to be closed by the caller instead.
func reflectNativeState(
	ctx context.Context,
	bc forge.BoardClient,
	card *native.Issue,
	it forge.ProjectItem,
	sync *native.ExternalProject,
	boardStatus string,
	opts *ProjectImportOptions,
	res *ProjectImportResult,
	repair bindingRepair,
) reflectOutcome {
	binding := opts.binding()
	if binding == nil {
		return reflectNothingToWrite // read-only pass: no write authority
	}
	if sync.Status == "" {
		// First sight of this card on this board: the board is the authority
		// on the join, and the import already applied it. Pushing here would
		// overwrite a column nobody has reconciled yet. Only reachable from
		// the no-op arm, which recorded the status already.
		return reflectNothingToWrite
	}
	want, ok := forge.StatusForState(opts.statusMapping(), card.State)
	if !ok {
		// An unmapped native state (`review`, `waiting_deps`, …) is INERT: the
		// board keeps showing the last true thing it was told.
		return reflectNothingToWrite
	}
	if strings.EqualFold(strings.TrimSpace(want), strings.TrimSpace(boardStatus)) {
		return reflectNothingToWrite // already there — the idempotence that keeps a pass free
	}
	option, ok := binding.OptionForState(card.State)
	if !ok || repair.lostState(card.State) {
		// No column for this state: one the map named and the board never had
		// (reported as `missing_statuses` at bind time), or one deleted since —
		// which this pass's reconciliation flagged on the binding, once. The
		// second case still HAS a cached id (kept as the evidence that keeps
		// the degradation re-derivable), so the lost set is what refuses it;
		// writing that id would 422 on every card of that column, every pass.
		// Counted, not warned: these cards are unreflectable on EVERY pass
		// until the binding is repaired, and a per-card Warn buries the rest.
		res.ReflectNoColumn++
		return reflectNothingToWrite
	}
	if binding.ProjectID == "" || binding.StatusFieldID == "" {
		logProjectWarn(opts, "project reflect: the binding carries no project/status id",
			"card", card.ID, "state", card.State)
		return reflectNothingToWrite
	}
	if fv, ok := it.Field(forge.ProjectStatusFieldName); ok && fv.OptionID != "" && fv.OptionID == option {
		// The board already carries the very OPTION we would write; only its
		// NAME differs from what the binding cached. Ids are the write
		// vocabulary, so this is the authoritative "already there" — the name
		// comparison above is the fallback for a provider that reports no
		// option id, and on its own it re-writes the same value forever after
		// a column is renamed.
		return reflectNothingToWrite
	}
	if err := bc.SetSingleSelect(ctx, binding.ProjectID, it.ID, binding.StatusFieldID, option); err != nil {
		res.ReflectFailed++
		logProjectWarn(opts, "project reflect: status write refused",
			"card", card.ID, "item", it.ID, "state", card.State, "status", want, "error", err.Error())
		return reflectFailed
	}
	res.Reflected++
	// Record what we just wrote, so the next pass reads "already equal" and
	// does nothing — without this the reflect rewrites on every tick, burning
	// the API budget and stamping a fresh updatedAt that then wins every
	// subsequent conflict against the operator.
	sync.Status = want
	sync.StatusAt = opts.now()
	sync.StateAt = opts.now()
	return reflectWrote
}

// projectSyncState reads the card's recorded sync state, resetting it when the
// card was last synced against a DIFFERENT board — a re-binding must not
// inherit the previous board's status as if it were this one's.
func projectSyncState(card *native.Issue, ref forge.ProjectRef, it forge.ProjectItem) native.ExternalProject {
	sync := native.ExternalProject{Owner: ref.Owner, Number: ref.Number, ItemID: it.ID}
	if card.External == nil || card.External.Project == nil {
		return sync
	}
	prev := *card.External.Project
	if !strings.EqualFold(prev.Owner, ref.Owner) || prev.Number != ref.Number {
		return sync
	}
	prev.ItemID = it.ID
	return prev
}

// projectStatusValue reads the item's Status field value and the board's own
// timestamp for it.
func projectStatusValue(it forge.ProjectItem) (string, time.Time) {
	fv, ok := it.Field(forge.ProjectStatusFieldName)
	if !ok {
		return "", time.Time{}
	}
	return fv.Value, fv.UpdatedAt
}

// reflectOutcome says what one reflect did to the board, which is what decides
// whether the caller still owes the sync record an update.
type reflectOutcome int

const (
	// reflectNothingToWrite: there was nothing to push, or nothing that COULD
	// be pushed. Either way the board's status will not change on this pass,
	// so a divergence left open here would be re-derived forever.
	reflectNothingToWrite reflectOutcome = iota
	// reflectWrote: the board's Status now says what the card's column means,
	// and the reflect recorded it itself.
	reflectWrote
	// reflectFailed: the forge refused the write. The record stays stale ON
	// PURPOSE — that is what makes the next pass retry.
	reflectFailed
)

type projectStatusDecision int

const (
	// projectStatusNoop: no status, unmapped status, or the board says exactly
	// what we last recorded (the echo suppressor).
	projectStatusNoop projectStatusDecision = iota
	// projectStatusApply: first sight of this card on this board, or the board
	// moved and iterion has not.
	projectStatusApply
	projectStatusConflictGitHub
	projectStatusConflictNative
)

// nativeStateAt is when the card's column last changed — the value the "newer
// state change wins" rule has to compare, and deliberately not the card's
// UpdatedAt (a retitle would win a status conflict) nor the sync record's
// StateAt (only THIS package writes it, so a move made in the studio, by the
// dispatcher or through the board MCP tool was invisible and lost every
// conflict inside one interval).
//
// The fallback is for a card whose last transition predates the store stamping
// one: the only transition time iterion has for it is when it last wrote the
// state itself, which is exactly what the rule used to read. Naming the
// fallback keeps a legacy card's behaviour unchanged instead of silently
// re-dating it to the zero time, where the board would win every conflict.
func nativeStateAt(card *native.Issue, sync native.ExternalProject) time.Time {
	if card != nil && !card.StateAt.IsZero() {
		return card.StateAt
	}
	return sync.StateAt
}

// decideProjectStatus resolves ADR-097's conflict rule. It returns the native
// state to write ("" = write nothing) and why. nativeAt is the card's own
// transition time (nativeStateAt); cardState is the column it sits in now.
func decideProjectStatus(statusName string, statusAt, nativeAt time.Time, cardState string, sync native.ExternalProject, opts *ProjectImportOptions) (string, projectStatusDecision) {
	if strings.TrimSpace(statusName) == "" {
		return "", projectStatusNoop
	}
	// 1. Value already equal ⇒ nothing happens. Checked FIRST, and against the
	// recorded status rather than the card's column: a card that has since
	// moved on must keep its move, not be dragged back by the value that
	// caused it.
	if strings.EqualFold(strings.TrimSpace(sync.Status), strings.TrimSpace(statusName)) {
		return "", projectStatusNoop
	}
	state, ok := forge.StateForStatus(opts.statusMapping(), statusName)
	if !ok {
		// An unmapped board status is inert, exactly like an unmapped native
		// state on the way out.
		return "", projectStatusNoop
	}
	if sync.Status == "" {
		// First sight: the board is the authority on the join.
		return state, projectStatusApply
	}
	// 2. Only the BOARD moved — the ordinary gesture on a forge board. It is a
	// plain apply, never a conflict: the counter means "both sides moved"
	// (ADR-097 §9.2), and a conflict resolved in the native side's favour
	// makes the reflect push the card's old column back over the drag.
	if !nativeMovedSince(sync, cardState, opts) {
		return state, projectStatusApply
	}
	// 3/4. Both sides moved. Newer wins; a tie goes to the board, which is
	// what a human is looking at.
	if nativeAt.After(statusAt) {
		return "", projectStatusConflictNative
	}
	return state, projectStatusConflictGitHub
}

// nativeMovedSince reports whether the card's column changed since the last
// synchronization with this board.
//
// The oracle is a FACT, not a timestamp comparison: sync.Status is the board
// status iterion last synchronized, so the native state it maps to IS
// iterion's own last write. A card still sitting there has not moved, and a
// timestamp-only rule cannot tell that from a card that moved and came back.
//
// A recorded status the mapping does not cover leaves the question
// undecidable — iterion never derived a state from it — and the answer is
// "assume it moved". A conflict that turns out one-sided costs one Warn; the
// reverse silently overwrites somebody's decision, which is the one outcome
// ADR-097 §9.4 forbids.
func nativeMovedSince(sync native.ExternalProject, cardState string, opts *ProjectImportOptions) bool {
	recorded, ok := forge.StateForStatus(opts.statusMapping(), sync.Status)
	if !ok {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(recorded), strings.TrimSpace(cardState))
}

// applyProjectLabels folds the item's label-bound field values into the card's
// labels, replacing any stale value in the same namespace. Returns the new
// label set and whether it differs.
func applyProjectLabels(project forge.Project, current []string, it forge.ProjectItem, opts *ProjectImportOptions) ([]string, bool) {
	fields := opts.labelFields()
	prefixes := make([]string, 0, len(fields))
	want := make([]string, 0, len(fields))
	for _, lf := range fields {
		if _, ok := project.Field(lf.Field); !ok {
			// The board does not carry this field: leave whatever the card has
			// alone rather than stripping labels on a board we cannot read.
			continue
		}
		prefixes = append(prefixes, lf.Prefix)
		fv, ok := it.Field(lf.Field)
		if !ok || strings.TrimSpace(fv.Value) == "" {
			continue // the field exists but this item has no value: clear it
		}
		if l := forge.FieldLabel(lf.Prefix, fv.Value); l != "" {
			want = append(want, l)
		}
	}
	if len(prefixes) == 0 {
		return nil, false
	}
	next := make([]string, 0, len(current)+len(want))
	for _, l := range current {
		if hasAnyPrefixFold(l, prefixes) {
			continue // owned by the project: re-derived below
		}
		next = append(next, l)
	}
	next = append(next, want...)
	if sameLabelSet(current, next) {
		return nil, false
	}
	return next, true
}

func hasAnyPrefixFold(label string, prefixes []string) bool {
	l := strings.ToLower(label)
	for _, p := range prefixes {
		if strings.HasPrefix(l, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// sameLabelSet compares two label sets ignoring order and case.
func sameLabelSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, l := range a {
		seen[strings.ToLower(l)]++
	}
	for _, l := range b {
		k := strings.ToLower(l)
		seen[k]--
		if seen[k] < 0 {
			return false
		}
	}
	return true
}

func logProjectConflict(opts *ProjectImportOptions, cardID string, it forge.ProjectItem, statusName string, statusAt, nativeAt time.Time, sync native.ExternalProject, d projectStatusDecision) {
	winner := "forge_board"
	if d == projectStatusConflictNative {
		winner = "native_board"
	}
	logProjectWarn(opts, "project import: both sides moved since the last sync",
		"card", cardID,
		"item", it.ID,
		"forge_status", statusName,
		"forge_status_at", statusAt.Format(time.RFC3339),
		"recorded_status", sync.Status,
		"native_state_at", nativeAt.Format(time.RFC3339),
		"winner", winner)
}

func logProjectWarn(opts *ProjectImportOptions, msg string, kv ...string) {
	if opts == nil || opts.Logger == nil {
		return
	}
	opts.Logger.Warn("%s %s", msg, joinKV(kv))
}

func joinKV(kv []string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%s", kv[i], kv[i+1])
	}
	return b.String()
}
