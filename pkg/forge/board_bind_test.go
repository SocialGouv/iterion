package forge_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// BindBoard is where a human-readable board address becomes the opaque ids
// every later write uses. Everything it can get wrong is silent later: a
// mistyped column name, a map that cannot round-trip, a board the credential
// cannot see. So it fails here, naming what it could not resolve.

type bindFake struct {
	project forge.Project
	err     error
}

func (f *bindFake) GetProject(context.Context, forge.ProjectRef) (forge.Project, error) {
	if f.err != nil {
		return forge.Project{}, f.err
	}
	return f.project, nil
}
func (f *bindFake) ListProjectItems(context.Context, forge.ProjectRef, forge.ProjectItemListOptions) (forge.ProjectItemPage, error) {
	return forge.ProjectItemPage{}, nil
}
func (f *bindFake) ItemForIssue(context.Context, forge.ProjectRef, string, int) (forge.ProjectItem, bool, error) {
	return forge.ProjectItem{}, false, errors.New("not used")
}
func (f *bindFake) IssueContentID(context.Context, string, int) (string, error) {
	return "", errors.New("not used")
}
func (f *bindFake) AddItem(context.Context, string, string) (forge.ProjectItem, error) {
	return forge.ProjectItem{}, errors.New("not used")
}
func (f *bindFake) SetSingleSelect(context.Context, string, string, string, string) error { return nil }

func bindBoardProject() forge.Project {
	return forge.Project{
		ID: "PVT_p", Number: 203, Title: "Iterion", URL: "https://github.com/orgs/SocialGouv/projects/203",
		Fields: []forge.ProjectField{
			{ID: "PVTSSF_status", Name: "Status", DataType: "SINGLE_SELECT", Options: []forge.ProjectFieldOption{
				{ID: "o_inbox", Name: "Inbox"}, {ID: "o_planned", Name: "Planned"},
				{ID: "o_prog", Name: "In progress"}, {ID: "o_blocked", Name: "Blocked"},
				{ID: "o_done", Name: "Done"},
			}},
			{ID: "PVTSSF_area", Name: "Area", DataType: "SINGLE_SELECT", Options: []forge.ProjectFieldOption{{ID: "a1", Name: "engine"}}},
			{ID: "PVTSSF_prio", Name: "Priority", DataType: "SINGLE_SELECT", Options: []forge.ProjectFieldOption{{ID: "p1", Name: "P0"}}},
			{ID: "PVTF_target", Name: "Target", DataType: "DATE"},
		},
	}
}

func bindRef() forge.ProjectRef {
	return forge.ProjectRef{Owner: "SocialGouv", OwnerKind: forge.ProjectOwnerOrg, Number: 203}
}

func TestBindBoardResolvesIdsByName(t *testing.T) {
	bc := &bindFake{project: bindBoardProject()}
	b, err := forge.BindBoard(context.Background(), bc, forge.BindRequest{
		TenantID: "team-a", Provider: forge.ProviderGitHub,
		Ref: bindRef(), ConnectionID: "conn-1",
	})
	if err != nil {
		t.Fatalf("BindBoard: %v", err)
	}
	if b.ProjectID != "PVT_p" || b.ProjectTitle != "Iterion" || b.StatusFieldID != "PVTSSF_status" {
		t.Fatalf("discovery wrong: %+v", b)
	}
	// Every mapped state must carry the option id the reflect will write.
	for _, m := range forge.DefaultStatusMapping() {
		id, ok := b.OptionForState(m.State)
		if !ok || id == "" {
			t.Errorf("state %q has no resolved option id", m.State)
		}
	}
	if id, _ := b.OptionForState("in_progress"); id != "o_prog" {
		t.Errorf("in_progress → %q, want o_prog", id)
	}
	// Only the label fields the board actually carries are bound: Mode is
	// absent here, so binding it would promise a label that never arrives.
	names := []string{}
	for _, f := range b.LabelFields {
		names = append(names, f.Name)
	}
	if !slices.Contains(names, "Area") || !slices.Contains(names, "Priority") {
		t.Errorf("label fields = %v, want Area + Priority", names)
	}
	if slices.Contains(names, "Mode") {
		t.Errorf("label fields = %v: Mode is not on this board", names)
	}
	if b.SyncEvery != forge.DefaultBoardSyncEvery {
		t.Errorf("SyncEvery = %v, want the default %v", b.SyncEvery, forge.DefaultBoardSyncEvery)
	}
	if err := b.Validate(); err != nil {
		t.Errorf("a fresh binding must be storable: %v", err)
	}
}

func TestBindBoardAcceptsAnOperatorStatusMap(t *testing.T) {
	p := bindBoardProject()
	p.Fields[0].Options = []forge.ProjectFieldOption{
		{ID: "o_todo", Name: "Todo"}, {ID: "o_doing", Name: "Doing"}, {ID: "o_ship", Name: "Shipped"},
	}
	bc := &bindFake{project: p}

	b, err := forge.BindBoard(context.Background(), bc, forge.BindRequest{
		TenantID: "team-a", Provider: forge.ProviderGitHub, Ref: bindRef(), ConnectionID: "conn-1",
		StatusMap: map[string]string{"Todo": "ready", "Doing": "in_progress", "Shipped": "done"},
	})
	if err != nil {
		t.Fatalf("BindBoard: %v", err)
	}
	if got := b.StatusOptions["ready"]; got != "o_todo" {
		t.Errorf("ready → %q, want o_todo — the five shipped names are a default, not a fence", got)
	}
	if len(b.StatusMapping) != 3 {
		t.Errorf("the effective map must be stored: %+v", b.StatusMapping)
	}
	if len(b.MissingStatuses) != 0 {
		t.Errorf("nothing is missing here: %v", b.MissingStatuses)
	}
}

func TestBindBoardRefusesANonInjectiveMap(t *testing.T) {
	bc := &bindFake{project: bindBoardProject()}
	_, err := forge.BindBoard(context.Background(), bc, forge.BindRequest{
		TenantID: "team-a", Provider: forge.ProviderGitHub, Ref: bindRef(), ConnectionID: "conn-1",
		StatusMap: map[string]string{"Planned": "ready", "Inbox": "ready"},
	})
	if err == nil {
		t.Fatal("want an error: two columns on one state cannot round-trip")
	}
	if !strings.Contains(err.Error(), "injective") {
		t.Errorf("the error must explain the refusal, got %q", err)
	}
}

// TestBindBoardReportsPartialCoverage: a board missing SOME mapped columns
// still binds — the covered half works — but the gap is named, not silent.
func TestBindBoardReportsPartialCoverage(t *testing.T) {
	p := bindBoardProject()
	p.Fields[0].Options = []forge.ProjectFieldOption{
		{ID: "o_planned", Name: "Planned"}, {ID: "o_done", Name: "Done"},
	}
	bc := &bindFake{project: p}

	b, err := forge.BindBoard(context.Background(), bc, forge.BindRequest{
		TenantID: "team-a", Provider: forge.ProviderGitHub, Ref: bindRef(), ConnectionID: "conn-1",
	})
	if err != nil {
		t.Fatalf("a partially-covered board must still bind: %v", err)
	}
	if len(b.StatusOptions) != 2 {
		t.Errorf("resolved options = %v, want the two the board carries", b.StatusOptions)
	}
	want := []string{"Blocked", "In progress", "Inbox"}
	if !slices.Equal(b.MissingStatuses, want) {
		t.Errorf("MissingStatuses = %v, want %v (sorted, so the report is stable)", b.MissingStatuses, want)
	}
}

// TestBindBoardRefusesAMapThatMatchesNothing: a map covering none of the
// board's columns is an operator typo, not a partial board — binding it would
// ship a status projection that is inert from the first minute.
func TestBindBoardRefusesAMapThatMatchesNothing(t *testing.T) {
	bc := &bindFake{project: bindBoardProject()}
	_, err := forge.BindBoard(context.Background(), bc, forge.BindRequest{
		TenantID: "team-a", Provider: forge.ProviderGitHub, Ref: bindRef(), ConnectionID: "conn-1",
		StatusMap: map[string]string{"Nope": "ready", "Nada": "done"},
	})
	if err == nil {
		t.Fatal("want an error: not one mapped column exists on the board")
	}
	for _, want := range []string{"Nope", "Nada", "Status"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name what it could not find (%q missing): %q", want, err)
		}
	}
}

func TestBindBoardWithoutAStatusFieldBindsLabelsOnly(t *testing.T) {
	p := bindBoardProject()
	p.Fields = p.Fields[1:] // drop Status
	bc := &bindFake{project: p}

	b, err := forge.BindBoard(context.Background(), bc, forge.BindRequest{
		TenantID: "team-a", Provider: forge.ProviderGitHub, Ref: bindRef(), ConnectionID: "conn-1",
	})
	if err != nil {
		t.Fatalf("a board with no Status field must still bind for labels: %v", err)
	}
	if b.StatusFieldID != "" || len(b.StatusOptions) != 0 {
		t.Errorf("there is no status to project: %+v", b)
	}
	if len(b.LabelFields) == 0 {
		t.Error("the label half must still be bound")
	}
}

func TestBindBoardSyncEveryIsExplicit(t *testing.T) {
	bc := &bindFake{project: bindBoardProject()}
	off := time.Duration(0)
	b, err := forge.BindBoard(context.Background(), bc, forge.BindRequest{
		TenantID: "team-a", Provider: forge.ProviderGitHub, Ref: bindRef(), ConnectionID: "conn-1",
		SyncEvery: &off,
	})
	if err != nil {
		t.Fatalf("BindBoard: %v", err)
	}
	if b.SyncEvery != 0 {
		t.Errorf("an explicit 0 means OFF and must not be replaced by the default, got %v", b.SyncEvery)
	}
	five := 5 * time.Minute
	b2, err := forge.BindBoard(context.Background(), bc, forge.BindRequest{
		TenantID: "team-a", Provider: forge.ProviderGitHub, Ref: bindRef(), ConnectionID: "conn-1",
		SyncEvery: &five,
	})
	if err != nil {
		t.Fatalf("BindBoard: %v", err)
	}
	if b2.SyncEvery != five {
		t.Errorf("SyncEvery = %v, want %v", b2.SyncEvery, five)
	}
	neg := -time.Second
	if _, err := forge.BindBoard(context.Background(), bc, forge.BindRequest{
		TenantID: "team-a", Provider: forge.ProviderGitHub, Ref: bindRef(), ConnectionID: "conn-1",
		SyncEvery: &neg,
	}); err == nil {
		t.Error("a negative interval must be refused, not silently coerced")
	}
	// Below the floor is REFUSED, not clamped: an operator who typed 10s must
	// see that they did not get 10s.
	tooFast := 10 * time.Second
	_, err = forge.BindBoard(context.Background(), bc, forge.BindRequest{
		TenantID: "team-a", Provider: forge.ProviderGitHub, Ref: bindRef(), ConnectionID: "conn-1",
		SyncEvery: &tooFast,
	})
	if err == nil {
		t.Fatal("an interval under the floor must be refused")
	}
	if !strings.Contains(err.Error(), forge.MinBoardSyncEvery.String()) {
		t.Errorf("the error must name the floor, got %q", err)
	}
}

func TestBindBoardSurfacesAnUnreachableBoard(t *testing.T) {
	bc := &bindFake{err: forge.ErrProjectNotFound}
	_, err := forge.BindBoard(context.Background(), bc, forge.BindRequest{
		TenantID: "team-a", Provider: forge.ProviderGitHub, Ref: bindRef(), ConnectionID: "conn-1",
	})
	if !errors.Is(err, forge.ErrProjectNotFound) {
		t.Fatalf("want the client's error, got %v", err)
	}
}

func TestBindBoardValidatesTheRequest(t *testing.T) {
	bc := &bindFake{project: bindBoardProject()}
	for _, tc := range []struct {
		name string
		req  forge.BindRequest
	}{
		{"no tenant", forge.BindRequest{Provider: forge.ProviderGitHub, Ref: bindRef(), ConnectionID: "c"}},
		{"no connection", forge.BindRequest{TenantID: "t", Provider: forge.ProviderGitHub, Ref: bindRef()}},
		{"bad ref", forge.BindRequest{TenantID: "t", Provider: forge.ProviderGitHub, ConnectionID: "c"}},
		{"bad provider", forge.BindRequest{TenantID: "t", Provider: "svn", Ref: bindRef(), ConnectionID: "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := forge.BindBoard(context.Background(), bc, tc.req); err == nil {
				t.Fatal("want a validation error")
			}
		})
	}
}
