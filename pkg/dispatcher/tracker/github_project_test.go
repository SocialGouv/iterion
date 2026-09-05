package tracker_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// Board mode: the GitHub tracker takes its workflow state from a Projects v2
// board's Status field instead of from labels. The claim stays a label — a
// Projects v2 item carries nothing to fence a lease with — which is why
// ClaimLeaser remains unimplemented for this adapter either way.

// ---------------------------------------------------------------------------
// fake board client
// ---------------------------------------------------------------------------

type fakeProjectBoard struct {
	fields  []forge.ProjectField
	items   []forge.ProjectItem
	writes  []string // "<item>/<field>/<option>"
	added   []string // content ids passed to AddItem
	getErr  error
	setErr  error
	nodeIDs map[string]string // "repo#number" → content node id
}

func (f *fakeProjectBoard) GetProject(context.Context, forge.ProjectRef) (forge.Project, error) {
	if f.getErr != nil {
		return forge.Project{}, f.getErr
	}
	return forge.Project{ID: "PVT_p", Number: 203, Title: "Iterion", Fields: f.fields}, nil
}

func (f *fakeProjectBoard) ListProjectItems(context.Context, forge.ProjectRef, forge.ProjectItemListOptions) (forge.ProjectItemPage, error) {
	if f.getErr != nil {
		return forge.ProjectItemPage{}, f.getErr
	}
	return forge.ProjectItemPage{Items: f.items}, nil
}

func (f *fakeProjectBoard) IssueContentID(_ context.Context, repo string, number int) (string, error) {
	key := repo + "#" + itoa(number)
	if id, ok := f.nodeIDs[key]; ok {
		return id, nil
	}
	return "", errors.New("no node id for " + key)
}

func (f *fakeProjectBoard) AddItem(_ context.Context, _, contentID string) (forge.ProjectItem, error) {
	f.added = append(f.added, contentID)
	it := forge.ProjectItem{ID: "PVTI_new", Content: forge.ProjectItemContent{
		Kind: forge.ProjectContentIssue, Repo: "owner/repo", Number: 42,
	}}
	f.items = append(f.items, it)
	return it, nil
}

func (f *fakeProjectBoard) SetSingleSelect(_ context.Context, _, itemID, fieldID, optionID string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.writes = append(f.writes, itemID+"/"+fieldID+"/"+optionID)
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func projectStatusField() forge.ProjectField {
	return forge.ProjectField{ID: "PVTSSF_status", Name: "Status", DataType: "SINGLE_SELECT",
		Options: []forge.ProjectFieldOption{
			{ID: "opt_inbox", Name: "Inbox"},
			{ID: "opt_planned", Name: "Planned"},
			{ID: "opt_progress", Name: "In progress"},
			{ID: "opt_blocked", Name: "Blocked"},
			{ID: "opt_done", Name: "Done"},
		}}
}

func projectItem(itemID, repo string, number int, status string) forge.ProjectItem {
	it := forge.ProjectItem{
		ID:      itemID,
		Content: forge.ProjectItemContent{Kind: forge.ProjectContentIssue, Repo: repo, Number: number},
	}
	if status != "" {
		it.Fields = []forge.ProjectItemField{{
			FieldID: "PVTSSF_status", FieldName: "Status", Value: status,
			UpdatedAt: time.Now().UTC(),
		}}
	}
	return it
}

// boardModeAdapter wires a GitHub tracker in board mode over the two fakes.
func boardModeAdapter(t *testing.T, gh *fakeGH, board *fakeProjectBoard) *tracker.GitHubAdapter {
	t.Helper()
	a, err := tracker.NewGitHub(tracker.GitHubOptions{
		Repo:    "owner/repo",
		Command: gh.cmd,
		Project: &tracker.GitHubProjectOptions{
			Owner:  "owner",
			Number: 203,
			Board:  board,
		},
	})
	if err != nil {
		t.Fatalf("NewGitHub: %v", err)
	}
	return a
}

const twoOpenIssues = `[
 {"number":1,"title":"planned one","body":"","state":"OPEN","labels":[],"assignees":[],"author":{"login":"jo"},"url":"u1"},
 {"number":2,"title":"in progress one","body":"","state":"OPEN","labels":[],"assignees":[],"author":{"login":"jo"},"url":"u2"}
]`

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestGitHubProjectListCandidatesUsesTheStatusField(t *testing.T) {
	gh := &fakeGH{listOut: []byte(twoOpenIssues)}
	board := &fakeProjectBoard{
		fields: []forge.ProjectField{projectStatusField()},
		items: []forge.ProjectItem{
			projectItem("PVTI_1", "owner/repo", 1, "Planned"),
			projectItem("PVTI_2", "owner/repo", 2, "In progress"),
			// Another repo's card on the same board must not leak in.
			projectItem("PVTI_x", "other/repo", 1, "Planned"),
		},
	}
	a := boardModeAdapter(t, gh, board)

	got, err := a.ListCandidates(context.Background())
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("candidates = %d, want 1 (only the Planned one): %+v", len(got), got)
	}
	if got[0].ID != "github:owner/repo#1" {
		t.Errorf("candidate id = %q", got[0].ID)
	}
	if got[0].WorkflowState != "ready" {
		t.Errorf("WorkflowState = %q, want %q — Planned maps to ready", got[0].WorkflowState, "ready")
	}
	if got[0].Title != "planned one" {
		t.Errorf("title = %q: content still comes from the issue, not the item", got[0].Title)
	}
	if got[0].Metadata["project_item"] != "PVTI_1" {
		t.Errorf("the item id must be surfaced for the state write: %v", got[0].Metadata)
	}
}

func TestGitHubProjectListCandidatesDropsIssuesNotOnTheBoard(t *testing.T) {
	gh := &fakeGH{listOut: []byte(twoOpenIssues)}
	board := &fakeProjectBoard{
		fields: []forge.ProjectField{projectStatusField()},
		items:  []forge.ProjectItem{projectItem("PVTI_1", "owner/repo", 1, "Planned")},
	}
	a := boardModeAdapter(t, gh, board)

	got, err := a.ListCandidates(context.Background())
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(got) != 1 || got[0].ID != "github:owner/repo#1" {
		t.Fatalf("an issue absent from the board has no status and cannot be a candidate: %+v", got)
	}
}

func TestGitHubProjectListCandidatesStillHonoursTheClaimLabel(t *testing.T) {
	gh := &fakeGH{listOut: []byte(`[
	 {"number":1,"title":"claimed","body":"","state":"OPEN","labels":[{"name":"iterion-claimed"}],"assignees":[],"author":{"login":"jo"},"url":"u1"}
	]`)}
	board := &fakeProjectBoard{
		fields: []forge.ProjectField{projectStatusField()},
		items:  []forge.ProjectItem{projectItem("PVTI_1", "owner/repo", 1, "Planned")},
	}
	a := boardModeAdapter(t, gh, board)

	got, err := a.ListCandidates(context.Background())
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the label claim is still the lease in board mode: %+v", got)
	}
}

func TestGitHubProjectCandidateStatusesAreConfigurable(t *testing.T) {
	gh := &fakeGH{listOut: []byte(twoOpenIssues)}
	board := &fakeProjectBoard{
		fields: []forge.ProjectField{projectStatusField()},
		items: []forge.ProjectItem{
			projectItem("PVTI_1", "owner/repo", 1, "Planned"),
			projectItem("PVTI_2", "owner/repo", 2, "In progress"),
		},
	}
	a, err := tracker.NewGitHub(tracker.GitHubOptions{
		Repo: "owner/repo", Command: gh.cmd,
		Project: &tracker.GitHubProjectOptions{
			Owner: "owner", Number: 203, Board: board,
			CandidateStatuses: []string{"Planned", "In progress"},
		},
	})
	if err != nil {
		t.Fatalf("NewGitHub: %v", err)
	}
	got, err := a.ListCandidates(context.Background())
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2 — the eligible column list is the operator's", len(got))
	}
}

func TestGitHubProjectUpdateStateWritesTheField(t *testing.T) {
	gh := &fakeGH{listOut: []byte(twoOpenIssues)}
	board := &fakeProjectBoard{
		fields: []forge.ProjectField{projectStatusField()},
		items:  []forge.ProjectItem{projectItem("PVTI_1", "owner/repo", 1, "Planned")},
	}
	a := boardModeAdapter(t, gh, board)

	if err := a.UpdateState(context.Background(), "github:owner/repo#1", "in_progress"); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	if len(board.writes) != 1 || board.writes[0] != "PVTI_1/PVTSSF_status/opt_progress" {
		t.Fatalf("writes = %v, want the In progress option on PVTI_1", board.writes)
	}
	// No `gh issue edit` label shuffling in board mode: the column IS the field.
	for _, c := range gh.calls {
		if len(c) >= 2 && c[0] == "issue" && c[1] == "edit" {
			t.Errorf("board mode must not fall back to label edits: %v", c)
		}
	}
}

func TestGitHubProjectUpdateStateRejectsAnUnmappedState(t *testing.T) {
	gh := &fakeGH{listOut: []byte(twoOpenIssues)}
	board := &fakeProjectBoard{
		fields: []forge.ProjectField{projectStatusField()},
		items:  []forge.ProjectItem{projectItem("PVTI_1", "owner/repo", 1, "Planned")},
	}
	a := boardModeAdapter(t, gh, board)

	err := a.UpdateState(context.Background(), "github:owner/repo#1", "review")
	if !errors.Is(err, tracker.ErrTransitionRejected) {
		t.Fatalf("want ErrTransitionRejected for a state the board has no column for, got %v", err)
	}
	if len(board.writes) != 0 {
		t.Errorf("a rejected transition must write nothing: %v", board.writes)
	}
}

func TestGitHubProjectUpdateStateAddsAnAbsentIssueToTheBoard(t *testing.T) {
	gh := &fakeGH{listOut: []byte(twoOpenIssues)}
	board := &fakeProjectBoard{
		fields:  []forge.ProjectField{projectStatusField()},
		items:   nil,
		nodeIDs: map[string]string{"owner/repo#42": "I_node42"},
	}
	a := boardModeAdapter(t, gh, board)

	if err := a.UpdateState(context.Background(), "github:owner/repo#42", "done"); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	if len(board.added) != 1 || board.added[0] != "I_node42" {
		t.Fatalf("an issue the board does not carry must be added, not silently skipped: %v", board.added)
	}
	if len(board.writes) != 1 || !strings.HasSuffix(board.writes[0], "/opt_done") {
		t.Fatalf("writes = %v", board.writes)
	}
}

func TestGitHubProjectRefreshStatesReadsTheStatusField(t *testing.T) {
	gh := &fakeGH{listOut: []byte(twoOpenIssues)}
	board := &fakeProjectBoard{
		fields: []forge.ProjectField{projectStatusField()},
		items: []forge.ProjectItem{
			projectItem("PVTI_1", "owner/repo", 1, "Done"),
			projectItem("PVTI_2", "owner/repo", 2, "Icebox"), // unmapped
		},
	}
	a := boardModeAdapter(t, gh, board)

	got, err := a.RefreshStates(context.Background(), []string{
		"github:owner/repo#1", "github:owner/repo#2", "github:owner/repo#9",
	})
	if err != nil {
		t.Fatalf("RefreshStates: %v", err)
	}
	if got["github:owner/repo#1"] != "done" {
		t.Errorf("state = %q, want done", got["github:owner/repo#1"])
	}
	if _, ok := got["github:owner/repo#2"]; ok {
		t.Error("an unmapped status must be absent, not a zero-value state")
	}
	if _, ok := got["github:owner/repo#9"]; ok {
		t.Error("an issue not on the board must be absent — the dispatcher reads absence as 'disappeared'")
	}
	// No per-issue REST calls in board mode.
	for _, c := range gh.calls {
		if len(c) >= 1 && c[0] == "api" {
			t.Errorf("board mode must read the board, not the REST issue: %v", c)
		}
	}
}

// TestGitHubProjectBoardErrorsAreLoud pins the explicit-errors rule: a board
// the adapter cannot read must fail the poll, never quietly degrade back to
// label-derived states — which would dispatch on a state nobody configured.
func TestGitHubProjectBoardErrorsAreLoud(t *testing.T) {
	gh := &fakeGH{listOut: []byte(twoOpenIssues)}
	board := &fakeProjectBoard{getErr: forge.ErrProjectNotFound}
	a := boardModeAdapter(t, gh, board)

	if _, err := a.ListCandidates(context.Background()); !errors.Is(err, forge.ErrProjectNotFound) {
		t.Fatalf("ListCandidates must surface the board error, got %v", err)
	}
	if _, err := a.RefreshStates(context.Background(), []string{"github:owner/repo#1"}); !errors.Is(err, forge.ErrProjectNotFound) {
		t.Fatalf("RefreshStates must surface the board error, got %v", err)
	}
}

func TestGitHubProjectMissingStatusFieldIsAnError(t *testing.T) {
	gh := &fakeGH{listOut: []byte(twoOpenIssues)}
	board := &fakeProjectBoard{fields: []forge.ProjectField{
		{ID: "PVTSSF_area", Name: "Area", Options: []forge.ProjectFieldOption{{ID: "o", Name: "engine"}}},
	}}
	a := boardModeAdapter(t, gh, board)

	_, err := a.ListCandidates(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Status") {
		t.Fatalf("a board with no Status field must fail naming it, got %v", err)
	}
}

func TestGitHubProjectClaimStaysALabel(t *testing.T) {
	gh := &fakeGH{listOut: []byte(twoOpenIssues)}
	board := &fakeProjectBoard{
		fields: []forge.ProjectField{projectStatusField()},
		items:  []forge.ProjectItem{projectItem("PVTI_1", "owner/repo", 1, "Planned")},
	}
	a := boardModeAdapter(t, gh, board)

	if err := a.Claim(context.Background(), "github:owner/repo#1", "host-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	found := false
	for _, c := range gh.calls {
		if len(c) >= 2 && c[0] == "issue" && c[1] == "edit" {
			found = true
		}
	}
	if !found {
		t.Error("the claim is still a label edit: Projects v2 has nothing to fence a lease with")
	}
	if len(board.writes) != 0 {
		t.Errorf("a claim must not touch the board's Status: %v", board.writes)
	}
	// And the adapter must keep declining the lease capability.
	if _, ok := any(a).(tracker.ClaimLeaser); ok {
		t.Error("a label claim carries no fencing epoch — ClaimLeaser must stay unimplemented")
	}
}

func TestGitHubProjectOptionsValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts tracker.GitHubProjectOptions
	}{
		{"no owner", tracker.GitHubProjectOptions{Number: 1, Board: &fakeProjectBoard{}}},
		{"no number", tracker.GitHubProjectOptions{Owner: "o", Board: &fakeProjectBoard{}}},
		{"no board client", tracker.GitHubProjectOptions{Owner: "o", Number: 1}},
		{"bad owner kind", tracker.GitHubProjectOptions{Owner: "o", Number: 1, OwnerKind: "group", Board: &fakeProjectBoard{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.opts
			if _, err := tracker.NewGitHub(tracker.GitHubOptions{Repo: "owner/repo", Project: &p}); err == nil {
				t.Fatal("want a construction error")
			}
		})
	}
}
