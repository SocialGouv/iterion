// ADR-167 T2, the stamp: every run that seals a gateway key carries the
// gateway's capacity subject `gw:<host>` beside its fingerprints, at launch and
// at every resume, read from the bundle's two channels and never from the
// content boundary; T2 reserves none of it; and a resume whose re-stamp is
// lost is refused as soon as its old or its new stamp names a subject.

package cloudpublisher

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// stampFixture is a publisher over the slot-ledger store, a memory key store
// and a capturing logger, launching and resuming as team1.
type stampFixture struct {
	spy    *ledgerRetrySpy
	keys   *secrets.MemoryApiKeyStore
	sealer secrets.Sealer
	log    *bytes.Buffer
	p      *Publisher
	ctx    context.Context
}

// newStampFixture maps team1 to runnerPool when it is not empty.
func newStampFixture(t *testing.T, runnerPool string) *stampFixture {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	f := &stampFixture{spy: newLedgerRetrySpy(t), keys: secrets.NewMemoryApiKeyStore(), sealer: sealer, log: &bytes.Buffer{},
		ctx: store.WithIdentity(context.Background(), "team1", "owner1")}
	f.p = &Publisher{
		apiKeys: f.keys, usageCaps: usagecap.NewMemStore(), store: f.spy,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger:     iterlog.New(iterlog.LevelInfo, f.log),
		publishRun: func(context.Context, *queue.RunMessage) error { return nil },
	}
	if runnerPool != "" {
		f.p.identity = &fakeTeamResolver{orgs: map[string]string{"team1": ""}, runnerPool: map[string]string{"team1": runnerPool}}
		f.p.pools = activeRegistry(runnerPool)
	}
	return f
}

const stampSource = "workflow wf:\n  start -> done\n"

func (f *stampFixture) launch(t *testing.T, runID string, wf *ir.Workflow) {
	t.Helper()
	if _, err := f.p.SubmitLaunch(f.ctx, runID, runview.LaunchSpec{FilePath: "wf.bot", Source: stampSource, BotID: gwBot}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch %s: %v (log:\n%s)", runID, err, f.log.String())
	}
}

func (f *stampFixture) resume(runID string, wf *ir.Workflow) error {
	return f.p.SubmitResume(f.ctx, runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: stampSource}, wf, &runview.CompiledSource{Hash: "hash"})
}

func (f *stampFixture) load(t *testing.T, runID string) *store.Run {
	t.Helper()
	r, err := f.spy.LoadRun(f.ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", runID, err)
	}
	return r
}

func (f *stampFixture) park(t *testing.T, runID string) {
	t.Helper()
	r := f.load(t, runID)
	r.Status = store.RunStatusFailedResumable
	if err := f.spy.SaveRun(f.ctx, r); err != nil {
		t.Fatalf("park %s: %v", runID, err)
	}
}

// deleteKeys removes every key of the fixture's store.
func (f *stampFixture) deleteKeys(t *testing.T) {
	t.Helper()
	keys, err := f.keys.ListByTeam(context.Background(), "team1", "")
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	for _, k := range keys {
		if err := f.keys.Delete(context.Background(), k.ID); err != nil {
			t.Fatalf("delete key %s: %v", k.ID, err)
		}
	}
}

// poolDialWorkflow is a run whose one route is an env-templated dial: the
// walk cannot decide it, so the team's gateway key seals for it only under a
// runner pool, under an own_keys boundary naming the key.
func poolDialWorkflow() *ir.Workflow {
	return &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "${C3_UNSET_ROUTE_DIAL:-anthropic/claude-sonnet-4-5}"}, Tools: []string{}},
	}}
}

func gwSubject() string { return GatewayCapacitySubjectOf(gwURL) }

// A launch that seals the team's gateway key stamps the gateway's subject
// beside the key's fingerprint, in their own fields.
func TestGatewayCapacityStamp_ALaunchStampsTheGatewaySubject(t *testing.T) {
	f := newStampFixture(t, "")
	seedGatewayKey(t, f.keys, f.sealer, "team1", nil)
	f.launch(t, "run-stamp-1", gatewayOnlyWorkflow())
	r := f.load(t, "run-stamp-1")
	if !slices.Equal(r.CapacitySubjects, []string{gwSubject()}) {
		t.Fatalf("CapacitySubjects = %v, want [%s] — the run seals the team's gateway key", r.CapacitySubjects, gwSubject())
	}
	if !slices.Equal(r.CredFingerprints, []string{gwFingerprint}) {
		t.Fatalf("CredFingerprints = %v, want [%s] alone — the subject rides beside the fingerprints, never among them", r.CredFingerprints, gwFingerprint)
	}
	if !r.CapacitySubjectsRead() {
		t.Fatalf("CapacitySubjectsRead() = false (marker %q) — the stamp read its subjects off the sealed bundle and must say so", r.CapacityStampedFor)
	}
}

// capacitySubjectsOf reads both channels, one subject per host, gateway keys
// only, and never stamps an empty subject.
//
// The pinned channel is witnessed here and not through a launch: no fill
// seals an openai_compatible key pinned today. Its wire family is its own
// (secrets.WireFamily), so the family is taken only by an openai_compatible
// key in the default channel, and every shared tier skips a provider either
// channel already holds — a call site that read the default channel alone
// stamps the same subjects on every bundle a launch can build.
func TestCapacitySubjectsOf(t *testing.T) {
	gw := secrets.ProviderOpenAICompatible
	a, b := "https://a.example", "https://b.example"
	for _, row := range []struct {
		name   string
		bundle secrets.RunBundle
		want   []string
	}{
		{"default channel only", secrets.RunBundle{APIKeys: map[secrets.Provider]string{gw: "k"}, APIBaseURLs: map[secrets.Provider]string{gw: a}},
			[]string{GatewayCapacitySubjectOf(a)}},
		{"pinned channel only", secrets.RunBundle{PinnedAPIKeys: map[secrets.Provider]string{gw: "k"}, PinnedAPIBaseURLs: map[secrets.Provider]string{gw: b}},
			[]string{GatewayCapacitySubjectOf(b)}},
		{"both channels, two hosts", secrets.RunBundle{
			APIKeys: map[secrets.Provider]string{gw: "k1"}, APIBaseURLs: map[secrets.Provider]string{gw: b},
			PinnedAPIKeys: map[secrets.Provider]string{gw: "k2"}, PinnedAPIBaseURLs: map[secrets.Provider]string{gw: a},
		}, []string{GatewayCapacitySubjectOf(a), GatewayCapacitySubjectOf(b)}},
		{"both channels, one host", secrets.RunBundle{
			APIKeys: map[secrets.Provider]string{gw: "k1"}, APIBaseURLs: map[secrets.Provider]string{gw: a},
			PinnedAPIKeys: map[secrets.Provider]string{gw: "k2"}, PinnedAPIBaseURLs: map[secrets.Provider]string{gw: a},
		}, []string{GatewayCapacitySubjectOf(a)}},
		{"vendor keys only", secrets.RunBundle{
			APIKeys:     map[secrets.Provider]string{secrets.ProviderAnthropic: "k"},
			APIBaseURLs: map[secrets.Provider]string{secrets.ProviderAnthropic: a},
		}, nil},
		{"a gateway key whose URL names no host", secrets.RunBundle{
			APIKeys: map[secrets.Provider]string{gw: "k1"}, APIBaseURLs: map[secrets.Provider]string{gw: "not a url"},
			PinnedAPIKeys: map[secrets.Provider]string{gw: "k2"}, PinnedAPIBaseURLs: map[secrets.Provider]string{gw: a},
		}, []string{GatewayCapacitySubjectOf(a)}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := capacitySubjectsOf(row.bundle); !slices.Equal(got, row.want) {
				t.Fatalf("capacitySubjectsOf = %v, want %v", got, row.want)
			}
		})
	}
}

// A gateway key whose URL names no host never reaches a run: the resolution
// that would stamp it refuses at the seal, so no run carries a gateway key
// counted on no gateway. This is the guarantee capacitySubjectsOf's silence
// on such a key rests on.
func TestGatewayCapacityStamp_AGatewayKeyNamingNoHostIsNeverPublished(t *testing.T) {
	const hostless = "https:///v1"
	if GatewayCapacitySubjectOf(hostless) != "" {
		t.Fatalf("premise: %q must name no host", hostless)
	}
	f := newStampFixture(t, "")
	seedGatewayKey(t, f.keys, f.sealer, "team1", func(k *secrets.ApiKey) {
		k.BaseURL = hostless
		k.Fingerprint = secrets.APIKeyFingerprint(secrets.ProviderOpenAICompatible, gwTeamKey, hostless)
	})
	_, err := f.p.SubmitLaunch(f.ctx, "run-hostless", runview.LaunchSpec{FilePath: "wf.bot", Source: stampSource, BotID: gwBot}, gatewayOnlyWorkflow(), &runview.CompiledSource{Hash: "hash"})
	if !errors.Is(err, secrets.ErrGatewayKeyWithoutURL) {
		t.Fatalf("launch over a gateway key naming no host = %v, want the seal's refusal (ErrGatewayKeyWithoutURL) (log:\n%s)", err, f.log.String())
	}
}

// The subject folds every spelling of one host onto the name a dial resolves,
// as GatewayCapacitySubject does for the ledger. A launch never stamps a
// non-canonical spelling (SealRunBundle refuses a gateway URL that is not the
// canonical form), so the fold is witnessed here, on the function the stamp
// and the status route share.
func TestGatewayCapacitySubjectOf_FoldsTheHost(t *testing.T) {
	for raw, want := range map[string]string{
		"https://GW.Example.com./":        "gw:gw.example.com",
		"https://gw.example.com:8443/v1":  "gw:gw.example.com",
		"https://bücher.example":          "gw:xn--bcher-kva.example",
		"https://[::ffff:10.0.0.7]:4000/": "gw:10.0.0.7",
		"not a url":                       "",
		"":                                "",
	} {
		if got := GatewayCapacitySubjectOf(raw); got != want {
			t.Errorf("GatewayCapacitySubjectOf(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A resume after the team's gateway key moved to another host re-stamps the
// new host, read from the re-sealed bundle, while the content boundary keeps
// the host its launch froze.
func TestGatewayCapacityStamp_AResumeAfterTheGatewayMovedReStampsTheNewHost(t *testing.T) {
	const movedURL = "https://gw2.example"
	f := newStampFixture(t, "")
	seedGatewayKey(t, f.keys, f.sealer, "team1", nil)
	f.launch(t, "run-moved", gatewayOnlyWorkflow())
	f.park(t, "run-moved")
	f.deleteKeys(t)
	seedGatewayKey(t, f.keys, f.sealer, "team1", func(k *secrets.ApiKey) {
		k.BaseURL = movedURL
		k.Fingerprint = secrets.APIKeyFingerprint(secrets.ProviderOpenAICompatible, gwTeamKey, movedURL)
	})
	if err := f.resume("run-moved", gatewayOnlyWorkflow()); err != nil {
		t.Fatalf("resume: %v (log:\n%s)", err, f.log.String())
	}
	r := f.load(t, "run-moved")
	if want := GatewayCapacitySubjectOf(movedURL); !slices.Equal(r.CapacitySubjects, []string{want}) {
		t.Fatalf("CapacitySubjects = %v, want [%s] — the host the re-sealed key dials", r.CapacitySubjects, want)
	}
	if r.ContentBoundary == nil || r.ContentBoundary.GatewayHost != "gw.example" {
		t.Fatalf("content boundary = %+v, want the launch's frozen gateway_host gw.example — the fixture must show the boundary and the stamp disagree", r.ContentBoundary)
	}
}

// A resume that seals no gateway key any more clears the subjects: a run off
// the gateway holds no place on it.
func TestGatewayCapacityStamp_AResumeAfterTheGatewayKeyIsDeletedClearsTheSubjects(t *testing.T) {
	f := newStampFixture(t, "pool-a")
	seedGatewayKey(t, f.keys, f.sealer, "team1", nil)
	f.launch(t, "run-cleared", poolDialWorkflow())
	if got := f.load(t, "run-cleared").CapacitySubjects; !slices.Equal(got, []string{gwSubject()}) {
		t.Fatalf("fixture: launch stamped %v, want [%s] (log:\n%s)", got, gwSubject(), f.log.String())
	}
	f.park(t, "run-cleared")
	f.deleteKeys(t)
	if err := f.resume("run-cleared", poolDialWorkflow()); err != nil {
		t.Fatalf("resume: %v (log:\n%s)", err, f.log.String())
	}
	r := f.load(t, "run-cleared")
	if len(r.CapacitySubjects) != 0 {
		t.Fatalf("CapacitySubjects = %v after a resume that sealed no gateway key, want none", r.CapacitySubjects)
	}
	// The content boundary keeps the key id its launch froze; the stamp says
	// its empty subjects were read, so no reading takes the run for one whose
	// gateway is unknown.
	if !r.CapacitySubjectsRead() || r.ContentBoundary == nil || r.ContentBoundary.KeyID == "" {
		t.Fatalf("CapacitySubjectsRead()=%v boundary=%+v, want the stamp marked as read beside the launch's frozen key id", r.CapacitySubjectsRead(), r.ContentBoundary)
	}
}

// A default-runner run whose dials are env-templated seals no gateway key
// (W7b): it rides the runner's environment gateway and carries no subject,
// whatever its team holds and whatever its boundary says.
func TestGatewayCapacityStamp_ADefaultRunnerEnvDialRunCarriesNoSubject(t *testing.T) {
	f := newStampFixture(t, "")
	seedGatewayKey(t, f.keys, f.sealer, "team1", nil)
	f.launch(t, "run-envdial", poolDialWorkflow())
	r := f.load(t, "run-envdial")
	if len(r.CapacitySubjects) != 0 || slices.Contains(r.CredFingerprints, gwFingerprint) {
		t.Fatalf("CapacitySubjects = %v, fingerprints %v — a default-runner env dial seals no gateway key and carries no subject", r.CapacitySubjects, r.CredFingerprints)
	}
}

// reserveSpy records every slot the admission asks the ledger for.
type reserveSpy struct {
	store.RunStore
	ledger store.CredSlotStore
	mu     sync.Mutex
	asked  []string
}

func (s *reserveSpy) Unwrap() store.RunStore { return s.RunStore }
func (s *reserveSpy) ReserveCredSlot(ctx context.Context, fp, runID string, max int) (store.CredSlotAdmission, error) {
	s.mu.Lock()
	s.asked = append(s.asked, fp)
	s.mu.Unlock()
	return s.ledger.ReserveCredSlot(ctx, fp, runID, max)
}
func (s *reserveSpy) ReassertCredSlot(ctx context.Context, fp, runID string, token time.Time) (store.CredSlotReassertion, error) {
	return s.ledger.ReassertCredSlot(ctx, fp, runID, token)
}
func (s *reserveSpy) ReleaseCredSlot(ctx context.Context, fp, runID string, at time.Time) error {
	return s.ledger.ReleaseCredSlot(ctx, fp, runID, at)
}
func (s *reserveSpy) SettleCredSlotAdmission(ctx context.Context, runID string, at time.Time) error {
	return s.ledger.SettleCredSlotAdmission(ctx, runID, at)
}

// T2 observes: no gateway subject is ever reserved, on a launch or a resume —
// the capped key beside it is, so the admission demonstrably ran.
func TestGatewayCapacityStamp_T2ReservesNoGatewaySlot(t *testing.T) {
	f := newStampFixture(t, "")
	seedGatewayKey(t, f.keys, f.sealer, "team1", func(k *secrets.ApiKey) { k.MaxConcurrentRuns = 5 })
	spy := &reserveSpy{RunStore: f.spy, ledger: store.AsCredSlotStore(f.spy)}
	f.p.store = spy
	f.launch(t, "run-observe", gatewayOnlyWorkflow())
	f.park(t, "run-observe")
	if err := f.resume("run-observe", gatewayOnlyWorkflow()); err != nil {
		t.Fatalf("resume: %v (log:\n%s)", err, f.log.String())
	}
	if got := f.load(t, "run-observe").CapacitySubjects; !slices.Equal(got, []string{gwSubject()}) {
		t.Fatalf("fixture: CapacitySubjects = %v, want [%s]", got, gwSubject())
	}
	if !slices.Equal(spy.asked, []string{gwFingerprint, gwFingerprint}) {
		t.Fatalf("ledger asked for %v, want the capped key's fingerprint once per admission and never %s — T2 reserves no gateway slot", spy.asked, gwSubject())
	}
}

// Fail closed (a): a run stamped with a subject whose resume seals no gateway
// key any more, its re-stamp lost — refused, put back as it was, and not a
// capacity wait.
func TestGatewayCapacityStamp_AResumeLosingAStampedSubjectIsRefused(t *testing.T) {
	f := newStampFixture(t, "pool-a")
	seedGatewayKey(t, f.keys, f.sealer, "team1", nil)
	f.launch(t, "run-fc-a", poolDialWorkflow())
	if got := f.load(t, "run-fc-a").CapacitySubjects; !slices.Equal(got, []string{gwSubject()}) {
		t.Fatalf("fixture: launch stamped %v, want [%s]", got, gwSubject())
	}
	f.park(t, "run-fc-a")
	f.deleteKeys(t)
	f.p.store = &unstampableStore{RunStore: f.spy}
	err := f.resume("run-fc-a", poolDialWorkflow())
	f.p.store = f.spy
	if err == nil || !strings.Contains(err.Error(), "capacity subjects") || !strings.Contains(err.Error(), "cred stamp write lost") {
		t.Fatalf("resume = %v, want the refusal naming the capacity subjects and the lost write (log:\n%s)", err, f.log.String())
	}
	if errors.Is(err, ErrCredentialAtCapacity) {
		t.Fatalf("resume = %v reads as a capacity wait: a lost write is no full gateway", err)
	}
	if r := f.load(t, "run-fc-a"); r.Status != store.RunStatusFailedResumable || !slices.Equal(r.CapacitySubjects, []string{gwSubject()}) {
		t.Fatalf("run-fc-a: status=%s subjects=%v, want failed_resumable as before the resume", r.Status, r.CapacitySubjects)
	}
}

// Fail closed (b): a run stamped before T2 (no subject) whose resume seals a
// gateway key, its re-stamp lost — refused.
func TestGatewayCapacityStamp_AResumeGainingASubjectIsRefused(t *testing.T) {
	f := newStampFixture(t, "")
	seedGatewayKey(t, f.keys, f.sealer, "team1", nil)
	f.launch(t, "run-fc-b", gatewayOnlyWorkflow())
	f.park(t, "run-fc-b")
	if err := f.spy.SetRunCredStamp(f.ctx, "run-fc-b", store.RunCredStamp{Fingerprints: []string{gwFingerprint}}); err != nil {
		t.Fatalf("rewrite the stamp as a pre-T2 replica wrote it: %v", err)
	}
	f.p.store = &unstampableStore{RunStore: f.spy}
	err := f.resume("run-fc-b", gatewayOnlyWorkflow())
	f.p.store = f.spy
	if err == nil || !strings.Contains(err.Error(), "capacity subjects") {
		t.Fatalf("resume = %v, want the refusal naming the capacity subjects (log:\n%s)", err, f.log.String())
	}
	if r := f.load(t, "run-fc-b"); r.Status != store.RunStatusFailedResumable {
		t.Fatalf("run-fc-b: status=%s, want failed_resumable as before the resume", r.Status)
	}
}

// staleReadStore answers its first LoadRun — the resume's read before its
// claim — without the run's capacity subjects, as a document read before a
// concurrent resume stamped [gw:X] and was reverted (a revert puts the status
// back, never the stamp). Every later read is the store's. With rereadFails,
// a read after a lost stamp write fails.
type staleReadStore struct {
	store.RunStore
	reads       int
	stampLost   bool
	rereadFails bool
}

func (s *staleReadStore) Unwrap() store.RunStore { return s.RunStore }
func (s *staleReadStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	s.reads++
	if s.stampLost && s.rereadFails {
		return nil, errors.New("run document unreadable")
	}
	r, err := s.RunStore.LoadRun(ctx, id)
	if err != nil || s.reads > 1 {
		return r, err
	}
	stale := *r
	stale.CapacitySubjects = nil
	return &stale, nil
}
func (s *staleReadStore) SetRunCredStamp(context.Context, string, store.RunCredStamp) error {
	s.stampLost = true
	return errors.New("cred stamp write lost")
}

// Fail closed, the old stamp read after the claim: the document holds
// [gw:H] while the read before the claim saw none, the resume seals no
// gateway key, and its re-stamp is lost — refused, since the write would
// have cleared a subject the document keeps.
func TestGatewayCapacityStamp_ALostReStampJudgesTheDocumentNotThePreClaimRead(t *testing.T) {
	f := newStampFixture(t, "pool-a")
	seedGatewayKey(t, f.keys, f.sealer, "team1", nil)
	f.launch(t, "run-fc-stale", poolDialWorkflow())
	if got := f.load(t, "run-fc-stale").CapacitySubjects; !slices.Equal(got, []string{gwSubject()}) {
		t.Fatalf("fixture: launch stamped %v, want [%s]", got, gwSubject())
	}
	f.park(t, "run-fc-stale")
	f.deleteKeys(t)
	stale := &staleReadStore{RunStore: f.spy}
	f.p.store = stale
	err := f.resume("run-fc-stale", poolDialWorkflow())
	f.p.store = f.spy
	if !stale.stampLost {
		t.Fatalf("fixture: the resume never reached its re-stamp (err=%v, log:\n%s)", err, f.log.String())
	}
	if err == nil || !strings.Contains(err.Error(), "capacity subjects") || !strings.Contains(err.Error(), gwSubject()) {
		t.Fatalf("resume = %v, want the refusal naming the subject the document still holds (log:\n%s)", err, f.log.String())
	}
	if errors.Is(err, ErrCredentialAtCapacity) {
		t.Fatalf("resume = %v reads as a capacity wait: a lost write is no full gateway", err)
	}
	if r := f.load(t, "run-fc-stale"); r.Status != store.RunStatusFailedResumable {
		t.Fatalf("run-fc-stale: status=%s, want failed_resumable as before the resume", r.Status)
	}
}

// Fail closed when the document cannot be read again after a lost re-stamp:
// nothing then proves it names no subject.
func TestGatewayCapacityStamp_ALostReStampOverAnUnreadableDocumentIsRefused(t *testing.T) {
	f := newStampFixture(t, "")
	seedKeyFP(t, f.keys, f.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")
	f.launch(t, "run-fc-unread", cappedWorkflow())
	f.park(t, "run-fc-unread")
	stale := &staleReadStore{RunStore: f.spy, rereadFails: true}
	f.p.store = stale
	err := f.resume("run-fc-unread", cappedWorkflow())
	f.p.store = f.spy
	if err == nil || !strings.Contains(err.Error(), "capacity subjects") || !strings.Contains(err.Error(), "run document unreadable") {
		t.Fatalf("resume = %v, want the refusal naming the lost write and the failed read (log:\n%s)", err, f.log.String())
	}
	if r := f.load(t, "run-fc-unread"); r.Status != store.RunStatusFailedResumable {
		t.Fatalf("run-fc-unread: status=%s, want failed_resumable as before the resume", r.Status)
	}
}

// Fail closed (c): with no subject on either side and no capped key, a lost
// re-stamp stays best-effort — the resume proceeds and says so.
func TestGatewayCapacityStamp_AVendorOnlyResumeLosingItsStampProceeds(t *testing.T) {
	f := newStampFixture(t, "")
	seedKeyFP(t, f.keys, f.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")
	f.launch(t, "run-fc-c", cappedWorkflow())
	f.park(t, "run-fc-c")
	f.p.store = &unstampableStore{RunStore: f.spy}
	err := f.resume("run-fc-c", cappedWorkflow())
	f.p.store = f.spy
	if err != nil {
		t.Fatalf("resume = %v, want it to proceed — nothing capped and no subject on either side (log:\n%s)", err, f.log.String())
	}
	if !strings.Contains(f.log.String(), "re-stamp cred fingerprints on run run-fc-c") {
		t.Fatalf("the lost re-stamp was not logged:\n%s", f.log.String())
	}
}
