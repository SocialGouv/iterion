package cloudpublisher

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The fork lane's pool (ADR-162 Q3 and decision 10): the publisher stamps
// an untrusted run with the pool its fork lane names — the run document's
// and the queue message's RunnerPool — so the fork run lands on that pool
// alone, and every resume follows the frozen stamp under the same rule.
// Each refusal is ErrForkLanePool, before anything is persisted or
// published. One row per guard of forkLanePoolRefusal, each isolated: the
// other guards admit its fixture.

const forkLaneHead = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// forkLaneSpec is an untrusted launch as the fork lane hands it over.
func forkLaneSpec(pool string) runview.LaunchSpec {
	return runview.LaunchSpec{
		FilePath: "fork_review.bot", Source: "workflow fork_review:\n  entry: done\n", BotID: "review-pr",
		Trust: store.RunTrustFork, RepoSHAExpected: forkLaneHead,
		ForkAuthorSubject: string(orgusage.ForkAuthorSubject("team-a", "github", "4242")),
		ForkLanePool:      pool,
	}
}

// holderUnreadable answers teams like fakeTeamResolver and fails the
// one-team-per-pool read: the store that cannot say who holds a pool.
type holderUnreadable struct {
	fakeTeamResolver
	err error
}

func (h *holderUnreadable) GetTeamByRunnerPool(context.Context, string) (identity.Team, error) {
	return identity.Team{}, h.err
}

func forkLanePublisher(t *testing.T, res TeamResolver, reg RunnerPoolResolver) (*Publisher, *store.FilesystemRunStore, *[]*queue.RunMessage) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	published := &[]*queue.RunMessage{}
	return &Publisher{
		store:    st,
		logger:   iterlog.New(iterlog.LevelError, io.Discard),
		identity: res,
		pools:    reg,
		publishRun: func(_ context.Context, msg *queue.RunMessage) error {
			*published = append(*published, msg)
			return nil
		},
	}, st, published
}

// The happy path, on the REAL launch entry: the lane's pool is the run's
// pool on BOTH carriers, never the team's (empty) mapping.
//
// Mutation: SubmitLaunch resolving its pool through runnerPoolForTeam again
// (the team's mapping, whatever the trust) → the wire and the document carry
// "" (red on their own assertions).
func TestSubmitLaunch_AForkRunIsStampedWithItsLanesPool(t *testing.T) {
	res := &fakeTeamResolver{orgs: map[string]string{"team-a": ""}}
	p, st, published := forkLanePublisher(t, res, activeRegistry("forks"))
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	if _, err := p.SubmitLaunch(ctx, "run-fork", forkLaneSpec("forks"), &ir.Workflow{Name: "fork_review"}, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if len(*published) != 1 || (*published)[0].RunnerPool != "forks" {
		t.Fatalf("the wire must carry the lane's pool so the run lands on it alone: %+v", *published)
	}
	r, err := st.LoadRun(ctx, "run-fork")
	if err != nil {
		t.Fatal(err)
	}
	if r.RunnerPool != "forks" {
		t.Fatalf("the document froze RunnerPool %q, want the lane's \"forks\" — every resume and forked child follows it", r.RunnerPool)
	}
}

// Every refusal of the rule, on the launch: typed, and before anything is
// persisted or published.
//
// Mutations, each red on its own row and no other:
//   - the `pool == ""` guard dropped → "names no pool" publishes onto the
//     shared consumer;
//   - the grammar guard dropped → "not a pool name" reaches the registry
//     and is refused untyped (red on the typed assertion);
//   - the `teamPool != pool` guard dropped → "a mapped team names another
//     pool" publishes off its sovereign pool;
//   - the holder comparison dropped → "another team's sovereign pool"
//     publishes there;
//   - the holder read's error swallowed → "who holds the pool is unreadable"
//     publishes;
//   - the trusted arm ignoring a lane pool → "a trusted launch" publishes.
func TestSubmitLaunch_TheForkLanePoolRuleRefusesTyped(t *testing.T) {
	for _, row := range []struct {
		name      string
		res       TeamResolver
		spec      runview.LaunchSpec
		wantTyped bool
		want      string
	}{
		{
			name:      "an untrusted launch names no pool",
			res:       &fakeTeamResolver{orgs: map[string]string{"team-a": ""}},
			spec:      forkLaneSpec(""),
			wantTyped: true, want: "names no runner pool",
		},
		{
			name:      "the pool is not a pool name",
			res:       &fakeTeamResolver{orgs: map[string]string{"team-a": ""}},
			spec:      forkLaneSpec("Forks_Pool"),
			wantTyped: true, want: "is not a pool name",
		},
		{
			name: "a mapped team's fork lane names another pool",
			res: &fakeTeamResolver{orgs: map[string]string{"team-a": ""},
				runnerPool: map[string]string{"team-a": "sovereign"}},
			spec:      forkLaneSpec("forks"),
			wantTyped: true, want: `sovereign pool "sovereign"`,
		},
		{
			name: "the lane names another team's sovereign pool",
			res: &fakeTeamResolver{orgs: map[string]string{"team-a": "", "team-b": ""},
				runnerPool: map[string]string{"team-b": "forks"}},
			spec:      forkLaneSpec("forks"),
			wantTyped: true, want: "sovereign pool of team team-b",
		},
		{
			name:      "a trusted launch names a lane pool",
			res:       &fakeTeamResolver{orgs: map[string]string{"team-a": ""}},
			spec:      runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n", ForkLanePool: "forks"},
			wantTyped: true, want: "a trusted launch names the fork-lane pool",
		},
		{
			name: "who holds the pool is unreadable",
			res: &holderUnreadable{fakeTeamResolver: fakeTeamResolver{orgs: map[string]string{"team-a": ""}},
				err: errors.New("identity store down")},
			spec:      forkLaneSpec("forks"),
			wantTyped: false, want: "identity store down",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, st, published := forkLanePublisher(t, row.res, activeRegistry("forks"))
			ctx := store.WithIdentity(context.Background(), "team-a", "u1")
			_, err := p.SubmitLaunch(ctx, "run-refused", row.spec, &ir.Workflow{Name: "wf"}, &runview.CompiledSource{Hash: "hash"})
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("SubmitLaunch = %v, want a refusal naming %q", err, row.want)
			}
			if got := errors.Is(err, ErrForkLanePool); got != row.wantTyped {
				t.Fatalf("errors.Is(err, ErrForkLanePool) = %v, want %v — a definitive refusal is typed, a store that cannot answer is the transient it is", got, row.wantTyped)
			}
			if len(*published) != 0 {
				t.Fatalf("a refused launch published %d message(s)", len(*published))
			}
			if r, lerr := st.LoadRun(context.Background(), "run-refused"); lerr == nil && r.Status == store.RunStatusQueued {
				t.Fatalf("a refused launch left a queued row nothing will claim: %+v", r)
			}
		})
	}
}

// The lane's pool crosses the registry's routing gate like every pool: a
// pool that is not active is refused naming its state, and a mapped team's
// own pool — the one shape where the lane and the mapping agree — routes.
func TestSubmitLaunch_TheLanesPoolCrossesTheRegistryGate(t *testing.T) {
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	t.Run("a provisioning pool refuses", func(t *testing.T) {
		p, _, published := forkLanePublisher(t, &fakeTeamResolver{orgs: map[string]string{"team-a": ""}},
			fakePoolRegistry{rec: &platformcfg.RunnerPools{Pools: []platformcfg.RunnerPool{{Name: "forks", State: platformcfg.RunnerPoolProvisioning}}}})
		if _, err := p.SubmitLaunch(ctx, "run-prov", forkLaneSpec("forks"), &ir.Workflow{Name: "wf"}, &runview.CompiledSource{Hash: "hash"}); err == nil ||
			!strings.Contains(err.Error(), "provisioning") {
			t.Fatalf("a provisioning lane pool must refuse naming the state, got: %v", err)
		}
		if len(*published) != 0 {
			t.Fatal("a refused launch published")
		}
	})
	t.Run("a mapped team's own pool routes", func(t *testing.T) {
		p, _, published := forkLanePublisher(t, &fakeTeamResolver{orgs: map[string]string{"team-a": ""},
			runnerPool: map[string]string{"team-a": "sovereign"}}, activeRegistry("sovereign"))
		if _, err := p.SubmitLaunch(ctx, "run-own", forkLaneSpec("sovereign"), &ir.Workflow{Name: "wf"}, &runview.CompiledSource{Hash: "hash"}); err != nil {
			t.Fatalf("a mapped team's fork lane naming its own pool must route: %v", err)
		}
		if len(*published) != 1 || (*published)[0].RunnerPool != "sovereign" {
			t.Fatalf("the wire must carry the team's own pool: %+v", *published)
		}
	})
}

// A resume follows the run's FROZEN lane pool under the same rule, re-judged
// now. Before the rule, an untrusted run's stamp was compared with its team's
// mapping, so every resume of a fork run on an unmapped team read as a pool
// move (ErrPoolRemapped) and could never resume.
//
// Mutations: the resume's untrusted arm replaced by the trusted comparison →
// the first row is refused ErrPoolRemapped; forkLanePoolRefusal skipped on
// resume → the second and third rows publish.
func TestSubmitResume_AForkRunFollowsItsFrozenLanePool(t *testing.T) {
	seed := func(t *testing.T, st *store.FilesystemRunStore, pool string) runview.ResumeSpec {
		t.Helper()
		subject := string(orgusage.ForkAuthorSubject("team-a", "github", "4242"))
		if err := st.SaveRun(context.Background(), &store.Run{
			ID: "run-fork", TenantID: "team-a", OwnerID: "u1", Status: store.RunStatusFailedResumable,
			Trust: store.RunTrustFork, RepoSHAExpected: forkLaneHead, ForkAuthorSubject: subject,
			RunnerPool: pool,
		}); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		return runview.ResumeSpec{
			RunID: "run-fork", FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n",
			Admitted: &runview.GateAdmission{Trust: store.RunTrustFork, ForkAuthorSubject: subject},
		}
	}
	wf, cs := &ir.Workflow{Name: "wf"}, &runview.CompiledSource{Hash: "hash"}

	t.Run("an unmapped team's fork run resumes on its lane's pool", func(t *testing.T) {
		p, st, published := forkLanePublisher(t, &fakeTeamResolver{orgs: map[string]string{"team-a": ""}}, activeRegistry("forks"))
		if err := p.SubmitResume(context.Background(), seed(t, st, "forks"), wf, cs); err != nil {
			t.Fatalf("SubmitResume: %v — a fork run's frozen stamp is its lane's pool, not its team's mapping", err)
		}
		if len(*published) != 1 || (*published)[0].RunnerPool != "forks" {
			t.Fatalf("the resume wire must carry the frozen lane pool: %+v", *published)
		}
	})
	for _, row := range []struct {
		name string
		res  TeamResolver
		pool string
		want string
	}{
		{"a team mapped since refuses", &fakeTeamResolver{orgs: map[string]string{"team-a": ""},
			runnerPool: map[string]string{"team-a": "sovereign"}}, "forks", `sovereign pool "sovereign"`},
		{"a fork run frozen without a pool refuses", &fakeTeamResolver{orgs: map[string]string{"team-a": ""}}, "", "names no runner pool"},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, st, published := forkLanePublisher(t, row.res, activeRegistry("forks"))
			err := p.SubmitResume(context.Background(), seed(t, st, row.pool), wf, cs)
			if !errors.Is(err, ErrForkLanePool) || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("SubmitResume = %v, want ErrForkLanePool naming %q", err, row.want)
			}
			if len(*published) != 0 {
				t.Fatalf("a refused resume published %d message(s)", len(*published))
			}
			r, lerr := st.LoadRun(context.Background(), "run-fork")
			if lerr != nil {
				t.Fatal(lerr)
			}
			if r.Status != store.RunStatusFailedResumable {
				t.Fatalf("status = %s after a refused resume, want it unclaimed", r.Status)
			}
		})
	}
}
