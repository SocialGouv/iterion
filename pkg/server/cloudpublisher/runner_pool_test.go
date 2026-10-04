package cloudpublisher

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// fakePoolTeamResolver answers the publisher's TeamResolver seam with a fixed
// team (or error), so the pool mapping's publish-side contract is testable
// without Mongo.
type fakePoolTeamResolver struct {
	team identity.Team
	err  error
}

func (f fakePoolTeamResolver) GetTeam(context.Context, string) (identity.Team, error) {
	return f.team, f.err
}

func (f fakePoolTeamResolver) GetOrg(context.Context, string) (identity.Org, error) {
	return identity.Org{}, errors.New("not used by these tests")
}

func poolTestPublisher(st store.RunStore, res TeamResolver, published *[]*queue.RunMessage) *Publisher {
	return &Publisher{
		store:    st,
		logger:   iterlog.New(iterlog.LevelError, io.Discard),
		identity: res,
		publishRun: func(_ context.Context, msg *queue.RunMessage) error {
			*published = append(*published, msg)
			return nil
		},
	}
}

func poolLaunch() (context.Context, *ir.Workflow, *runview.CompiledSource) {
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	return ctx, &ir.Workflow{Name: "wf"}, &runview.CompiledSource{Hash: "hash"}
}

// A team mapped to a sovereign pool must NOT launch while pool dispatch is
// unwired: the run is refused synchronously rather than served outside the
// team's pool — the leak #2029 exists to make impossible. Red when the
// poolDispatchEnabled refusal is dropped.
func TestSubmitLaunch_ATeamMappedToAPoolIsRefusedUntilDispatchShips(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := poolTestPublisher(st, fakePoolTeamResolver{team: identity.Team{ID: "team-a", RunnerPool: "honorabilite"}}, &[]*queue.RunMessage{})
	ctx, wf, cs := poolLaunch()
	if _, err := p.SubmitLaunch(ctx, "run-pool-refused", runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n"}, wf, cs); err == nil ||
		!strings.Contains(err.Error(), "honorabilite") || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("a pool-mapped launch must be refused naming the pool, got: %v", err)
	}
	// Synchronous refusal like the input check: nothing is persisted and
	// nothing is published — there is no queued row to grow stale.
	if stRun, lerr := st.LoadRun(context.Background(), "run-pool-refused"); lerr == nil && stRun.Status == store.RunStatusQueued {
		t.Fatalf("a refused pool launch left a queued row nothing will claim: %+v", stRun)
	}
}

// A deployment without the identity seam has no mapping table at all: the
// launch proceeds with the shared default (the adjusted F10 disposition),
// while a store ERROR with the seam wired refuses (the witness above).
func TestSubmitLaunch_NoIdentitySeamLaunchesUnmapped(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := poolTestPublisher(st, nil, &[]*queue.RunMessage{})
	ctx, wf, cs := poolLaunch()
	if _, err := p.SubmitLaunch(ctx, "run-nil-identity", runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n"}, wf, cs); err != nil {
		t.Fatalf("a no-seam deployment's launch must proceed unmapped: %v", err)
	}
}

// An identity-store error is a refusal, not a silent fallback onto the
// shared pool. Red when the resolver swallows the error.
func TestSubmitLaunch_AnUnreadablePoolMappingRefusesTheLaunch(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := poolTestPublisher(st, fakePoolTeamResolver{err: errors.New("identity store down")}, &[]*queue.RunMessage{})
	ctx, wf, cs := poolLaunch()
	if _, err := p.SubmitLaunch(ctx, "run-pool-error", runview.LaunchSpec{FilePath: "wf.bot"}, wf, cs); err == nil ||
		!strings.Contains(err.Error(), "identity store down") {
		t.Fatalf("an unreadable mapping must refuse the launch, got: %v", err)
	}
}

// An unmapped team is untouched: the launch proceeds and both carriers
// stamp the empty (shared default) pool.
func TestSubmitLaunch_AnUnmappedTeamStillLaunches(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var published []*queue.RunMessage
	p := poolTestPublisher(st, fakePoolTeamResolver{team: identity.Team{ID: "team-a"}}, &published)
	ctx, wf, cs := poolLaunch()
	if _, err := p.SubmitLaunch(ctx, "run-unmapped", runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n"}, wf, cs); err != nil {
		t.Fatalf("an unmapped team's launch must proceed: %v", err)
	}
	if len(published) != 1 || published[0].RunnerPool != "" {
		t.Fatalf("the wire must carry the empty (default) pool: %+v", published)
	}
	r, lerr := st.LoadRun(context.Background(), "run-unmapped")
	if lerr != nil {
		t.Fatalf("LoadRun: %v", lerr)
	}
	if r.RunnerPool != "" {
		t.Fatalf("the doc must stamp the empty (default) pool: %+v", r)
	}
}

// A resume whose team's mapping moved past the run's frozen stamp is
// refused before anything is published. Red when the comparison is dropped.
func TestSubmitResume_APoolMoveRefusesTheResume(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := st.SaveRun(context.Background(), &store.Run{
		ID: "run-moved", TenantID: "team-a", OwnerID: "u1",
		Status: store.RunStatusFailedResumable, RunnerPool: "old-pool",
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	p := poolTestPublisher(st, fakePoolTeamResolver{team: identity.Team{ID: "team-a", RunnerPool: "new-pool"}}, &[]*queue.RunMessage{})
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.ResumeSpec{RunID: "run-moved", FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n"}
	if err := p.SubmitResume(context.Background(), spec, wf, &runview.CompiledSource{Hash: "hash"}); err == nil ||
		!errors.Is(err, ErrPoolRemapped) || !strings.Contains(err.Error(), "old-pool") || !strings.Contains(err.Error(), "new-pool") {
		t.Fatalf("a pool move must refuse with the typed ErrPoolRemapped naming both pools, got: %v", err)
	}
}

// The dispatch gate is symmetric: a POOL-stamped resume publishes onto the
// pool's stream — unwired, it is refused exactly like a pool-mapped launch,
// even when the frozen stamp still matches the mapping. Red when the resume
// gate is dropped (F8). When P1b flips poolDispatchEnabled, this test flips
// with it into the frozen-stamp-on-the-wire witness.
func TestSubmitResume_TheFrozenPoolGate(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := st.SaveRun(context.Background(), &store.Run{
		ID: "run-frozen", TenantID: "team-a", OwnerID: "u1",
		Status: store.RunStatusFailedResumable, RunnerPool: "old-pool",
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	var published []*queue.RunMessage
	p := poolTestPublisher(st, fakePoolTeamResolver{team: identity.Team{ID: "team-a", RunnerPool: "old-pool"}}, &published)
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.ResumeSpec{RunID: "run-frozen", FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n"}
	if poolDispatchEnabled {
		t.Skip("pool dispatch is enabled — this becomes the frozen-stamp-on-the-wire witness")
	}
	if err := p.SubmitResume(context.Background(), spec, wf, &runview.CompiledSource{Hash: "hash"}); err == nil ||
		!strings.Contains(err.Error(), "old-pool") || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("a pool-stamped resume must be gated while dispatch is off, got: %v", err)
	}
	if len(published) != 0 {
		t.Fatalf("the gated resume published %d message(s)", len(published))
	}
}

// The resume wire carries the FROZEN stamp — witnessed at the source level
// while the dispatch gate refuses everything pool-stamped (rva-2 F4: the
// behavioural capture is impossible before P1b flips the gate, and dropping
// `RunnerPool: prior.RunnerPool` from the resume literal shipped green).
// The scan is anchored to the SubmitResume function so an unrelated field
// elsewhere cannot satisfy it. Red when the literal is dropped again.
func TestSubmitResumeWireCarriesTheFrozenStamp(t *testing.T) {
	src, err := os.ReadFile("publisher.go")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "func (p *Publisher) SubmitResume(")
	if start < 0 {
		t.Fatal("SubmitResume not found — the publisher moved; re-anchor this witness")
	}
	end := strings.Index(string(src)[start:], "\nfunc ")
	if end < 0 {
		t.Fatal("SubmitResume's end not found")
	}
	body := string(src[start : start+end])
	if !strings.Contains(body, "RunnerPool: prior.RunnerPool") {
		t.Fatal("SubmitResume no longer stamps the resume message with the run's frozen pool — pool-stamped resumes would publish onto the shared stream unstamped")
	}
}
