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

	missing := map[string]int{}
	cursor := ""
	for {
		page, err := bc.ListProjectItems(ctx, ref, forge.ProjectItemListOptions{Cursor: cursor})
		if err != nil {
			return res, fmt.Errorf("project import: list items of %s: %w", ref, err)
		}
		for _, it := range page.Items {
			if err := applyProjectItem(ctx, bc, project, ref, provider, board, it, opts, &res, missing); err != nil {
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
) error {
	res.Items++
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
	statusName, statusAt := projectStatusValue(it)

	patch := native.Patch{}
	if labels, changed := applyProjectLabels(project, card.Labels, it, opts); changed {
		patch.Labels = &labels
		res.Labelled++
	}

	nativeAt := nativeStateAt(card, sync)
	targetState, decision := decideProjectStatus(statusName, statusAt, nativeAt, sync, opts)
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
	// would push nothing. The reflect overwrites these two fields itself with
	// what it actually wrote, which is what makes the next pass a no-op.
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
		reflectNativeState(ctx, bc, card, it, &sync, statusName, opts, res)
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
func reflectNativeState(
	ctx context.Context,
	bc forge.BoardClient,
	card *native.Issue,
	it forge.ProjectItem,
	sync *native.ExternalProject,
	boardStatus string,
	opts *ProjectImportOptions,
	res *ProjectImportResult,
) {
	binding := opts.binding()
	if binding == nil {
		return // read-only pass: no write authority
	}
	if sync.Status == "" {
		// First sight of this card on this board: the board is the authority
		// on the join, and the import already applied it. Pushing here would
		// overwrite a column nobody has reconciled yet.
		return
	}
	want, ok := forge.StatusForState(opts.statusMapping(), card.State)
	if !ok {
		// An unmapped native state (`review`, `waiting_deps`, …) is INERT: the
		// board keeps showing the last true thing it was told.
		return
	}
	if strings.EqualFold(strings.TrimSpace(want), strings.TrimSpace(boardStatus)) {
		return // already there — the idempotence that keeps a pass free
	}
	option, ok := binding.OptionForState(card.State)
	if !ok {
		logProjectWarn(opts, "project reflect: the bound board has no column for this state",
			"card", card.ID, "state", card.State, "status", want)
		return
	}
	if binding.ProjectID == "" || binding.StatusFieldID == "" {
		logProjectWarn(opts, "project reflect: the binding carries no project/status id",
			"card", card.ID, "state", card.State)
		return
	}
	if err := bc.SetSingleSelect(ctx, binding.ProjectID, it.ID, binding.StatusFieldID, option); err != nil {
		res.ReflectFailed++
		logProjectWarn(opts, "project reflect: status write refused",
			"card", card.ID, "item", it.ID, "state", card.State, "status", want, "error", err.Error())
		return
	}
	res.Reflected++
	// Record what we just wrote, so the next pass reads "already equal" and
	// does nothing — without this the reflect rewrites on every tick, burning
	// the API budget and stamping a fresh updatedAt that then wins every
	// subsequent conflict against the operator.
	sync.Status = want
	sync.StatusAt = opts.now()
	sync.StateAt = opts.now()
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
// transition time (nativeStateAt).
func decideProjectStatus(statusName string, statusAt, nativeAt time.Time, sync native.ExternalProject, opts *ProjectImportOptions) (string, projectStatusDecision) {
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
	// 2/3. Both sides moved. Newer wins; a tie goes to the board, which is
	// what a human is looking at.
	if nativeAt.After(statusAt) {
		return "", projectStatusConflictNative
	}
	return state, projectStatusConflictGitHub
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
