package cloudpublisher

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// fakeTeamResolver maps team → org and counts lookups (cache assertions).
type fakeTeamResolver struct {
	orgs  map[string]string
	err   error
	calls int
}

func (f *fakeTeamResolver) GetTeam(_ context.Context, id string) (identity.Team, error) {
	f.calls++
	if f.err != nil {
		return identity.Team{}, f.err
	}
	orgID, ok := f.orgs[id]
	if !ok {
		return identity.Team{}, identity.ErrNotFound
	}
	return identity.Team{ID: id, OrgID: orgID}, nil
}

// TestSubmitLaunchStampsOrgID is the regression test for the multi-team
// cost-cap bug: the launch gate meters on the org key, so the published
// RunMessage must carry the org id for the runner's AddSpend — a message
// without it made spend accrue on the team key and the org's monthly
// cost cap never trip.
func TestSubmitLaunchStampsOrgID(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	resolver := &fakeTeamResolver{orgs: map[string]string{"team-a": "org-1"}}
	var published []*queue.RunMessage
	p := &Publisher{
		store:    st,
		identity: resolver,
		publishRun: func(_ context.Context, msg *queue.RunMessage) error {
			published = append(published, msg)
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}
	if _, err := p.SubmitLaunch(ctx, "run-1", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if len(published) != 1 || published[0].OrgID != "org-1" {
		t.Fatalf("published OrgID = %+v, want org-1", published)
	}
	// Second launch for the same team hits the cache, not the resolver.
	if _, err := p.SubmitLaunch(ctx, "run-2", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch #2: %v", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1 (second launch must be served from cache)", resolver.calls)
	}
	if published[1].OrgID != "org-1" {
		t.Fatalf("cached OrgID = %q, want org-1", published[1].OrgID)
	}
}

func TestSubmitResumeStampsOrgIDFromPriorTenant(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	resolver := &fakeTeamResolver{orgs: map[string]string{"team-b": "org-2"}}
	var published []*queue.RunMessage
	p := &Publisher{
		store:    st,
		identity: resolver,
		publishRun: func(_ context.Context, msg *queue.RunMessage) error {
			published = append(published, msg)
			return nil
		},
	}
	// Prior run belongs to team-b; the resumer's ctx tenant is a DIFFERENT
	// team (super-admin resume) — the spend must still charge team-b's org.
	priorCtx := store.WithIdentity(context.Background(), "team-b", "u1")
	if err := st.SaveRun(priorCtx, &store.Run{
		FormatVersion: store.RunFormatVersion,
		ID:            "run-r", WorkflowName: "wf", Status: store.RunStatusFailedResumable,
		TenantID: "team-b", OwnerID: "u1",
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	adminCtx := store.WithIdentity(context.Background(), "team-b", "admin")
	wf := &ir.Workflow{Name: "wf"}
	if err := p.SubmitResume(adminCtx, runview.ResumeSpec{RunID: "run-r", FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if len(published) != 1 || published[0].OrgID != "org-2" {
		t.Fatalf("published OrgID = %+v, want org-2", published)
	}
}

func TestOrgIDForTeamFallbacks(t *testing.T) {
	// nil resolver (local mode) → "".
	p := &Publisher{}
	if got := p.orgIDForTeam(context.Background(), "team"); got != "" {
		t.Fatalf("nil resolver orgID = %q, want empty", got)
	}
	// Resolver error → "" and NOT cached (a later call retries).
	failing := &fakeTeamResolver{err: errors.New("mongo blip")}
	p = &Publisher{identity: failing}
	if got := p.orgIDForTeam(context.Background(), "team"); got != "" {
		t.Fatalf("error orgID = %q, want empty", got)
	}
	failing.err = nil
	failing.orgs = map[string]string{"team": "org-9"}
	if got := p.orgIDForTeam(context.Background(), "team"); got != "org-9" {
		t.Fatalf("post-recovery orgID = %q, want org-9 (error must not be cached)", got)
	}
	// Org-less team (pre-backfill) → "" (runner charges the tenant key,
	// matching the gate's own fallback).
	p = &Publisher{identity: &fakeTeamResolver{orgs: map[string]string{"solo": ""}}}
	if got := p.orgIDForTeam(context.Background(), "solo"); got != "" {
		t.Fatalf("org-less orgID = %q, want empty", got)
	}
}
