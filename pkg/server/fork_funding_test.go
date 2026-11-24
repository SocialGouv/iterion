package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The fork run's funding, judged before metering (the operator's decision on
// #3125): a pool-stamped fork run is sealed no shared tier and spends only
// its team's own API keys, so the launch gate refuses — typed, before the
// org's counter or the author's bound moves — a fork run those keys cannot
// fund, on every carrier that meters one: the gesture's delivery, the
// delivery's durable retry and the resume of a fork run.
//
// Every witness here drives the REAL cloud publisher behind the run service:
// the gate's judge is that publisher's own unfunded decision (through the
// credential preview), and an admitted launch is sealed and published by it.

// forkFundingBot is the lane's review-pr with one model route that pins a
// provider — the route the team's own key must fund.
const forkFundingBot = "agent draft:\n  model: \"anthropic/claude-opus-5\"\n  backend: \"claw\"\n\nworkflow review_pr:\n  worktree: none\n  entry: draft\n  draft -> done\n"

// chargeRecorder records every subject the gate charges: a refusal that
// lands after the charge is refunded, so the counter's final value alone
// cannot tell "refused before metering" from "metered, then refunded".
type chargeRecorder struct {
	orgusage.Counter
	mu      sync.Mutex
	charged []orgusage.Subject
}

func (c *chargeRecorder) AllowRun(ctx context.Context, subject orgusage.Subject, when time.Time, maxRuns int, maxCostMillis int64) (orgusage.DenyReason, error) {
	c.mu.Lock()
	c.charged = append(c.charged, subject)
	c.mu.Unlock()
	return c.Counter.AllowRun(ctx, subject, when, maxRuns, maxCostMillis)
}

func (c *chargeRecorder) charges() []orgusage.Subject {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]orgusage.Subject(nil), c.charged...)
}

// activeForkPool is the sovereign-pool registry with the lane's pool active.
type activeForkPool struct{}

func (activeForkPool) Get(context.Context) (*platformcfg.RunnerPools, error) {
	return &platformcfg.RunnerPools{Pools: []platformcfg.RunnerPool{{Name: "forks", State: platformcfg.RunnerPoolActive}}}, nil
}

// forkFundingRig is the real publisher wired behind a server's run service,
// over in-memory credential stores and a run store, publishing into a slice.
type forkFundingRig struct {
	keys   secrets.ApiKeyStore
	sealer secrets.Sealer
	rs     *store.FilesystemRunStore
	mu     sync.Mutex
	msgs   []*queue.RunMessage
}

func newForkFundingRig(t *testing.T, s *Server) *forkFundingRig {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("run store: %v", err)
	}
	rig := &forkFundingRig{keys: secrets.NewMemoryApiKeyStore(), sealer: sealer, rs: rs}
	pub := cloudpublisher.NewForTesting(cloudpublisher.Config{
		Store: rs, ApiKeys: rig.keys, RunSecrets: secrets.NewMemoryRunSecretsStore(), Sealer: sealer,
		Identity: s.authStore(), RunnerPools: activeForkPool{},
		Logger: iterlog.New(iterlog.LevelError, io.Discard),
	}, func(_ context.Context, msg *queue.RunMessage) error {
		rig.mu.Lock()
		rig.msgs = append(rig.msgs, msg)
		rig.mu.Unlock()
		return nil
	})
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	return rig
}

// seedKey stores an anthropic key under tenant: "t1" is the team's own, the
// platform tenant the shared tier a fork run never spends.
func (r *forkFundingRig) seedKey(t *testing.T, tenant string) {
	t.Helper()
	id := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(r.sealer, id, []byte("sk-ant-"+tenant))
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	if err := r.keys.Create(context.Background(), secrets.ApiKey{
		ID: id, ScopeTeamID: tenant, Provider: secrets.ProviderAnthropic, Fingerprint: "fp-" + tenant,
		Name: "anthropic-" + tenant, SealedSecret: sealed, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
}

func (r *forkFundingRig) published() []*queue.RunMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*queue.RunMessage(nil), r.msgs...)
}

// forkFundingGesture is the fork-gesture fixture behind the real launcher and
// the real publisher, with the gate's charges recorded.
func forkFundingGesture(t *testing.T) (*forkGestureFixture, *forkFundingRig, *chargeRecorder) {
	t.Helper()
	fx := newForkGestureFixture(t)
	fx.s.webhookLaunchBot = nil
	if err := os.WriteFile(filepath.Join(fx.s.cfg.WorkDir, "bots", "review-pr.bot"), []byte(forkFundingBot), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &chargeRecorder{Counter: fx.counter}
	fx.s.orgUsage = rec
	return fx, newForkFundingRig(t, fx.s), rec
}

// assertUncharged: neither the org's unit nor the author's was charged —
// not even transiently.
func assertUncharged(t *testing.T, rec *chargeRecorder, counter *orgusage.MemoryCounter, author orgusage.Subject) {
	t.Helper()
	if got := rec.charges(); len(got) != 0 {
		t.Fatalf("the gate charged %v — an unfunded fork run is refused BEFORE anything is metered", got)
	}
	now := time.Now().UTC()
	for _, subj := range []orgusage.Subject{orgusage.OrgSubject("t1"), author} {
		u, err := counter.Usage(context.Background(), subj, now)
		if err != nil {
			t.Fatal(err)
		}
		if u.Runs != 0 {
			t.Fatalf("subject %s metered %d run(s)", subj, u.Runs)
		}
	}
}

var forkGestureAuthor = orgusage.ForkAuthorSubject("t1", "github", "4242")

// The gesture: a team funded only by a shared (platform) key is refused
// typed — the delivery row names fork_run_unfunded, neither counter is
// charged, nothing is published — and the same gesture on a team holding
// its own key is admitted, metered on both subjects, and published on the
// lane's pool.
//
// Mutations, each seen red and restored byte-identical (the same three
// redden the durable-retry and resume witnesses below where they apply):
//   - gateLaunch no longer asking gateForkFunding → the gesture is accepted
//     (202), metered, and left to the publisher's own refusal;
//   - gateForkFunding moved below gateMonthlyCaps, refunding on refusal →
//     the typed 422 still answers, but the charge record sees both subjects
//     charged (the counters' final values alone would read zero);
//   - webhookForkFunding returning no judge → the gesture is accepted;
//   - gateForkFunding ignoring the unfunded verdict → the gesture is
//     accepted.
func TestForkGesture_AnUnfundedTeamIsRefusedBeforeMetering(t *testing.T) {
	t.Run("a team funded only by a shared key: refused typed, nothing metered", func(t *testing.T) {
		fx, rig, rec := forkFundingGesture(t)
		rig.seedKey(t, secrets.PlatformTenantID)
		w := fx.replay(t, 4201, "/revi")
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), denyForkRunUnfunded) {
			t.Fatalf("code=%d body=%s, want 422 naming %s", w.Code, w.Body.String(), denyForkRunUnfunded)
		}
		assertUncharged(t, rec, fx.counter, forkGestureAuthor)
		if n := len(rig.published()); n != 0 {
			t.Fatalf("published %d run(s) for an unfunded fork gesture", n)
		}
		rows, err := fx.s.webhookDeliveries.ListByWebhook(context.Background(), "t1", fx.cfg.ID, 10)
		if err != nil || len(rows) != 1 || rows[0].Status != webhooks.StatusLaunchError || rows[0].Error != denyForkRunUnfunded {
			t.Fatalf("delivery rows = %+v (err %v), want one launch_error row whose reason is %q", rows, err, denyForkRunUnfunded)
		}
	})
	t.Run("a team holding its own key: admitted, metered, stamped on its pool", func(t *testing.T) {
		fx, rig, rec := forkFundingGesture(t)
		rig.seedKey(t, secrets.PlatformTenantID)
		rig.seedKey(t, "t1")
		w := fx.replay(t, 4202, "/revi")
		if w.Code != http.StatusAccepted {
			t.Fatalf("code=%d body=%s, want 202 — the team's own key funds the fork run", w.Code, w.Body.String())
		}
		msgs := rig.published()
		if len(msgs) != 1 || msgs[0].RunnerPool != "forks" || msgs[0].Trust != store.RunTrustFork {
			t.Fatalf("published %+v, want one fork run on the lane's pool", msgs)
		}
		if got := rec.charges(); len(got) != 2 {
			t.Fatalf("charged %v, want the org's subject and the author's", got)
		}
	})
}

// The delivery's durable retry crosses the same gate arm: a fork row whose
// team holds no key of its own fails its attempt with the typed reason —
// retryable, since provisioning a key cures it — without charging either
// subject or publishing.
//
// Mutation: webhookForkFunding returning no judge → the row is prepared for
// launch past the gate's charge.
func TestForkGesture_TheDurableRetryOfAnUnfundedForkRowIsRefusedBeforeMetering(t *testing.T) {
	fx, rig, rec := forkFundingGesture(t)
	rig.seedKey(t, secrets.PlatformTenantID)
	if err := fx.s.webhookConfigs.Create(context.Background(), fx.cfg); err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(-time.Hour)
	d := webhooks.Delivery{
		ID: "fork-retry", WebhookID: fx.cfg.ID, TenantID: fx.cfg.TenantID, Provider: fx.cfg.Provider,
		Status: webhooks.StatusLaunchError, IdempotencyKey: "fork-retry-idem", BotID: "review-pr",
		ProjectPath: "acme/widgets", Attempts: 1, NextAttemptAt: &due,
		RetryTarget: &webhooks.DeferredTarget{
			BotID: "review-pr", IdemKey: "fork-retry-idem",
			Vars:    map[string]string{"pr_url": "https://github.com/acme/widgets/pull/7", "head_sha": forkForkPR().HeadSHA},
			RepoURL: "https://github.com/acme/widgets.git", RepoRef: "refs/pull/7/head",
			Trust: store.RunTrustFork, ExpectedSHA: forkForkPR().HeadSHA,
			ForkAuthor: &webhooks.ForkAuthorRef{Provider: "github", AuthorID: "4242"},
		},
	}
	if err := fx.s.webhookDeliveries.Insert(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	out, launch := fx.s.prepareWebhookRetry(context.Background(), d)
	if launch != nil {
		t.Fatalf("an unfunded fork row was prepared for launch: %+v", out)
	}
	if out.denial == nil || out.denial.reason != denyForkRunUnfunded {
		t.Fatalf("retry outcome %+v, want the gate's %s denial", out, denyForkRunUnfunded)
	}
	assertUncharged(t, rec, fx.counter, forkGestureAuthor)
	if n := len(rig.published()); n != 0 {
		t.Fatalf("published %d run(s)", n)
	}
	got, err := fx.s.webhookDeliveries.Get(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != webhooks.StatusLaunchError || got.Error != denyForkRunUnfunded {
		t.Fatalf("row status=%s error=%q, want a retryable launch_error naming %s", got.Status, got.Error, denyForkRunUnfunded)
	}
}

// A bot whose route reads a routing dial — review-pr's own shape — is
// refused before metering even on a team whose own key funds the vendor
// route this server expands the dial to: the lane's pool runner expands it
// with the pool's env block, which this server cannot read. The same team
// and program on a literal route is admitted (the "holding its own key" row
// above).
//
// Mutation: the publisher's untrusted arm ignoring routes.deferred → the
// gesture is accepted (202), metered and published.
func TestForkGesture_ARoutingDialIsRefusedBeforeMetering(t *testing.T) {
	fx, rig, rec := forkFundingGesture(t)
	const dialBot = "agent draft:\n  model: \"${ITERION_FORK_GESTURE_PROBE_MODEL:-anthropic/claude-opus-5}\"\n  backend: \"claw\"\n\nworkflow review_pr:\n  worktree: none\n  entry: draft\n  draft -> done\n"
	if err := os.WriteFile(filepath.Join(fx.s.cfg.WorkDir, "bots", "review-pr.bot"), []byte(dialBot), 0o644); err != nil {
		t.Fatal(err)
	}
	rig.seedKey(t, "t1")
	w := fx.replay(t, 4211, "/revi")
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), denyForkRunUnfunded) || !strings.Contains(w.Body.String(), "decided on the runner") {
		t.Fatalf("code=%d body=%s, want 422 naming %s and the deferred route", w.Code, w.Body.String(), denyForkRunUnfunded)
	}
	assertUncharged(t, rec, fx.counter, forkGestureAuthor)
	if n := len(rig.published()); n != 0 {
		t.Fatalf("published %d run(s) for a fork gesture whose route the pool's runner decides", n)
	}
}

// A fork gesture whose bot no longer compiles is refused at the gate as the
// PERMANENT condition it is — fork_funding_unjudgeable, a 422 — never the
// retryable 503 of a judge that could not read a store, and nothing metered.
//
// Mutation: runview's FundingRefusal returning the compile error unwrapped →
// the gate answers fork_funding_unreadable (503).
//
// And a bot gone from the catalog altogether is the same permanent refusal.
// Mutation: webhookForkFunding's not-found arm returned bare → 503.
func TestForkGesture_ABotThatNoLongerCompilesIsRefusedAsPermanent(t *testing.T) {
	for _, row := range []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{"its source no longer compiles", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("agent draft:\n  model: \"anthropic/claude-opus-5\"\n\nworkflow review_pr:\n  entry: missing_node\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"it is gone from the catalog", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			fx, rig, rec := forkFundingGesture(t)
			row.write(t, filepath.Join(fx.s.cfg.WorkDir, "bots", "review-pr.bot"))
			rig.seedKey(t, "t1")
			w := fx.replay(t, 4231, "/revi")
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), denyForkFundingUnjudgeable) {
				t.Fatalf("code=%d body=%s, want 422 naming %s", w.Code, w.Body.String(), denyForkFundingUnjudgeable)
			}
			assertUncharged(t, rec, fx.counter, forkGestureAuthor)
		})
	}
}

// The fork lane's pool rule answers before metering too: a team mapped to a
// sovereign pool since its lane was created is refused typed — permanent,
// never the transient 503 — with neither counter charged and nothing
// published. Its team holds its own key, so the refusal is the pool's alone.
//
// Mutations, each seen red and restored byte-identical:
//   - the publisher's FundingRefusal no longer judging the lane's pool →
//     the gesture is metered, then refused at publish (the charge record
//     sees both subjects);
//   - gateForkFunding no longer mapping ErrForkLanePool → the transient
//     fork_funding_unreadable 503 answers.
func TestForkGesture_APoolTheLaneMayNoLongerNameIsRefusedBeforeMetering(t *testing.T) {
	fx, rig, rec := forkFundingGesture(t)
	rig.seedKey(t, "t1")
	sov := "t-sov"
	if _, err := fx.s.authStore().PatchTeam(context.Background(), "t1", identity.TeamPatch{RunnerPool: &sov}); err != nil {
		t.Fatal(err)
	}
	w := fx.replay(t, 4221, "/revi")
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), denyForkLanePoolRefused) {
		t.Fatalf("code=%d body=%s, want 422 naming %s", w.Code, w.Body.String(), denyForkLanePoolRefused)
	}
	assertUncharged(t, rec, fx.counter, forkGestureAuthor)
	if n := len(rig.published()); n != 0 {
		t.Fatalf("published %d run(s) on a pool the lane may not name", n)
	}
}

// seedFundedForkRun persists a paused fork run of team t1 on the lane's
// pool whose one model route pins anthropic, in the rig's run store and the
// server's working directory.
func seedFundedForkRun(t *testing.T, s *Server, rs *store.FilesystemRunStore) {
	t.Helper()
	path := filepath.Join(s.cfg.WorkDir, "review_pr.bot")
	if err := os.WriteFile(path, []byte(forkFundingBot), 0o644); err != nil {
		t.Fatal(err)
	}
	_, hash, err := runview.CompileWorkflowFromSource(path, forkFundingBot, "", store.RunTrustFork)
	if err != nil {
		t.Fatalf("compile under fork trust: %v", err)
	}
	ctx := context.Background()
	if _, err := rs.CreateRun(ctx, "run-fork", "review_pr", nil); err != nil {
		t.Fatal(err)
	}
	r, err := rs.LoadRun(ctx, "run-fork")
	if err != nil {
		t.Fatal(err)
	}
	r.TenantID, r.OwnerID, r.BotID, r.ProjectPath = "t1", "u1", "review-pr", "acme/widgets"
	r.FilePath, r.WorkflowSource, r.WorkflowHash = path, forkFundingBot, hash
	r.Status = store.RunStatusPausedOperator
	r.Trust, r.ForkAuthorSubject, r.RunnerPool = store.RunTrustFork, string(forkGestureAuthor), "forks"
	r.RepoSHAExpected = forkForkPR().HeadSHA
	r.PinnedProviders = []string{"anthropic"}
	if err := rs.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := rs.SaveCheckpoint(ctx, "run-fork", &store.Checkpoint{NodeID: "draft"}); err != nil {
		t.Fatal(err)
	}
}

// The resume of a fork run whose team lost its own key since the launch:
// refused typed before the resume re-meters the org and the author, nothing
// published; with the key back, the same resume is admitted and metered.
//
// Mutation: resumeLaunchSubject no longer carrying the funding judge → the
// resume is metered and refused by the publisher's own walk instead (a 422
// that names no fork_run_unfunded), alone among these witnesses.
func TestHandleResumeRun_AForkRunWhoseTeamLostItsKeyIsRefusedBeforeMetering(t *testing.T) {
	setup := func(t *testing.T) (*Server, *forkFundingRig, *chargeRecorder, context.Context, *orgusage.MemoryCounter) {
		t.Helper()
		s := newOrgTestServer(t)
		s.cfg.WorkDir = t.TempDir()
		counter := orgusage.NewMemoryCounter()
		rec := &chargeRecorder{Counter: counter}
		s.orgUsage = rec
		ctx := seedGate(t, s, gateSpec{id: "t1"})
		rig := newForkFundingRig(t, s)
		seedFundedForkRun(t, s, rig.rs)
		return s, rig, rec, ctx, counter
	}
	t.Run("the team lost its key: refused typed, nothing metered", func(t *testing.T) {
		s, rig, rec, ctx, counter := setup(t)
		rig.seedKey(t, secrets.PlatformTenantID)
		w := httptest.NewRecorder()
		s.handleResumeRun(w, orgReq(ctx, http.MethodPost, "/api/runs/run-fork/resume", `{}`, "run-fork"))
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), denyForkRunUnfunded) {
			t.Fatalf("code=%d body=%s, want 422 naming %s", w.Code, w.Body.String(), denyForkRunUnfunded)
		}
		assertUncharged(t, rec, counter, forkGestureAuthor)
		if n := len(rig.published()); n != 0 {
			t.Fatalf("published %d resume(s)", n)
		}
	})
	t.Run("the key is back: the resume is admitted and metered", func(t *testing.T) {
		s, rig, rec, ctx, _ := setup(t)
		rig.seedKey(t, "t1")
		w := httptest.NewRecorder()
		s.handleResumeRun(w, orgReq(ctx, http.MethodPost, "/api/runs/run-fork/resume", `{}`, "run-fork"))
		if w.Code >= 300 {
			t.Fatalf("code=%d body=%s, want the funded fork run resumed", w.Code, w.Body.String())
		}
		if msgs := rig.published(); len(msgs) != 1 || msgs[0].RunnerPool != "forks" {
			t.Fatalf("published %+v, want the resume on the frozen lane pool", msgs)
		}
		if got := rec.charges(); len(got) != 2 {
			t.Fatalf("charged %v, want the org's subject and the author's", got)
		}
	})
}

// unfundedForkResume is the resume rig of the two other gated resume
// carriers: the real publisher behind the run service, the fork run of
// seedFundedForkRun, the gate's charges recorded — and, under the team, only
// a platform key, which funds no fork run (funded: the team's own key).
func unfundedForkResume(t *testing.T, funded bool) (*Server, *forkFundingRig, *chargeRecorder, *orgusage.MemoryCounter, context.Context) {
	t.Helper()
	s := newOrgTestServer(t)
	s.cfg.WorkDir = t.TempDir()
	counter := orgusage.NewMemoryCounter()
	rec := &chargeRecorder{Counter: counter}
	s.orgUsage = rec
	ctx := seedGate(t, s, gateSpec{id: "t1"})
	rig := newForkFundingRig(t, s)
	seedFundedForkRun(t, s, rig.rs)
	if funded {
		rig.seedKey(t, "t1")
	} else {
		rig.seedKey(t, secrets.PlatformTenantID)
	}
	return s, rig, rec, counter, ctx
}

// The retry sweeper's resume of a fork run is judged by the same funding
// judge before it meters: a fork run whose team lost its own key since the
// launch is refused unmetered, and its retry ABANDONED — fork_run_unfunded is
// permanent until someone provisions a key. With the key back, the same
// retry is admitted, metered on both counters and resumed.
//
// Mutation: resumeDueRetry's subject stripped of its funding judge
// (`subj.Funding = nil` after resumeLaunchSubject) → the retry is metered on
// the org and the author and handed to the resumer (red on the abandon and
// the uncharged assertions).
func TestSweepDueRetries_AnUnfundedForkRunIsRefusedBeforeMetering(t *testing.T) {
	due := func() mongostore.RetryDueRef {
		ref := dueRef("run-fork", time.Now().UTC().Add(-time.Minute))
		ref.TenantID, ref.OwnerID = "t1", "u1"
		return ref
	}
	// A run under retry is failed_resumable, the status the sweeper resumes.
	retryable := func(t *testing.T, s *Server, rig *forkFundingRig) {
		t.Helper()
		r, err := rig.rs.LoadRun(context.Background(), "run-fork")
		if err != nil {
			t.Fatal(err)
		}
		r.Status = store.RunStatusFailedResumable
		if err := rig.rs.SaveRun(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		s.cfg.Store = rig.rs
	}
	t.Run("the team lost its key: abandoned, nothing metered", func(t *testing.T) {
		s, rig, rec, counter, _ := unfundedForkResume(t, false)
		retryable(t, s, rig)
		st := newFakeRetryStore()
		st.claimWins["run-fork"] = true
		resumer := &fakeResumer{}
		s.resumeDueRetry(context.Background(), st, resumer, due())
		if reason, ok := st.abandoned["run-fork"]; !ok || !strings.Contains(reason, denyForkRunUnfunded) {
			t.Fatalf("abandoned=%v re-armed=%v, want the retry abandoned naming %s", st.abandoned, st.rearmed, denyForkRunUnfunded)
		}
		assertUncharged(t, rec, counter, forkGestureAuthor)
		if len(resumer.calls) != 0 {
			t.Fatalf("the sweeper resumed %d time(s) a fork run its team cannot fund", len(resumer.calls))
		}
	})
	t.Run("the key is back: the retry is metered and resumed", func(t *testing.T) {
		s, rig, rec, _, _ := unfundedForkResume(t, true)
		retryable(t, s, rig)
		st := newFakeRetryStore()
		st.claimWins["run-fork"] = true
		resumer := &fakeResumer{}
		s.resumeDueRetry(context.Background(), st, resumer, due())
		if len(resumer.calls) != 1 {
			t.Fatalf("resumer calls=%d abandoned=%v re-armed=%v, want the funded fork run resumed", len(resumer.calls), st.abandoned, st.rearmed)
		}
		if got := rec.charges(); len(got) != 2 {
			t.Fatalf("charged %v, want the org's subject and the author's", got)
		}
	})
}

// The WS answer twin, the gated surface that resumes a paused_waiting_human
// run: a fork run whose team lost its own key is refused with
// fork_run_unfunded before the answer meters anything, and nothing reaches the
// publisher.
//
// Mutation: handleAnswer's subject stripped of its funding judge
// (`subj.Funding = nil` after resumeLaunchSubject) → the answer is metered
// before anything refuses it (red on the error code and the uncharged
// assertion).
func TestRunsWS_HandleAnswer_AnUnfundedForkRunIsRefusedBeforeMetering(t *testing.T) {
	s, rig, rec, counter, ctx := unfundedForkResume(t, false)
	id, _ := auth.FromContext(ctx)
	c := &runConn{server: s, runID: "run-fork", sendCh: make(chan []byte, 8), closed: make(chan struct{}),
		tenantID: id.TeamID, userID: id.UserID, identity: id}
	payload, err := json.Marshal(map[string]any{"answers": map[string]any{"q": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	c.handleAnswer(runWSEnvelope{Type: wsTypeAnswer, AckID: "a", Payload: payload})
	if got := firstWSError(t, c).Code; got != denyForkRunUnfunded {
		t.Fatalf("error code = %q, want %q — a fork run its team cannot fund is refused before the answer meters", got, denyForkRunUnfunded)
	}
	assertUncharged(t, rec, counter, forkGestureAuthor)
	if n := len(rig.published()); n != 0 {
		t.Fatalf("published %d resume(s)", n)
	}
}

// A fork run whose program can no longer be built — its bot file is gone —
// is refused at the resume gate as the PERMANENT condition it is: a 422
// naming fork_funding_unjudgeable, never the retryable 503 a store blip
// answers, and nothing metered.
//
// Mutation: runview's ResumeFundingRefusal returning the compile error
// unwrapped → the gate answers fork_funding_unreadable (503).
func TestHandleResumeRun_AForkRunWhoseProgramIsGoneIsRefusedAsPermanent(t *testing.T) {
	s := newOrgTestServer(t)
	s.cfg.WorkDir = t.TempDir()
	counter := orgusage.NewMemoryCounter()
	rec := &chargeRecorder{Counter: counter}
	s.orgUsage = rec
	ctx := seedGate(t, s, gateSpec{id: "t1"})
	rig := newForkFundingRig(t, s)
	seedFundedForkRun(t, s, rig.rs)
	rig.seedKey(t, "t1")
	if err := os.Remove(filepath.Join(s.cfg.WorkDir, "review_pr.bot")); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleResumeRun(w, orgReq(ctx, http.MethodPost, "/api/runs/run-fork/resume", `{}`, "run-fork"))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), denyForkFundingUnjudgeable) {
		t.Fatalf("code=%d body=%s, want 422 naming %s", w.Code, w.Body.String(), denyForkFundingUnjudgeable)
	}
	assertUncharged(t, rec, counter, forkGestureAuthor)
	if n := len(rig.published()); n != 0 {
		t.Fatalf("published %d resume(s)", n)
	}
}

// The cloud twin: the stored bot a fork run launched from is gone, so its
// resume source never resolves — the retry sweeper's own "a bot removed from
// the catalog will never resolve" — and the gate refuses it as permanent.
//
// Mutation: forkResumeFunding no longer classifying a non-transient filler
// failure (every failure returned bare) → fork_funding_unreadable (503).
func TestHandleResumeRun_AForkRunWhoseStoredBotIsGoneIsRefusedAsPermanent(t *testing.T) {
	s := newOrgTestServer(t)
	s.cfg.WorkDir = t.TempDir()
	counter := orgusage.NewMemoryCounter()
	rec := &chargeRecorder{Counter: counter}
	s.orgUsage = rec
	ctx := seedGate(t, s, gateSpec{id: "t1"})
	rig := newForkFundingRig(t, s)
	seedFundedForkRun(t, s, rig.rs)
	rig.seedKey(t, "t1")
	r, err := rig.rs.LoadRun(context.Background(), "run-fork")
	if err != nil {
		t.Fatal(err)
	}
	r.BotSourceTenant = "t1"
	if err := rig.rs.SaveRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	s.cfg.Mode = "cloud"
	w := httptest.NewRecorder()
	s.handleResumeRun(w, orgReq(ctx, http.MethodPost, "/api/runs/run-fork/resume", `{}`, "run-fork"))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), denyForkFundingUnjudgeable) {
		t.Fatalf("code=%d body=%s, want 422 naming %s", w.Code, w.Body.String(), denyForkFundingUnjudgeable)
	}
	assertUncharged(t, rec, counter, forkGestureAuthor)
}

// The credential preview of a fork-lane webhook answers for the fork lane's
// launch — the fork's trust, the lane's pool — so it prints the very verdict
// the gate refuses on: on a team funded only by a shared key, the live launch
// refuses, naming the untrusted posture and the pool.
//
// Mutation: the handler judging a fork-lane config as a trusted launch (its
// fork-lane branch removed) → the platform key funds the previewed run and
// no refusal is printed.
func TestCredentialPreview_AForkLaneWebhookPrintsTheGatesFundingVerdict(t *testing.T) {
	fx, rig, _ := forkFundingGesture(t)
	rig.seedKey(t, secrets.PlatformTenantID)
	if err := fx.s.webhookConfigs.Create(context.Background(), fx.cfg); err != nil {
		t.Fatal(err)
	}
	w := callCredentialPreview(fx.s, `{"source":{"kind":"webhook","id":"`+fx.cfg.ID+`"},"bot_id":"review-pr"}`,
		auth.Identity{UserID: "human", TeamID: "t1", Role: identity.RoleMember})
	if w.Code != http.StatusOK {
		t.Fatalf("preview=%d %s", w.Code, w.Body.String())
	}
	var out runview.CredentialPreview
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, warning := range out.Warnings {
		if strings.HasPrefix(warning, "The live launch refuses: ") && strings.Contains(warning, "untrusted (fork-lane) run on runner pool \"forks\"") {
			return
		}
	}
	t.Fatalf("warnings %q, want the live launch's untrusted refusal on the lane's pool", out.Warnings)
}
