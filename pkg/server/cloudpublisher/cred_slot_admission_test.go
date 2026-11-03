package cloudpublisher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The HARD per-key concurrency cap, through the publisher's own entry
// points: the admission after the walk (cred_slots.go) is what the walk's
// skip cannot be — atomic across publishers, honoured by a pinned key, and
// honoured by the refused-key restore — and a full key PARKS the run with a
// durable retry instead of refusing it.

// armedRetry is one ScheduleRunRetry call the spy saw.
type armedRetry struct {
	at          time.Time
	reason      string
	code        string
	maxAttempts int
}

// ledgerRetrySpy wraps the filesystem store — which hosts the slot ledger
// and the queued flip — and adds the durable retry surface, recording every
// arming. Unwrap hands the capability probes through to the store it wraps.
type ledgerRetrySpy struct {
	store.RunStore
	mu    sync.Mutex
	armed map[string]armedRetry
}

func newLedgerRetrySpy(t *testing.T) *ledgerRetrySpy {
	t.Helper()
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return &ledgerRetrySpy{RunStore: base, armed: map[string]armedRetry{}}
}

func (s *ledgerRetrySpy) Unwrap() store.RunStore { return s.RunStore }

func (s *ledgerRetrySpy) ScheduleRunRetry(_ context.Context, runID string, at, _ time.Time, reason, code string, maxAttempts int) (bool, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed[runID] = armedRetry{at: at, reason: reason, code: code, maxAttempts: maxAttempts}
	return true, 1, nil
}

func (s *ledgerRetrySpy) DelayRunRetry(context.Context, string, time.Time, time.Time, string, string) (bool, error) {
	return false, nil
}
func (s *ledgerRetrySpy) PullRunRetry(context.Context, string, time.Time, time.Time) (bool, error) {
	return false, nil
}
func (s *ledgerRetrySpy) RearmProbedRunRetry(context.Context, string, time.Time, time.Time) (bool, error) {
	return false, nil
}
func (s *ledgerRetrySpy) ClaimRunRetry(context.Context, string, time.Time) (bool, error) {
	return true, nil
}
func (s *ledgerRetrySpy) ClearRunRetry(context.Context, string) error { return nil }
func (s *ledgerRetrySpy) AbandonRunRetry(context.Context, string, string, time.Time) (bool, error) {
	return true, nil
}

func (s *ledgerRetrySpy) armedFor(runID string) (armedRetry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.armed[runID]
	return a, ok
}

// publishedRuns counts the queue messages a publisher emitted, by run.
type publishedRuns struct {
	mu   sync.Mutex
	runs []string
}

func (p *publishedRuns) publish(_ context.Context, m *queue.RunMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs = append(p.runs, m.RunID)
	return nil
}

func (p *publishedRuns) ids() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.runs...)
}

// seedCappedKey stores a team key with a hard concurrency cap and returns its id.
func seedCappedKey(t *testing.T, keys secrets.ApiKeyStore, sealer secrets.Sealer, teamID string, provider secrets.Provider, plaintext, fp, name string, max int) string {
	t.Helper()
	id := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, id, []byte(plaintext))
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	if err := keys.Create(context.Background(), secrets.ApiKey{
		ID: id, ScopeTeamID: teamID, Provider: provider, Fingerprint: fp, Name: name,
		SealedSecret: sealed, CreatedAt: time.Now().UTC(), MaxConcurrentRuns: max,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return id
}

// cappedKeyPublisher builds a publisher over a shared store and key set; two
// of them on one store are two replicas.
func cappedKeyPublisher(rs store.RunStore, keys secrets.ApiKeyStore, sealer secrets.Sealer, caps usagecap.Store, published *publishedRuns) *Publisher {
	return &Publisher{
		apiKeys:    keys,
		usageCaps:  caps,
		store:      rs,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		logger:     testLogger(),
		publishRun: published.publish,
	}
}

// cappedWorkflow has one unpinned model node, so the stamp keeps every key
// the walk seals.
func cappedWorkflow() *ir.Workflow {
	return &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{"implement": &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "implement"}, LLMFields: ir.LLMFields{Backend: "claw"}, Tools: []string{},
	}}}
}

func launchSpec(overrides map[string]string) runview.LaunchSpec {
	return runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n", BotID: "docs-refresh", KeyOverrides: overrides}
}

type cappedFixture struct {
	spy       *ledgerRetrySpy
	keys      *secrets.MemoryApiKeyStore
	sealer    secrets.Sealer
	caps      usagecap.Store
	published *publishedRuns
	ctx       context.Context
	keyID     string
}

const (
	cappedTeam    = "team-capped"
	cappedFP      = "fp-capped"
	cappedKeyName = "anthropic-capped"
)

func newCappedFixture(t *testing.T, max int) *cappedFixture {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	f := &cappedFixture{
		spy:       newLedgerRetrySpy(t),
		keys:      secrets.NewMemoryApiKeyStore(),
		sealer:    sealer,
		caps:      usagecap.NewMemStore(),
		published: &publishedRuns{},
		ctx:       store.WithIdentity(context.Background(), cappedTeam, "owner1"),
	}
	f.keyID = seedCappedKey(t, f.keys, sealer, cappedTeam, secrets.ProviderAnthropic, "sk-capped", cappedFP, cappedKeyName, max)
	return f
}

func (f *cappedFixture) publisher() *Publisher {
	return cappedKeyPublisher(f.spy, f.keys, f.sealer, f.caps, f.published)
}

func (f *cappedFixture) launch(t *testing.T, p *Publisher, runID string, overrides map[string]string) error {
	t.Helper()
	_, err := p.SubmitLaunch(f.ctx, runID, launchSpec(overrides), cappedWorkflow(), &runview.CompiledSource{Hash: "hash"})
	return err
}

// launchSub is launch with the publisher's own word on where the run landed.
func (f *cappedFixture) launchSub(t *testing.T, p *Publisher, runID string, overrides map[string]string) runview.LaunchSubmission {
	t.Helper()
	sub, err := p.SubmitLaunch(f.ctx, runID, launchSpec(overrides), cappedWorkflow(), &runview.CompiledSource{Hash: "hash"})
	if err != nil {
		t.Fatalf("launch %s: %v", runID, err)
	}
	return sub
}

func (f *cappedFixture) load(t *testing.T, runID string) *store.Run {
	t.Helper()
	r, err := f.spy.LoadRun(f.ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", runID, err)
	}
	return r
}

// assertParked checks the whole park: the row, its words, and the armed retry.
func (f *cappedFixture) assertParked(t *testing.T, runID string) {
	t.Helper()
	r := f.load(t, runID)
	if r.Status != store.RunStatusFailedResumable || r.FailureCode != store.FailureCredSlotWait {
		t.Fatalf("run %s: status=%s code=%s, want failed_resumable/%s — a full key must PARK the run", runID, r.Status, r.FailureCode, store.FailureCredSlotWait)
	}
	if !strings.Contains(r.Error, cappedKeyName) {
		t.Fatalf("run %s: error %q does not name the full key %q", runID, r.Error, cappedKeyName)
	}
	armed, ok := f.spy.armedFor(runID)
	if !ok {
		t.Fatalf("run %s: parked with no retry armed — nothing would ever wake it", runID)
	}
	if armed.reason != store.RetryReasonCredSlot || armed.code != string(store.FailureCredSlotWait) {
		t.Fatalf("run %s: retry armed as reason=%q code=%q, want %q/%s", runID, armed.reason, armed.code, store.RetryReasonCredSlot, store.FailureCredSlotWait)
	}
	if until := time.Until(armed.at); until <= 0 || until > CredSlotProbeInterval+time.Minute {
		t.Fatalf("run %s: retry armed %s from now, want one slot probe interval (%s)", runID, until, CredSlotProbeInterval)
	}
	for _, id := range f.published.ids() {
		if id == runID {
			t.Fatalf("run %s was parked AND published — the queue would run it past the cap", runID)
		}
	}
}

func (f *cappedFixture) assertQueued(t *testing.T, runID string) {
	t.Helper()
	r := f.load(t, runID)
	if r.Status != store.RunStatusQueued {
		t.Fatalf("run %s: status=%s (%s), want queued", runID, r.Status, r.Error)
	}
	for _, id := range f.published.ids() {
		if id == runID {
			return
		}
	}
	t.Fatalf("run %s is queued but no message was published for it", runID)
}

// Two replicas launching at once on a key with one slot: exactly one run is
// queued, every other is parked. The walk's own count reads the run
// documents before any of them is written, so every racer reads zero — the
// ledger's conditional write is what decides. Red when the admission step
// is dropped (every racer is queued) and when the reservation is not
// atomic.
func TestCredSlotAdmission_ReplicasRacingForTheLastSlotQueueExactlyOne(t *testing.T) {
	f := newCappedFixture(t, 1)
	replicas := []*Publisher{f.publisher(), f.publisher()}
	const racers = 6
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = f.launch(t, replicas[i%len(replicas)], runIDFor(i), nil)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("launch %d: %v", i, err)
		}
	}
	if got := f.published.ids(); len(got) != 1 {
		t.Fatalf("published %v, want exactly one run on a cap of one", got)
	}
	queued, parked := 0, 0
	for i := 0; i < racers; i++ {
		switch r := f.load(t, runIDFor(i)); r.Status {
		case store.RunStatusQueued:
			queued++
			f.assertQueued(t, runIDFor(i))
		case store.RunStatusFailedResumable:
			parked++
			f.assertParked(t, runIDFor(i))
		default:
			t.Fatalf("run %s: status %s (%s)", runIDFor(i), r.Status, r.Error)
		}
	}
	if queued != 1 || parked != racers-1 {
		t.Fatalf("queued=%d parked=%d, want 1 and %d", queued, parked, racers-1)
	}
}

func runIDFor(i int) string { return "run-racer-" + string(rune('a'+i)) }

// The second launch on a full key is parked, not refused and not queued —
// and it reaches the admission through the RESTORE: the walk's skip passes
// the full key over, nothing else serves the wire, the restore hands it
// back (a parked run with a durable retry beats one with no credential), and
// the admission parks the run on it. Red when the restored key skips the
// admission.
func TestCredSlotAdmission_ASecondLaunchOnAFullKeyIsParked(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	if sub := f.launchSub(t, p, "run-first", nil); sub.Status != store.RunStatusQueued {
		t.Fatalf("first launch submission = %+v, want status queued", sub)
	}
	f.assertQueued(t, "run-first")
	// A full key parks the run, it does not refuse the launch — and the
	// submission SAYS so: a park read off a zero queue position would be
	// reported as a run that runs.
	sub := f.launchSub(t, p, "run-second", nil)
	if sub.Status != store.RunStatusFailedResumable || sub.FailureCode != store.FailureCredSlotWait {
		t.Fatalf("second launch submission = %+v, want failed_resumable/%s — the park must travel with the answer", sub, store.FailureCredSlotWait)
	}
	f.assertParked(t, "run-second")
	// The parked run holds no slot: a slot freed later goes to the next
	// launch, and the parked run's resume asks again.
	r := f.load(t, "run-second")
	if len(r.CredFingerprints) == 0 || r.CredFingerprints[0] != cappedFP {
		t.Fatalf("the parked run's stamp %v does not name the key it waits for", r.CredFingerprints)
	}
}

// A key the provider refused — restored because nothing else serves the
// wire — is admitted against its cap like a chosen one.
func TestCredSlotAdmission_ARestoredRefusedKeyIsAdmittedAgainstItsCap(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	if err := f.launch(t, p, "run-first", nil); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	recordRefusal(t, f.caps, usagecap.TenantScope(cappedTeam), cappedFP)
	if err := f.launch(t, p, "run-restored", nil); err != nil {
		t.Fatalf("launch on a restored key: %v", err)
	}
	f.assertParked(t, "run-restored")
}

// A key_overrides pin bypasses the walk's skip by design; it does not bypass
// the cap. Red when the admission is skipped for a pinned key.
func TestCredSlotAdmission_APinnedKeyIsAdmittedAgainstItsCap(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	pin := map[string]string{string(secrets.ProviderAnthropic): f.keyID}
	if err := f.launch(t, p, "run-pinned-first", pin); err != nil {
		t.Fatalf("first pinned launch: %v", err)
	}
	f.assertQueued(t, "run-pinned-first")
	if err := f.launch(t, p, "run-pinned-second", pin); err != nil {
		t.Fatalf("second pinned launch: %v", err)
	}
	f.assertParked(t, "run-pinned-second")
}

// An uncapped key reserves nothing: two launches, two queued runs.
func TestCredSlotAdmission_AnUncappedKeyReservesNothing(t *testing.T) {
	f := newCappedFixture(t, 0)
	p := f.publisher()
	for _, id := range []string{"run-u1", "run-u2"} {
		if err := f.launch(t, p, id, nil); err != nil {
			t.Fatalf("launch %s: %v", id, err)
		}
		f.assertQueued(t, id)
	}
}

// park releases the run's slot: a resume re-reserves what the run holds —
// the run's own entry is admitted again without a second slot — and a run
// whose slot another run took in the meantime stays parked with the typed
// refusal, rolled back to where it was.
func TestCredSlotAdmission_AResumeReadmitsTheRunsOwnSlot(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	if err := f.launch(t, p, "run-a", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	f.park(t, "run-a")
	if err := f.resume(t, p, "run-a"); err != nil {
		t.Fatalf("resume of the slot holder: %v — the run's own slot must be admitted again", err)
	}
	f.assertQueued(t, "run-a")
	if got := f.published.ids(); len(got) != 2 {
		t.Fatalf("published %v, want the launch and the resume", got)
	}
}

func TestCredSlotAdmission_AResumeOnAKeyFullOfOtherRunsStaysParked(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	if err := f.launch(t, p, "run-a", nil); err != nil {
		t.Fatalf("launch run-a: %v", err)
	}
	f.park(t, "run-a")
	// The parked run spends nothing: the next launch takes the slot.
	if err := f.launch(t, p, "run-b", nil); err != nil {
		t.Fatalf("launch run-b: %v", err)
	}
	f.assertQueued(t, "run-b")
	err := f.resume(t, p, "run-a")
	if !errors.Is(err, ErrCredentialAtCapacity) {
		t.Fatalf("resume on a full key: err=%v, want ErrCredentialAtCapacity — the sweeper keys its free deferral on it", err)
	}
	if r := f.load(t, "run-a"); r.Status != store.RunStatusFailedResumable {
		t.Fatalf("run-a status=%s after a refused resume, want failed_resumable (rolled back)", r.Status)
	}
	if got := f.published.ids(); len(got) != 2 {
		t.Fatalf("published %v, want run-a's launch and run-b's only", got)
	}
}

// park flips a queued run to failed_resumable the way the runner's park does
// for the store's purposes: the status is what releases the slot.
func (f *cappedFixture) park(t *testing.T, runID string) {
	t.Helper()
	r := f.load(t, runID)
	r.Status = store.RunStatusFailedResumable
	if err := f.spy.SaveRun(f.ctx, r); err != nil {
		t.Fatalf("park %s: %v", runID, err)
	}
}

func (f *cappedFixture) resume(t *testing.T, p *Publisher, runID string) error {
	t.Helper()
	return p.SubmitResume(f.ctx, runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}, cappedWorkflow(), &runview.CompiledSource{Hash: "hash"})
}

// failingLedger refuses every reservation with a store error.
type failingLedger struct {
	store.RunStore
}

func (f *failingLedger) Unwrap() store.RunStore { return f.RunStore }
func (f *failingLedger) ReserveCredSlot(context.Context, string, string, int) (store.CredSlotAdmission, error) {
	return store.CredSlotAdmission{}, errors.New("ledger unavailable")
}
func (f *failingLedger) ReleaseCredSlot(context.Context, string, string) error { return nil }

// A ledger error refuses the launch and persists nothing: a hard cap that
// fails open on a blip is the advisory ceiling this step replaces.
func TestCredSlotAdmission_ALedgerErrorRefusesTheLaunch(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	p.store = &failingLedger{RunStore: f.spy}
	err := f.launch(t, p, "run-blip", nil)
	if err == nil || !strings.Contains(err.Error(), "ledger unavailable") {
		t.Fatalf("launch err=%v, want the ledger error — a cap that fails open is no cap", err)
	}
	if _, lerr := f.spy.LoadRun(f.ctx, "run-blip"); !errors.Is(lerr, store.ErrRunNotFound) {
		t.Fatalf("a refused launch persisted a row (load err=%v)", lerr)
	}
	if got := f.published.ids(); len(got) != 0 {
		t.Fatalf("a refused launch published %v", got)
	}
}

// noLedgerStore carries no slot ledger and hides the one it wraps.
type noLedgerStore struct {
	store.RunStore
}

// A store that cannot host the ledger cannot enforce a cap: the launch of a
// capped key is refused by name, never admitted on the walk's count alone.
func TestCredSlotAdmission_AStoreWithoutTheLedgerRefusesACappedKey(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	p.store = &noLedgerStore{RunStore: f.spy.RunStore}
	err := f.launch(t, p, "run-noledger", nil)
	if err == nil || !strings.Contains(err.Error(), "CredSlotStore") {
		t.Fatalf("launch err=%v, want a refusal naming the missing ledger", err)
	}
}

// A launch that names staged uploads and lands on a full key promotes them
// BEFORE it parks: the park's resume is the sweeper's, minutes to hours
// later, when staging may be reaped, and the runner reads attachments off
// the run document alone — a parked run resumed without the files its
// launch named would run without them in silence. The promotion runs once the parked row
// exists, since the store reflects attachments onto the run document. Red
// when the park returns before the promotion.
func TestCredSlotAdmission_AParkedLaunchPromotesItsAttachmentsFirst(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	if err := f.launch(t, p, "run-first", nil); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	var promotedFor []string
	var rowAtPromotion store.RunStatus
	spec := launchSpec(nil)
	spec.AttachmentPromote = func(_ context.Context, runID string) error {
		promotedFor = append(promotedFor, runID)
		r, err := f.spy.LoadRun(f.ctx, runID)
		if err != nil {
			return fmt.Errorf("the run document must exist when its attachments are promoted: %w", err)
		}
		rowAtPromotion = r.Status
		return nil
	}
	sub, err := p.SubmitLaunch(f.ctx, "run-parked", spec, cappedWorkflow(), &runview.CompiledSource{Hash: "hash"})
	if err != nil {
		t.Fatalf("launch on a full key: %v", err)
	}
	if sub.Status != store.RunStatusFailedResumable {
		t.Fatalf("submission = %+v, want a park", sub)
	}
	f.assertParked(t, "run-parked")
	if len(promotedFor) != 1 || promotedFor[0] != "run-parked" {
		t.Fatalf("attachments promoted for %v, want the parked run exactly once — the sweeper's resume runs the bot without the files its launch named", promotedFor)
	}
	if rowAtPromotion != store.RunStatusFailedResumable {
		t.Fatalf("the row read %q at promotion, want the parked row already persisted", rowAtPromotion)
	}
}

// A promotion that fails fails the parked launch exactly like a queued one:
// the row is LAUNCH_FAILED with the cause and no retry is armed on it — the
// sweeper would otherwise resume a run whose attachments never landed.
func TestCredSlotAdmission_AParkedLaunchWhosePromotionFailsIsLaunchFailed(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	if err := f.launch(t, p, "run-first", nil); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	spec := launchSpec(nil)
	spec.AttachmentPromote = func(context.Context, string) error { return errors.New(`upload "u1" not found`) }
	_, err := p.SubmitLaunch(f.ctx, "run-parked", spec, cappedWorkflow(), &runview.CompiledSource{Hash: "hash"})
	if err == nil || !strings.Contains(err.Error(), `upload "u1" not found`) {
		t.Fatalf("launch err=%v, want the promotion's own error", err)
	}
	r := f.load(t, "run-parked")
	if r.Status != store.RunStatusFailed || r.FailureCode != store.FailureLaunchFailed {
		t.Fatalf("run-parked: status=%s code=%s, want failed/%s — a park whose attachments never landed must not wait to be resumed", r.Status, r.FailureCode, store.FailureLaunchFailed)
	}
	if _, armed := f.spy.armedFor("run-parked"); armed {
		t.Fatal("a retry was armed on a run whose promotion failed")
	}
	for _, id := range f.published.ids() {
		if id == "run-parked" {
			t.Fatal("a launch whose promotion failed was published")
		}
	}
}

// A resume whose publish reports an error AFTER the message landed keeps its
// slot: the runner claims and executes that attempt, the rollback's CAS finds
// the run no longer queued and reverts nothing, and a slot given back under a
// run that runs lets the next launch take it — the cap exceeded by one until
// the run ends. Red when the release fires on every failed resume instead of
// following the rollback's own verdict.
func TestCredSlotAdmission_AResumeWhosePublishLandedKeepsItsSlot(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	if err := f.launch(t, p, "run-a", nil); err != nil {
		t.Fatalf("launch run-a: %v", err)
	}
	f.park(t, "run-a")
	// The message lands and the runner claims the attempt before the
	// publisher hears back: the row is running when the error returns.
	p.publishRun = func(_ context.Context, m *queue.RunMessage) error {
		r := f.load(t, m.RunID)
		r.Status = store.RunStatusRunning
		if err := f.spy.SaveRun(f.ctx, r); err != nil {
			t.Fatalf("claim %s: %v", m.RunID, err)
		}
		return errors.New("ack timeout after the broker accepted the message")
	}
	if err := f.resume(t, p, "run-a"); err == nil {
		t.Fatal("a resume whose publish reported an error returned nil")
	}
	if r := f.load(t, "run-a"); r.Status != store.RunStatusRunning {
		t.Fatalf("run-a status=%s after the publish error, want running — the rollback must not revert a claimed attempt", r.Status)
	}
	// The slot is still run-a's: the next launch on the key parks.
	p.publishRun = f.published.publish
	if err := f.launch(t, p, "run-b", nil); err != nil {
		t.Fatalf("launch run-b: %v", err)
	}
	f.assertParked(t, "run-b")
}

// A key lent through the credential pool is admitted against ITS cap: the
// grant carries the donor record's max_concurrent_runs and label, and the
// walk notes the lent key exactly like a key of the tenant's own. Red when
// the pool site stamps the fingerprint for the meter without noting it —
// the donor's alive_runs would count the borrower's runs the ledger never
// admitted.
func TestCredSlotAdmission_APoolLentKeyIsNotedAgainstItsOwnCap(t *testing.T) {
	ctx := context.Background()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	apiKeys := secrets.NewMemoryApiKeyStore()
	keyID := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, keyID, []byte("xai-donated-key"))
	if err != nil {
		t.Fatal(err)
	}
	lentFP := secrets.FingerprintSHA256("xai-donated-key")
	if err := apiKeys.Create(store.WithTenant(ctx, "donor-team"), secrets.ApiKey{
		ID: keyID, TenantID: "donor-team", ScopeTeamID: "donor-team", ScopeUserID: "donor",
		Provider: secrets.ProviderXAI, Name: "lent-capped", SealedSecret: sealed, Fingerprint: lentFP,
		MaxConcurrentRuns: 2,
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceAPIKey, "xai"), PoolID: "pool-1", UserID: "donor",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "xai", KeyID: keyID},
		Enabled:    true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5},
	}); err != nil {
		t.Fatalf("seed pledge: %v", err)
	}
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: credpool.NewMemoryLeaseStore(), Ledger: credpool.NewMemoryLedger(),
		OAuth: secrets.NewMemoryOAuthStore(), APIKeys: apiKeys, Sealer: sealer, Logger: testLogger(),
	})
	p := &Publisher{runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, credPool: broker, logger: testLogger()}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "xai", Model: "xai/grok-4"},
	}}}
	creds, err := p.resolveAndSealCredentials(store.WithTenant(ctx, poolTeam), "run-lent", poolOrg, poolTeam, "requester", "bot", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if creds.grant == nil || creds.grant.Source != credpool.SourceAPIKey {
		t.Fatalf("want an api_key grant, got %+v", creds.grant)
	}
	if len(creds.credSlots) != 1 {
		t.Fatalf("admission wants = %+v, want exactly the lent key", creds.credSlots)
	}
	if w := creds.credSlots[0]; w.fingerprint != lentFP || w.max != 2 || w.provider != secrets.ProviderXAI || w.name != "lent-capped" {
		t.Fatalf("lent key noted as %+v, want fingerprint=%s max=2 provider=xai name=lent-capped — the donor record's own cap and label", w, lentFP)
	}
}

// ledgerHolders counts the runs holding a slot of fp right now without
// running the ledger's reconcile: a probe reservation on a cap no row
// reaches is never refused, so it evicts nobody, and it gives its slot back
// at once.
func (f *cappedFixture) ledgerHolders(t *testing.T, fp string) int {
	t.Helper()
	ledger := store.AsCredSlotStore(f.spy)
	const probe = "probe-holders"
	adm, err := ledger.ReserveCredSlot(f.ctx, fp, probe, 1<<20)
	if err != nil || !adm.Granted {
		t.Fatalf("probe the holders of %s: %+v %v", fp, adm, err)
	}
	if err := ledger.ReleaseCredSlot(f.ctx, fp, probe); err != nil {
		t.Fatalf("release the probe of %s: %v", fp, err)
	}
	return adm.Held - 1
}

// A launch whose publish reports an error AFTER the message landed and a
// runner claimed it keeps its slot, and its claimed row: the rollback ends
// the row only while it is still queued for this launch, and gives the
// slots back only with that end. Red when the release fires on every
// unpublished exit (the next launch is queued past the cap), and when the
// rollback fails the claimed row unconditionally (the row reads failed
// under a run that executes, and the next refusal's reconcile evicts it).
func TestCredSlotAdmission_ALaunchWhosePublishLandedKeepsItsSlot(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	// The message lands and the runner claims the attempt before the
	// publisher hears back: the row is running when the error returns.
	p.publishRun = func(_ context.Context, m *queue.RunMessage) error {
		r := f.load(t, m.RunID)
		r.Status = store.RunStatusRunning
		if err := f.spy.SaveRun(f.ctx, r); err != nil {
			t.Fatalf("claim %s: %v", m.RunID, err)
		}
		return errors.New("ack timeout after the broker accepted the message")
	}
	if err := f.launch(t, p, "run-a", nil); err == nil {
		t.Fatal("a launch whose publish reported an error returned nil")
	}
	if r := f.load(t, "run-a"); r.Status != store.RunStatusRunning {
		t.Fatalf("run-a status=%s code=%s after the publish error, want running — the rollback must not end a claimed attempt", r.Status, r.FailureCode)
	}
	if n := f.ledgerHolders(t, cappedFP); n != 1 {
		t.Fatalf("the key names %d holders, want run-a's slot kept", n)
	}
	// The slot is still run-a's: the next launch on the key parks.
	p.publishRun = f.published.publish
	if err := f.launch(t, p, "run-b", nil); err != nil {
		t.Fatalf("launch run-b: %v", err)
	}
	f.assertParked(t, "run-b")
}

// A publish that fails while the row is still queued for this launch ends
// the row and gives the slot back at once: no runner will execute it — a
// delivery that lands later finds a failed row and is dropped.
func TestCredSlotAdmission_ALaunchWhosePublishFailedGivesItsSlotBack(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	p.publishRun = func(context.Context, *queue.RunMessage) error { return errors.New("nats: no responders") }
	if err := f.launch(t, p, "run-a", nil); err == nil {
		t.Fatal("a launch whose publish failed returned nil")
	}
	if r := f.load(t, "run-a"); r.Status != store.RunStatusFailed || r.FailureCode != store.FailureLaunchFailed {
		t.Fatalf("run-a: status=%s code=%s, want failed/%s", r.Status, r.FailureCode, store.FailureLaunchFailed)
	}
	if n := f.ledgerHolders(t, cappedFP); n != 0 {
		t.Fatalf("the key names %d holders after a launch that never left its launch path, want none", n)
	}
}

// unsavableRun refuses to persist one run: the launch's one SaveRun fails.
type unsavableRun struct {
	store.RunStore
	runID string
}

func (s *unsavableRun) Unwrap() store.RunStore { return s.RunStore }
func (s *unsavableRun) SaveRun(ctx context.Context, r *store.Run) error {
	if r.ID == s.runID {
		return errors.New("runs collection unavailable")
	}
	return s.RunStore.SaveRun(ctx, r)
}

// A launch whose row never persisted gives its slot back at once: no row
// means no message either (the publish follows the persist), and a holder
// with no run document keeps its slot for the reconcile's whole grace — the
// next launch on the key would park for minutes behind a run that never
// existed. Red when the release is dropped from that exit.
func TestCredSlotAdmission_ALaunchThatNeverPersistedGivesItsSlotBack(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	p.store = &unsavableRun{RunStore: f.spy, runID: "run-lost"}
	if err := f.launch(t, p, "run-lost", nil); err == nil || !strings.Contains(err.Error(), "runs collection unavailable") {
		t.Fatalf("launch err=%v, want the save's own error", err)
	}
	p.store = f.spy
	if err := f.launch(t, p, "run-next", nil); err != nil {
		t.Fatalf("launch run-next: %v", err)
	}
	f.assertQueued(t, "run-next")
}

// unstampableStore loses every credential re-stamp.
type unstampableStore struct{ store.RunStore }

func (s *unstampableStore) Unwrap() store.RunStore { return s.RunStore }
func (s *unstampableStore) SetRunCredStamp(context.Context, string, store.RunCredStamp) error {
	return errors.New("cred stamp write lost")
}

// A resume whose re-stamp fails is refused when it would reserve a capped
// key. The ledger's reconcile reads the run document's stamp: a run whose
// key rotated since its launch, resumed on the new key with the stamp write
// lost, holds a slot its document does not name — it reads as a run that
// stopped spending the key, and the next launch's refusal evicts it while
// it executes. The guarantee asserted is the cap's own: at most one run on
// the key is alive. Red when the failed stamp is only logged.
func TestCredSlotAdmission_AResumeWhoseReStampFailsHoldsNoSlot(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	if err := f.launch(t, p, "run-a", nil); err != nil {
		t.Fatalf("launch run-a: %v", err)
	}
	f.park(t, "run-a")
	// The key rotates: the record run-a launched on is replaced by a new
	// one, with its own fingerprint and the same cap.
	if err := f.keys.Delete(context.Background(), f.keyID); err != nil {
		t.Fatalf("delete the launch's key: %v", err)
	}
	const rotatedFP = "fp-capped-rotated"
	seedCappedKey(t, f.keys, f.sealer, cappedTeam, secrets.ProviderAnthropic, "sk-capped-rotated", rotatedFP, cappedKeyName, 1)
	p.store = &unstampableStore{RunStore: f.spy}
	resumeErr := f.resume(t, p, "run-a")
	p.store = f.spy
	if err := f.launch(t, p, "run-b", nil); err != nil {
		t.Fatalf("launch run-b: %v", err)
	}
	var alive []string
	for _, id := range []string{"run-a", "run-b"} {
		if f.load(t, id).Status.HoldsCredentialSlot() {
			alive = append(alive, id)
		}
	}
	if len(alive) > 1 {
		t.Fatalf("runs %v are alive on a key capped at one (resume err=%v): the resumed run held a slot its document did not name, and run-b's reconcile evicted it", alive, resumeErr)
	}
	if resumeErr == nil || !strings.Contains(resumeErr.Error(), "cred stamp write lost") {
		t.Fatalf("resume err=%v, want the lost stamp named", resumeErr)
	}
	if r := f.load(t, "run-a"); r.Status != store.RunStatusFailedResumable {
		t.Fatalf("run-a status=%s after a refused resume, want failed_resumable (rolled back)", r.Status)
	}
}

// cappedFPLater sorts after cappedFP: the admission reserves the stamp in
// order, so a launch holding both keys asks this one second.
const cappedFPLater = "fp-capped-z"

// seedLaterCappedKey adds an openai key on a cap of one beside the fixture's
// anthropic key, so every launch reserves both — the fixture's first.
func (f *cappedFixture) seedLaterCappedKey(t *testing.T) {
	t.Helper()
	seedCappedKey(t, f.keys, f.sealer, cappedTeam, secrets.ProviderOpenAI, "sk-openai-capped", cappedFPLater, "openai-capped", 1)
}

// A refusal on a later key gives back the slot the admission took on the
// earlier one: the parked run holds nothing, and the earlier key's slot is
// the next run's. Counted without the reconcile, which would hide a slot
// kept: the parked run's document no longer spends. Red when the partial
// refusal keeps what it took.
func TestCredSlotAdmission_ARefusalOnALaterKeyGivesBackTheEarlierSlot(t *testing.T) {
	f := newCappedFixture(t, 2)
	f.seedLaterCappedKey(t)
	p := f.publisher()
	if err := f.launch(t, p, "run-first", nil); err != nil {
		t.Fatalf("launch run-first: %v", err)
	}
	f.assertQueued(t, "run-first")
	if fps := f.load(t, "run-first").CredFingerprints; len(fps) != 2 || fps[0] != cappedFP || fps[1] != cappedFPLater {
		t.Fatalf("precondition: run-first's stamp is %v, want both capped keys in reservation order", fps)
	}
	if err := f.launch(t, p, "run-second", nil); err != nil {
		t.Fatalf("launch run-second: %v", err)
	}
	r := f.load(t, "run-second")
	if r.Status != store.RunStatusFailedResumable || r.FailureCode != store.FailureCredSlotWait || !strings.Contains(r.Error, "openai-capped") {
		t.Fatalf("run-second: status=%s code=%s err=%q, want parked on the openai key", r.Status, r.FailureCode, r.Error)
	}
	if n := f.ledgerHolders(t, cappedFP); n != 1 {
		t.Fatalf("the earlier key names %d holders after the refusal on the later one, want run-first alone — the parked run kept the slot it took", n)
	}
}

// failingKeyLedger answers the reservations of one fingerprint with a store
// error and serves every other from the ledger it wraps.
type failingKeyLedger struct {
	store.RunStore
	ledger store.CredSlotStore
	failFP string
}

func (s *failingKeyLedger) Unwrap() store.RunStore { return s.RunStore }
func (s *failingKeyLedger) ReserveCredSlot(ctx context.Context, fp, runID string, max int) (store.CredSlotAdmission, error) {
	if fp == s.failFP {
		return store.CredSlotAdmission{}, errors.New("ledger unavailable for " + fp)
	}
	return s.ledger.ReserveCredSlot(ctx, fp, runID, max)
}
func (s *failingKeyLedger) ReleaseCredSlot(ctx context.Context, fp, runID string) error {
	return s.ledger.ReleaseCredSlot(ctx, fp, runID)
}

// A store error on a later key gives back the slot taken on the earlier
// one, like a refusal: the launch is refused with nothing persisted, and a
// slot kept for it would stand, with no run document, for the reconcile's
// whole grace. Red when the error exit keeps what it took.
func TestCredSlotAdmission_AStoreErrorOnALaterKeyGivesBackTheEarlierSlot(t *testing.T) {
	f := newCappedFixture(t, 2)
	f.seedLaterCappedKey(t)
	p := f.publisher()
	p.store = &failingKeyLedger{RunStore: f.spy, ledger: store.AsCredSlotStore(f.spy), failFP: cappedFPLater}
	if err := f.launch(t, p, "run-blip", nil); err == nil || !strings.Contains(err.Error(), "ledger unavailable") {
		t.Fatalf("launch err=%v, want the ledger error", err)
	}
	if n := f.ledgerHolders(t, cappedFP); n != 0 {
		t.Fatalf("the earlier key names %d holders after a launch refused on a store error, want none", n)
	}
}

// A park on a store with no durable retry says so: nothing wakes the run,
// and a row promising that it "resumes by itself" is a wait nobody ends.
// Red when the words ignore the store the arming reads.
func TestCredSlotAdmission_AParkNothingWakesSaysSo(t *testing.T) {
	f := newCappedFixture(t, 1)
	p := f.publisher()
	if err := f.launch(t, p, "run-first", nil); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	// The filesystem store itself: the ledger and the queued flip, no
	// store.RunRetryStore.
	p.store = f.spy.RunStore
	sub := f.launchSub(t, p, "run-parked", nil)
	if sub.Status != store.RunStatusFailedResumable || sub.FailureCode != store.FailureCredSlotWait {
		t.Fatalf("submission = %+v, want a park", sub)
	}
	r := f.load(t, "run-parked")
	if strings.Contains(r.Error, "resumes by itself") || !strings.Contains(r.Error, "nothing resumes the run by itself") {
		t.Fatalf("the parked row says %q — on a store with no durable retry it must say nothing wakes it", r.Error)
	}
}
