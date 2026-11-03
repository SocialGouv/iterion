package cloudpublisher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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
