package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/assistantmission"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A mission is the one caller left to ask again when a resume it dispatches
// is refused on a credential at its concurrency cap: the refusal lands before
// anything is published and clears once a run on the key ends. Each of its
// three dispatches — the activation and the result delivered to the
// assistant, the resume of the target — waits for a slot instead of being
// rejected, asks again one slot probe out, and spends none of the mission's
// action budget meanwhile.

// slotRefusingPublisher answers a resume of each run named in refuse with the
// admission's refusal — once per count — and records every resume it lets
// through. Launches are not under test.
type slotRefusingPublisher struct {
	mu      sync.Mutex
	refuse  map[string]int
	asked   map[string]int
	resumed []runview.ResumeSpec
}

func (p *slotRefusingPublisher) SubmitLaunch(context.Context, string, runview.LaunchSpec, *ir.Workflow, *runview.CompiledSource) (runview.LaunchSubmission, error) {
	return runview.LaunchSubmission{}, fmt.Errorf("slotRefusingPublisher: launches are not under test")
}

func (p *slotRefusingPublisher) CancelRun(context.Context, string) error { return nil }

func (p *slotRefusingPublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}

func (p *slotRefusingPublisher) SubmitResume(_ context.Context, spec runview.ResumeSpec, _ *ir.Workflow, _ *runview.CompiledSource) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked[spec.RunID]++
	if p.refuse[spec.RunID] > 0 {
		p.refuse[spec.RunID]--
		return fmt.Errorf("cloudpublisher: resume of run %s refused before publishing, the run put back as it was: %w", spec.RunID, cloudpublisher.ErrCredentialAtCapacity)
	}
	p.resumed = append(p.resumed, spec)
	return nil
}

func (p *slotRefusingPublisher) askedFor(runID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.asked[runID]
}

func (p *slotRefusingPublisher) resumedWith(receiptID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, spec := range p.resumed {
		if spec.ReceiptID == receiptID {
			return true
		}
	}
	return false
}

type missionSlotFixture struct {
	srv       *Server
	pub       *slotRefusingPublisher
	coord     *assistantMissionCoordinator
	target    string
	assistant string
	watch     string
}

const (
	missionSlotAssistantSource = `schema chat_out:
  message: string
  host_event: json

human chat:
  output: chat_out
  interaction: human_or_host

workflow chatbot:
  entry: chat
  chat -> done
`
	missionSlotTargetSource = `schema out:
  value: string

agent first:
  model: "test"
  output: out

workflow target:
  entry: first
  first -> done
`
)

// newMissionSlotFixture is a target run parked failed_resumable, an
// assistant run paused at its host-event chat gate, the watch that binds
// them, and a coordinator whose run service publishes through pub — the
// cloud resume path up to the publisher.
func newMissionSlotFixture(t *testing.T) *missionSlotFixture {
	t.Helper()
	srv, _ := newTestServer(t)
	ctx := context.Background()
	rs := srv.runs.RunStore()

	botsRoot := filepath.Join(srv.cfg.WorkDir, "bots")
	bundleDir := filepath.Join(botsRoot, "chatbot")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	assistantPath := filepath.Join(bundleDir, "main.bot")
	if err := os.WriteFile(assistantPath, []byte(missionSlotAssistantSource), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte("name: chatbot\nchat:\n  nodes:\n    chat:\n      kind: human\n      text_field: message\n      host_event_field: host_event\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.cfg.Bots.Paths = []string{botsRoot}
	targetPath := filepath.Join(srv.cfg.WorkDir, "target.bot")
	if err := os.WriteFile(targetPath, []byte(missionSlotTargetSource), 0o600); err != nil {
		t.Fatal(err)
	}

	seed := func(id, workflow, path, source string, status store.RunStatus, node, botID string) {
		t.Helper()
		_, hash, err := runview.CompileWorkflowWithHash(path, "")
		if err != nil {
			t.Fatalf("compile %s: %v", path, err)
		}
		if _, err := rs.CreateRun(ctx, id, workflow, nil); err != nil {
			t.Fatal(err)
		}
		r, err := rs.LoadRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		r.FilePath, r.WorkflowSource, r.WorkflowHash, r.Status, r.BotID = path, source, hash, status, botID
		r.Checkpoint = &store.Checkpoint{NodeID: node}
		if err := rs.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	f := &missionSlotFixture{srv: srv, target: "mission-slot-target", assistant: "mission-slot-assistant", watch: "mission-slot-watch"}
	seed(f.target, "target", targetPath, missionSlotTargetSource, store.RunStatusFailedResumable, "first", "")
	seed(f.assistant, "chatbot", assistantPath, missionSlotAssistantSource, store.RunStatusPausedWaitingHuman, "chat", "chatbot")

	now := time.Now().UTC()
	if err := srv.assistantWatches.CreateWatch(ctx, runwatch.Watch{ID: f.watch, OwnerID: "local", TargetRunID: f.target, AssistantRunID: f.assistant, Mode: runwatch.ModePropose, State: runwatch.WatchActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	f.pub = &slotRefusingPublisher{refuse: map[string]int{}, asked: map[string]int{}}
	cloudRuns := newTestRunviewService(t, srv.cfg.StoreDir,
		runview.WithLogger(iterlog.Nop()),
		runview.WithWorkDir(srv.cfg.WorkDir),
		runview.WithStore(rs),
		runview.WithLaunchPublisher(f.pub))
	f.coord = &assistantMissionCoordinator{server: srv, runs: cloudRuns, watches: srv.assistantWatches, missions: srv.assistantMissions, worker: "test-worker"}
	return f
}

// mission creates the mission under test, shaped by edit.
func (f *missionSlotFixture) mission(t *testing.T, id string, edit func(*assistantmission.Mission)) {
	t.Helper()
	now := time.Now().UTC()
	m := assistantmission.Mission{
		Version: 1, ID: id, InvocationKey: "goal:" + id, OperatorID: "local",
		TargetRunID: f.target, WatchID: f.watch, AssistantRunID: f.assistant,
		Policy: assistantmission.Policy{
			Actions: []string{assistantmission.ActionResume}, TTLSeconds: 600, ExpiresAt: now.Add(10 * time.Minute),
			MaxActions: 1, ContractVersion: assistantmission.ContractVersion,
		},
		State:            assistantmission.StateActive,
		Activation:       &assistantmission.DeliveryReceipt{ID: "activated", Kind: "assistant-mission-started", State: assistantmission.ReceiptSucceeded},
		ProposalFrontier: map[string]int{},
		CreatedAt:        now, UpdatedAt: now,
	}
	if edit != nil {
		edit(&m)
	}
	if _, _, err := f.srv.assistantMissions.CreateOrGet(context.Background(), m); err != nil {
		t.Fatal(err)
	}
}

func (f *missionSlotFixture) get(t *testing.T, id string) assistantmission.Mission {
	t.Helper()
	m, err := f.srv.assistantMissions.Get(context.Background(), assistantmission.Scope{OperatorID: "local", TargetRunID: f.target}, id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// endSlotWaits moves every slot wait of the mission into the past: the probe
// interval has elapsed.
func (f *missionSlotFixture) endSlotWaits(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	m, won, err := f.srv.assistantMissions.Claim(ctx, id, f.coord.worker, time.Now().UTC(), time.Second)
	if err != nil || !won {
		t.Fatalf("claim mission %s: won=%v err=%v", id, won, err)
	}
	past := time.Now().UTC().Add(-time.Second)
	elapse := func(at **time.Time) {
		if *at != nil {
			*at = &past
		}
	}
	if m.Activation != nil {
		elapse(&m.Activation.WaitUntil)
	}
	for i := range m.Receipts {
		elapse(&m.Receipts[i].WaitUntil)
		if m.Receipts[i].ResultDelivery != nil {
			elapse(&m.Receipts[i].ResultDelivery.WaitUntil)
		}
	}
	if _, err := f.srv.assistantMissions.UpdateClaimed(ctx, m, f.coord.worker); err != nil {
		t.Fatalf("end the slot waits of mission %s: %v", id, err)
	}
}

func assertSlotWait(t *testing.T, what string, m assistantmission.Mission, state assistantmission.ReceiptState, waitUntil *time.Time, errText string) {
	t.Helper()
	if state != assistantmission.ReceiptPrepared || waitUntil == nil || !waitUntil.After(time.Now().UTC()) {
		t.Fatalf("%s refused on a full key: state=%s wait_until=%v, want prepared with a wait ahead — a slot wait is not a rejection", what, state, waitUntil)
	}
	if !strings.Contains(errText, cloudpublisher.ErrCredentialAtCapacity.Error()) {
		t.Fatalf("%s: the receipt does not say why it waits: %q", what, errText)
	}
	if m.State != assistantmission.StateActive || !strings.Contains(m.Reason, "credential slot") {
		t.Fatalf("%s: mission state=%s reason=%q, want active and waiting for a credential slot", what, m.State, m.Reason)
	}
}

// The resume of the target, refused on its full key, waits for a slot: the
// action stays prepared, its budget unspent, nothing is asked inside the
// wait, and once the wait ends the resume is dispatched again. Red when the
// refusal rejects the action (the assistant is told "rejected" for a key
// that frees by itself, and the action budget is burnt).
func TestAssistantMission_AResumeRefusedOnAFullKeyWaitsForASlot(t *testing.T) {
	f := newMissionSlotFixture(t)
	ctx := context.Background()
	if err := f.srv.runs.RunStore().WriteArtifact(ctx, &store.Artifact{
		RunID: f.assistant, NodeID: "proposal", Version: 0,
		Data: map[string]any{"assistant_actions": []any{map[string]any{
			"id":   assistantmission.ActionResume,
			"args": map[string]any{"run_id": f.target},
		}}}, WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	const id = "mission-slot-resume"
	f.mission(t, id, nil)
	f.pub.refuse[f.target] = 1

	f.coord.attempt(ctx, id) // the proposal becomes a prepared receipt
	f.coord.attempt(ctx, id) // dispatched: refused on the target's full key
	m := f.get(t, id)
	if len(m.Receipts) != 1 {
		t.Fatalf("receipts = %+v, want the one resume", m.Receipts)
	}
	r := m.Receipts[0]
	assertSlotWait(t, "the target's resume", m, r.State, r.WaitUntil, r.Error)
	if m.DispatchedActions != 0 {
		t.Fatalf("a resume refused before publishing spent %d of the mission's action budget", m.DispatchedActions)
	}
	if n := f.pub.askedFor(f.target); n != 1 {
		t.Fatalf("the target's resume was asked %d times, want once", n)
	}

	f.coord.attempt(ctx, id) // inside the wait: nothing is asked
	if n := f.pub.askedFor(f.target); n != 1 {
		t.Fatalf("the target's resume was asked again inside its slot wait (%d asks)", n)
	}

	f.endSlotWaits(t, id)
	f.coord.attempt(ctx, id) // the wait ended: asked again, the slot is free
	m = f.get(t, id)
	r = m.Receipts[0]
	if r.State != assistantmission.ReceiptIssued || r.WaitUntil != nil || r.Error != "" || m.DispatchedActions != 1 {
		t.Fatalf("after the wait: receipt=%+v dispatched=%d, want issued once, its wait and refusal cleared", r, m.DispatchedActions)
	}
	if !f.pub.resumedWith(r.ID) {
		t.Fatalf("the target's resume was not published with receipt %s", r.ID)
	}
}

// The activation, refused on the assistant's own full key, waits for a slot
// instead of blocking the mission for good. Red when the refusal rejects the
// activation (capability_blocked: nothing ever activates the mission).
func TestAssistantMission_AnActivationRefusedOnAFullKeyWaitsForASlot(t *testing.T) {
	f := newMissionSlotFixture(t)
	ctx := context.Background()
	const id = "mission-slot-activation"
	f.mission(t, id, func(m *assistantmission.Mission) {
		m.Activation = &assistantmission.DeliveryReceipt{ID: deterministicMissionID(id, "activation"), Kind: "assistant-mission-started", State: assistantmission.ReceiptPrepared}
	})
	f.pub.refuse[f.assistant] = 1

	f.coord.attempt(ctx, id) // dispatched: refused on the assistant's full key
	m := f.get(t, id)
	assertSlotWait(t, "the activation", m, m.Activation.State, m.Activation.WaitUntil, m.Activation.Error)
	if n := f.pub.askedFor(f.assistant); n != 1 {
		t.Fatalf("the assistant's resume was asked %d times, want once", n)
	}

	f.coord.attempt(ctx, id) // inside the wait: nothing is asked
	if n := f.pub.askedFor(f.assistant); n != 1 {
		t.Fatalf("the activation was asked again inside its slot wait (%d asks)", n)
	}

	f.endSlotWaits(t, id)
	f.coord.attempt(ctx, id)
	m = f.get(t, id)
	if m.Activation.State != assistantmission.ReceiptIssued || m.Activation.WaitUntil != nil || m.Activation.Error != "" {
		t.Fatalf("after the wait: activation=%+v, want issued, its wait and refusal cleared", m.Activation)
	}
	if !f.pub.resumedWith(m.Activation.ID) {
		t.Fatalf("the activation was not delivered with receipt %s", m.Activation.ID)
	}
}

// The result of an action, refused on the assistant's own full key, waits
// for a slot instead of blocking the mission for good. Red when the refusal
// rejects the delivery (capability_blocked: the assistant never learns what
// its action did).
func TestAssistantMission_AResultDeliveryRefusedOnAFullKeyWaitsForASlot(t *testing.T) {
	f := newMissionSlotFixture(t)
	ctx := context.Background()
	const id = "mission-slot-result"
	now := time.Now().UTC()
	f.mission(t, id, func(m *assistantmission.Mission) {
		m.DispatchedActions = 1
		m.Receipts = []assistantmission.ActionReceipt{{
			ID: "mission-slot-action", Action: assistantmission.ActionResume, AssistantRunID: f.assistant,
			Args: map[string]any{"run_id": f.target}, State: assistantmission.ReceiptSucceeded, CreatedAt: now, UpdatedAt: now,
			ResultDelivery: &assistantmission.DeliveryReceipt{ID: "mission-slot-result-delivery", Kind: "assistant-mission-action-result", State: assistantmission.ReceiptPrepared},
		}}
	})
	f.pub.refuse[f.assistant] = 1

	f.coord.attempt(ctx, id) // dispatched: refused on the assistant's full key
	m := f.get(t, id)
	rd := m.Receipts[0].ResultDelivery
	assertSlotWait(t, "the result delivery", m, rd.State, rd.WaitUntil, rd.Error)

	f.coord.attempt(ctx, id) // inside the wait: nothing is asked
	if n := f.pub.askedFor(f.assistant); n != 1 {
		t.Fatalf("the result delivery was asked again inside its slot wait (%d asks)", n)
	}

	f.endSlotWaits(t, id)
	f.coord.attempt(ctx, id)
	m = f.get(t, id)
	rd = m.Receipts[0].ResultDelivery
	if rd.State != assistantmission.ReceiptIssued || rd.WaitUntil != nil || rd.Error != "" {
		t.Fatalf("after the wait: result delivery=%+v, want issued, its wait and refusal cleared", rd)
	}
	if !f.pub.resumedWith(rd.ID) {
		t.Fatalf("the result was not delivered with receipt %s", rd.ID)
	}
}
