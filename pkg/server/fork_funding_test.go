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
