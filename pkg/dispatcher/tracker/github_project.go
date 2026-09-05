package tracker

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// Board mode for the GitHub tracker (ADR-097).
//
// Without it, an issue's workflow state is derived from its LABELS. With a
// project bound, the state comes from the board's Status field instead — the
// column a human actually drags cards between — and a dispatch decision stops
// depending on a parallel label convention nobody maintains.
//
// What does NOT change: the CLAIM. A Projects v2 item carries no marker and no
// epoch, so there is nothing to fence a lease with; the claim stays a label,
// and this adapter keeps declining ClaimLeaser exactly as it does today.
//
// Reads go through forge.BoardClient, never `gh`: an installation token from a
// forge.Connection is what a cloud pod has, and `gh` authenticates itself from
// its own config.

// GitHubProjectOptions binds the adapter to one Projects v2 board.
type GitHubProjectOptions struct {
	// Owner + Number identify the board ("owner/number" in its URL).
	Owner  string
	Number int
	// OwnerKind is "org" (default) or "user".
	OwnerKind forge.ProjectOwnerKind

	// Board is the project-board client. Required — board mode has no
	// fallback: a board that cannot be read fails the poll rather than
	// silently reverting to label-derived states, which would dispatch on a
	// state nobody configured.
	Board forge.BoardClient

	// CandidateStatuses are the board columns whose items are eligible for
	// dispatch. Empty defaults to the single "Planned" column — the column the
	// methodology means by "ready to be worked". Widen it to dispatch from
	// several columns; it is the operator's list, not a fence.
	CandidateStatuses []string

	// StatusMapping overrides the (board status ⇄ workflow state) pairs.
	// Empty uses forge.DefaultStatusMapping().
	StatusMapping []forge.StatusMapping
}

func (p *GitHubProjectOptions) validate() error {
	if strings.TrimSpace(p.Owner) == "" {
		return fmt.Errorf("github tracker: project owner is required")
	}
	if p.Number <= 0 {
		return fmt.Errorf("github tracker: project number must be positive, got %d", p.Number)
	}
	if p.Board == nil {
		return fmt.Errorf("github tracker: project mode needs a forge.BoardClient (board mode has no label fallback)")
	}
	if k := p.OwnerKind.OrDefault(); !k.Valid() {
		return fmt.Errorf("github tracker: project owner kind must be org or user, got %q", p.OwnerKind)
	}
	return nil
}

func (p *GitHubProjectOptions) ref() forge.ProjectRef {
	return forge.ProjectRef{Owner: p.Owner, OwnerKind: p.OwnerKind.OrDefault(), Number: p.Number}
}

func (p *GitHubProjectOptions) mapping() []forge.StatusMapping {
	if len(p.StatusMapping) > 0 {
		return p.StatusMapping
	}
	return forge.DefaultStatusMapping()
}

// defaultCandidateStatus is the column "eligible for dispatch" means when the
// operator has not narrowed it.
const defaultCandidateStatus = "Planned"

func (p *GitHubProjectOptions) candidateStatuses() []string {
	if len(p.CandidateStatuses) > 0 {
		return p.CandidateStatuses
	}
	return []string{defaultCandidateStatus}
}

// boardMode reports whether this adapter reads its states from a project.
func (a *GitHubAdapter) boardMode() bool { return a.opts.Project != nil }

// projectSnapshot is one read of the board: the Status field definition plus
// the items of THIS repo, keyed by issue number.
type projectSnapshot struct {
	statusField forge.ProjectField
	byNumber    map[int]forge.ProjectItem
}

// readProject fetches the board's Status schema and this repo's items.
//
// It is read fresh per call rather than cached: the dispatcher polls on the
// order of 30s, a board is a handful of GraphQL pages, and a cache would buy
// little at the cost of a staleness class in the one place a stale state means
// a wrong dispatch.
func (a *GitHubAdapter) readProject(ctx context.Context) (projectSnapshot, error) {
	p := a.opts.Project
	ref := p.ref()
	project, err := p.Board.GetProject(ctx, ref)
	if err != nil {
		return projectSnapshot{}, fmt.Errorf("github tracker: read project %s: %w", ref, err)
	}
	status, ok := project.Field(forge.ProjectStatusFieldName)
	if !ok {
		return projectSnapshot{}, fmt.Errorf("github tracker: project %s has no %q field — board mode has nothing to read a state from",
			ref, forge.ProjectStatusFieldName)
	}
	snap := projectSnapshot{statusField: status, byNumber: map[int]forge.ProjectItem{}}
	cursor := ""
	for {
		page, err := p.Board.ListProjectItems(ctx, ref, forge.ProjectItemListOptions{Cursor: cursor})
		if err != nil {
			return projectSnapshot{}, fmt.Errorf("github tracker: list items of %s: %w", ref, err)
		}
		for _, it := range page.Items {
			if it.Content.Kind != forge.ProjectContentIssue {
				continue
			}
			if !strings.EqualFold(it.Content.Repo, a.opts.Repo) {
				continue // another repo's card on a board that spans several
			}
			snap.byNumber[it.Content.Number] = it
		}
		if !page.HasNext || page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	return snap, nil
}

// state returns the workflow state of one issue per the board, and the item
// backing it. An issue absent from the board, or in a column the mapping does
// not cover, has NO state — which both callers read as "not dispatchable".
func (s projectSnapshot) state(number int, mapping []forge.StatusMapping) (string, forge.ProjectItem, bool) {
	it, ok := s.byNumber[number]
	if !ok {
		return "", forge.ProjectItem{}, false
	}
	fv, ok := it.Field(forge.ProjectStatusFieldName)
	if !ok || strings.TrimSpace(fv.Value) == "" {
		return "", it, false
	}
	st, ok := forge.StateForStatus(mapping, fv.Value)
	if !ok {
		return "", it, false
	}
	return st, it, true
}

// isCandidateStatus reports whether an item's column is one the operator
// dispatches from.
func (s projectSnapshot) isCandidateStatus(it forge.ProjectItem, want []string) bool {
	fv, ok := it.Field(forge.ProjectStatusFieldName)
	if !ok {
		return false
	}
	for _, w := range want {
		if strings.EqualFold(strings.TrimSpace(w), strings.TrimSpace(fv.Value)) {
			return true
		}
	}
	return false
}

// listCandidatesFromBoard is ListCandidates in board mode. Issue CONTENT still
// comes from the issue list (a project item carries no body, labels or
// assignee); only the state and the eligibility come from the board.
func (a *GitHubAdapter) listCandidatesFromBoard(ctx context.Context, raw []ghIssue) ([]Issue, error) {
	snap, err := a.readProject(ctx)
	if err != nil {
		return nil, err
	}
	p := a.opts.Project
	want := p.candidateStatuses()
	mapping := p.mapping()

	openNums := make(map[int]bool, len(raw))
	for _, r := range raw {
		openNums[r.Number] = true
	}
	pending := make([]Issue, 0, len(raw))
	for _, r := range raw {
		if ghHasLabel(r.Labels, a.opts.ClaimedLabel) {
			continue // the label claim is still the lease
		}
		if !a.authorAllowed(r.Author.Login) {
			continue
		}
		state, it, ok := snap.state(r.Number, mapping)
		if !ok || !snap.isCandidateStatus(it, want) {
			continue
		}
		iss := a.toIssue(r)
		iss.WorkflowState = state
		iss.Metadata["project_item"] = it.ID
		iss.Metadata["project_status"] = statusValueOf(it)
		pending = append(pending, iss)
	}
	return filterHeldByBlockers(pending, openNums, a.opts.Logger, "github"), nil
}

func statusValueOf(it forge.ProjectItem) string {
	if fv, ok := it.Field(forge.ProjectStatusFieldName); ok {
		return fv.Value
	}
	return ""
}

// refreshStatesFromBoard is RefreshStates in board mode: one board read
// answers the whole set, instead of one REST call per issue.
func (a *GitHubAdapter) refreshStatesFromBoard(ctx context.Context, ids []string) (map[string]string, error) {
	snap, err := a.readProject(ctx)
	if err != nil {
		return nil, err
	}
	mapping := a.opts.Project.mapping()
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		num, ok := parseGitHubID(a.opts.Repo, id)
		if !ok {
			continue
		}
		// Absence is meaningful to the dispatcher ("issue disappeared"), so an
		// issue off the board, or in an unmapped column, is OMITTED rather
		// than mapped onto a zero-value state.
		if state, _, ok := snap.state(num, mapping); ok {
			out[id] = state
		}
	}
	return out, nil
}

// updateStateOnBoard is UpdateState in board mode: it writes the item's Status
// field. An issue the board does not carry yet is ADDED to it — a dispatcher
// that could not record "in progress" because nobody had dragged the card on
// would leave the roadmap permanently behind.
func (a *GitHubAdapter) updateStateOnBoard(ctx context.Context, id, newState string) error {
	p := a.opts.Project
	status, ok := forge.StatusForState(p.mapping(), newState)
	if !ok {
		return fmt.Errorf("%w: state %q has no column on project %s", ErrTransitionRejected, newState, p.ref())
	}
	num, ok := parseGitHubID(a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	snap, err := a.readProject(ctx)
	if err != nil {
		return err
	}
	option, ok := snap.statusField.Option(status)
	if !ok {
		return fmt.Errorf("%w: project %s has no %q option in its %s field",
			ErrTransitionRejected, p.ref(), status, forge.ProjectStatusFieldName)
	}
	project, err := p.Board.GetProject(ctx, p.ref())
	if err != nil {
		return fmt.Errorf("github tracker: read project %s: %w", p.ref(), err)
	}
	itemID := ""
	if it, ok := snap.byNumber[num]; ok {
		itemID = it.ID
	} else {
		contentID, err := p.Board.IssueContentID(ctx, a.opts.Repo, num)
		if err != nil {
			return fmt.Errorf("github tracker: resolve %s#%d for the board: %w", a.opts.Repo, num, err)
		}
		added, err := p.Board.AddItem(ctx, project.ID, contentID)
		if err != nil {
			return fmt.Errorf("github tracker: add %s#%d to project %s: %w", a.opts.Repo, num, p.ref(), err)
		}
		itemID = added.ID
	}
	if err := p.Board.SetSingleSelect(ctx, project.ID, itemID, snap.statusField.ID, option.ID); err != nil {
		return fmt.Errorf("github tracker: set %s=%q on %s#%d: %w",
			forge.ProjectStatusFieldName, status, a.opts.Repo, num, err)
	}
	return nil
}
