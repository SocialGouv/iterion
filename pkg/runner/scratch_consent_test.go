package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/scripthost"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestSpendScratchConsent_isSpentByTheClaimOfItsPublication: a resume
// message's consent to the scratch's loss is applied only while the run is
// still queued for that message. Redelivered after its claim — the run
// running or parked again — adopted, or behind a later attempt's queued
// flip, the message no longer carries it; so does a message whose
// publication time cannot be read.
func TestSpendScratchConsent_isSpentByTheClaimOfItsPublication(t *testing.T) {
	published := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		status    store.RunStatus
		queuedAt  time.Time
		published string
		kept      bool
	}{
		{"queued for this message", store.RunStatusQueued, published.Add(-time.Second), published.Format(time.RFC3339Nano), true},
		{"claimed, then parked again", store.RunStatusFailedResumable, published.Add(-time.Second), published.Format(time.RFC3339Nano), false},
		{"claimed, left running", store.RunStatusRunning, published.Add(-time.Second), published.Format(time.RFC3339Nano), false},
		{"queued for a later attempt", store.RunStatusQueued, published.Add(time.Minute), published.Format(time.RFC3339Nano), false},
		{"a publication time that cannot be read", store.RunStatusQueued, published.Add(-time.Second), "yesterday", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := store.WithIdentity(context.Background(), "team-1", "u1")
			queuedAt := tc.queuedAt
			if err := st.SaveRun(ctx, &store.Run{ID: "run-consent", TenantID: "team-1", OwnerID: "u1", Status: tc.status, QueuedAt: &queuedAt}); err != nil {
				t.Fatal(err)
			}
			r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
			msg := &queue.RunMessage{RunID: "run-consent", TenantID: "team-1", OwnerID: "u1", PublishedAtRFC: tc.published,
				Resume: &queue.ResumeSpec{Force: true, AcceptScratchLoss: true}}
			if err := r.spendResumeIntent(context.Background(), msg, iterlog.Nop()); err != nil {
				t.Fatal(err)
			}
			if msg.Resume.AcceptScratchLoss != tc.kept || !msg.Resume.Force {
				t.Fatalf("after the claim check: consent %v (want %v), force %v (want it untouched)", msg.Resume.AcceptScratchLoss, tc.kept, msg.Resume.Force)
			}
		})
	}
}

// TestExecuteRun_givesTheEngineTheScratchsConsent: the consent a resume
// message carries reaches the engine that executes it — a run whose last
// teardown could not bank its scratch resumes with it, and is refused with
// force alone.
func TestExecuteRun_givesTheEngineTheScratchsConsent(t *testing.T) {
	for _, accept := range []bool{false, true} {
		t.Run(fmt.Sprintf("accept=%v", accept), func(t *testing.T) {
			t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
			ctx := store.WithIdentity(context.Background(), "team-1", "u1")
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const runID = "run-consent-wire"
			if _, err := st.CreateRun(ctx, runID, "main", nil); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveCheckpoint(ctx, runID, &store.Checkpoint{NodeID: "done"}); err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateRunStatus(ctx, runID, store.RunStatusFailedResumable, "boom"); err != nil {
				t.Fatal(err)
			}
			if _, err := st.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{
				"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap",
			}}); err != nil {
				t.Fatal(err)
			}
			pr := parsertest.Parse("main.bot", "workflow main:\n  entry: done\n")
			body, err := ast.MarshalFile(pr.File)
			if err != nil {
				t.Fatal(err)
			}
			r := &Runner{cfg: Config{Store: st, WorkDir: t.TempDir(), Logger: iterlog.Nop()}}
			msg := &queue.RunMessage{RunID: runID, TenantID: "team-1", OwnerID: "u1", WorkflowName: "main", IRCompiled: body,
				Resume: &queue.ResumeSpec{Force: true, AcceptScratchLoss: accept}}
			execErr := r.executeRun(ctx, msg, nil, nil)
			var rt *runtime.RuntimeError
			refused := errors.As(execErr, &rt) && rt.Code == runtime.ErrCodeScratchNotPortable
			if refused == accept {
				t.Fatalf("executeRun with consent %v: %v, want refused = %v", accept, execErr, !accept)
			}
		})
	}
}

// TestExecuteRun_aDeliveryClaimsItsOwnAttemptOnly: a resume delivery claims
// the queued run for the attempt it was published for. While its pod
// prepares, the run may be cancelled and resumed again without the consent:
// the delivery does not claim that newer attempt — nor lend it its consent —
// and the newer resume's own delivery meets the refusal it was published
// under. Undisturbed, the delivery claims its attempt and its consent holds.
func TestExecuteRun_aDeliveryClaimsItsOwnAttemptOnly(t *testing.T) {
	for _, requeued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued again=%v", requeued), func(t *testing.T) {
			t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
			ctx := store.WithIdentity(context.Background(), "team-1", "u1")
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const runID = "run-consent-attempt"
			if _, err := st.CreateRun(ctx, runID, "main", nil); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveCheckpoint(ctx, runID, &store.Checkpoint{NodeID: "done"}); err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateRunStatus(ctx, runID, store.RunStatusFailedResumable, "boom"); err != nil {
				t.Fatal(err)
			}
			if _, err := st.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{
				"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap",
			}}); err != nil {
				t.Fatal(err)
			}
			pr := parsertest.Parse("main.bot", "workflow main:\n  entry: done\n")
			body, err := ast.MarshalFile(pr.File)
			if err != nil {
				t.Fatal(err)
			}
			publish := func(from store.RunStatus, accept bool) *queue.RunMessage {
				t.Helper()
				if ok, err := st.UpdateRunStatusIf(ctx, runID, store.RunStatusQueued, "", []store.RunStatus{from}); err != nil || !ok {
					t.Fatalf("queued flip from %s: %v %v", from, ok, err)
				}
				time.Sleep(2 * time.Millisecond)
				return &queue.RunMessage{RunID: runID, TenantID: "team-1", OwnerID: "u1", WorkflowName: "main", IRCompiled: body,
					PublishedAtRFC: time.Now().UTC().Format(time.RFC3339Nano),
					Resume:         &queue.ResumeSpec{AcceptScratchLoss: accept, PriorStatus: from}}
			}
			r := &Runner{cfg: Config{Store: st, WorkDir: t.TempDir(), Logger: iterlog.Nop()}}
			m1 := publish(store.RunStatusFailedResumable, true)
			if err := r.spendResumeIntent(ctx, m1, iterlog.Nop()); err != nil {
				t.Fatal(err)
			}
			if !m1.Resume.AcceptScratchLoss {
				t.Fatal("precondition: the delivery's consent was dropped while the run was queued for it")
			}
			if !requeued {
				if err := r.executeRun(ctx, m1, nil, nil); err != nil {
					t.Fatalf("the delivery of the queued attempt, with its consent: %v, want the run resumed", err)
				}
				return
			}
			time.Sleep(2 * time.Millisecond)
			if ok, err := st.UpdateRunStatusIf(ctx, runID, store.RunStatusCancelled, "operator", []store.RunStatus{store.RunStatusQueued}); err != nil || !ok {
				t.Fatalf("cancel: %v %v", ok, err)
			}
			m2 := publish(store.RunStatusCancelled, false)
			if err := r.executeRun(ctx, m1, nil, nil); !errors.Is(err, runtime.ErrResumeSuperseded) {
				t.Fatalf("the first delivery on the attempt queued after it: %v, want ErrResumeSuperseded (no claim, no consent lent)", err)
			}
			doc, err := st.LoadRun(ctx, runID)
			if err != nil || doc.Status != store.RunStatusQueued {
				t.Fatalf("after the first delivery: %v (%v), want still queued for the newer resume", doc.Status, err)
			}
			for _, ev := range eventsOfType(t, st, runID, store.EventRunResumed) {
				t.Fatalf("the first delivery resumed the run: %v", ev)
			}
			execErr := r.executeRun(ctx, m2, nil, nil)
			var rt *runtime.RuntimeError
			if !errors.As(execErr, &rt) || rt.Code != runtime.ErrCodeScratchNotPortable {
				t.Fatalf("the newer resume's own delivery, published without the consent: %v, want SCRATCH_NOT_PORTABLE", execErr)
			}
		})
	}
}

func eventsOfType(t *testing.T, st store.RunStore, runID string, typ store.EventType) []map[string]any {
	t.Helper()
	evs, err := st.LoadEvents(store.WithIdentity(context.Background(), "team-1", "u1"), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev.Data)
		}
	}
	return out
}

// TestExecuteRun_aDeliveryResumesARunNoLongerQueued: the attempt a delivery
// claims for is a queued run's; a run no longer queued — parked again by the
// sweeper before its delivery came back — is resumed from the status it is
// in, as ever.
func TestExecuteRun_aDeliveryResumesARunNoLongerQueued(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-swept"
	if _, err := st.CreateRun(ctx, runID, "main", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCheckpoint(ctx, runID, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRunStatus(ctx, runID, store.RunStatusFailedResumable, "swept"); err != nil {
		t.Fatal(err)
	}
	pr := parsertest.Parse("main.bot", "workflow main:\n  entry: done\n")
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Store: st, WorkDir: t.TempDir(), Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{RunID: runID, TenantID: "team-1", OwnerID: "u1", WorkflowName: "main", IRCompiled: body,
		PublishedAtRFC: time.Now().UTC().Format(time.RFC3339Nano),
		Resume:         &queue.ResumeSpec{PriorStatus: store.RunStatusFailedResumable}}
	if err := r.executeRun(ctx, msg, nil, nil); err != nil {
		t.Fatalf("a delivery of a run parked again before it came back: %v, want it resumed", err)
	}
	if doc, err := st.LoadRun(ctx, runID); err != nil || doc.Status != store.RunStatusFinished {
		t.Fatalf("the run is %v (%v), want finished", doc.Status, err)
	}
}

// The cloud executor may be built from an older publication snapshot. The
// bank's authoritative values are the admitted document and explicit patch.
func TestExecuteHoldingLeaseSeparatesCodemodeQuotaPatchFromReplay(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    store.RunStatus
		resume    bool
		patch     *queue.CodemodeQuotaPatch
		wantBytes int64
	}{
		{"queued explicit bytes only", store.RunStatusQueued, true, &queue.CodemodeQuotaPatch{Bytes: 800}, 800},
		{"queued silent resume", store.RunStatusQueued, true, nil, 500},
		{"redelivery of a claimed explicit resume", store.RunStatusFailedResumable, true, &queue.CodemodeQuotaPatch{Bytes: 800}, 500},
		{"launch redelivery synthesized as resume", store.RunStatusFailedResumable, false, nil, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
			ctx := store.WithIdentity(context.Background(), "team-1", "u1")
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const id = "run-codemode-quota-wire"
			queuedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			doc := &store.Run{ID: id, TenantID: "team-1", OwnerID: "u1", Status: tc.status, QueuedAt: &queuedAt,
				CodemodeBoundsOverride: &scripthost.Limits{StoreEntries: 12, StoreBytes: 500}}
			if tc.patch != nil && tc.status != store.RunStatusQueued {
				doc.CodemodeQuotaClaimedAt = &queuedAt
			}
			if err := st.SaveRun(ctx, doc); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveCheckpoint(ctx, id, &store.Checkpoint{NodeID: "done"}); err != nil {
				t.Fatal(err)
			}
			pr := parsertest.Parse("main.bot", "workflow main:\n  entry: done\n")
			body, err := ast.MarshalFile(pr.File)
			if err != nil {
				t.Fatal(err)
			}
			r := &Runner{cfg: Config{Store: st, WorkDir: t.TempDir(), HeartbeatInterval: time.Minute, Logger: iterlog.Nop()}}
			msg := &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1", WorkflowName: "main", IRCompiled: body,
				PublishedAtRFC: queuedAt.Add(time.Second).Format(time.RFC3339Nano),
				CodemodeBounds: &scripthost.Limits{StoreEntries: 6, StoreBytes: 800}}
			if tc.resume {
				msg.Resume = &queue.ResumeSpec{CodemodeQuota: tc.patch}
				if tc.patch != nil {
					tc.patch.QueuedAt = queuedAt
				}
			}
			doc, err = st.LoadRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			admitted := dispositionForStatus(msg, doc)
			if !admitted.proceed || msg.Resume == nil {
				t.Fatalf("delivery = %+v, resume = %+v; want admitted as resume", admitted, msg.Resume)
			}
			leaseCtx, cancel := context.WithCancelCause(ctx)
			if err := r.executeHoldingLease(leaseCtx, cancel, msg, admitted.preRun, &countingLease{}, nopProgress{}, nil); err != nil {
				t.Fatalf("execute resume: %v", err)
			}
			result, err := st.LoadRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != store.RunStatusFinished {
				t.Fatalf("status = %s, want finished", result.Status)
			}
			if got := result.CodemodeBoundsOverride; got == nil || got.StoreEntries != 12 || got.StoreBytes != tc.wantBytes {
				t.Fatalf("persisted quota = %+v, want entries 12 and bytes %d", got, tc.wantBytes)
			}
		})
	}
}

func TestPublishedCodemodeQuotaResumeFencesLeaseAndReceipt(t *testing.T) {
	for _, stale := range []bool{true, false} {
		name := "current marker"
		if stale {
			name = "different marker despite later publication"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
			ctx := store.WithIdentity(context.Background(), "team-1", "u1")
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const id, receipt = "published-quota-run", "quota-receipt"
			const source = "workflow main:\n  entry: done\n"
			if err := st.SaveRun(ctx, &store.Run{ID: id, TenantID: "team-1", OwnerID: "u1", Status: store.RunStatusFailedResumable,
				CodemodeBoundsOverride: &scripthost.Limits{StoreEntries: 12, StoreBytes: 500}}); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveCheckpoint(ctx, id, &store.Checkpoint{NodeID: "done"}); err != nil {
				t.Fatal(err)
			}
			var delivery queue.RunMessage
			publisher := cloudpublisher.NewForTesting(cloudpublisher.Config{Store: st, Logger: iterlog.Nop()}, func(_ context.Context, msg *queue.RunMessage) error {
				if err := msg.Validate(); err != nil {
					return err
				}
				body, err := json.Marshal(msg)
				if err != nil {
					return err
				}
				return json.Unmarshal(body, &delivery)
			})
			if err := publisher.SubmitResume(ctx, runview.ResumeSpec{RunID: id, FilePath: "main.bot", Source: source, ReceiptID: receipt,
				CodemodeBounds: &ir.CodemodeBounds{StoreBytes: 800}}, &ir.Workflow{Name: "main"}, nil); err != nil {
				t.Fatal(err)
			}
			r := &Runner{cfg: Config{Store: st, WorkDir: t.TempDir(), HeartbeatInterval: time.Minute, Logger: iterlog.Nop()}}
			pre := r.resolveDeliveryPreconditions(&delivery)
			if !pre.proceed {
				t.Fatalf("publisher's exact delivery not admitted: %+v", pre)
			}
			var newer store.QueuedFlip
			if stale {
				if err := st.UpdateRunStatus(ctx, id, store.RunStatusFailedResumable, "second attempt"); err != nil {
					t.Fatal(err)
				}
				var changed bool
				newer, changed, err = st.FlipToQueued(ctx, id, store.RunStatusFailedResumable, delivery.Resume.CodemodeQuota.QueuedAt)
				if err != nil || !changed {
					t.Fatalf("flip to second queued attempt: changed=%v err=%v", changed, err)
				}
				published, err := time.Parse(time.RFC3339Nano, delivery.PublishedAtRFC)
				if err != nil || published.Before(newer.At) {
					t.Fatalf("fixture's older publication must be >= newer marker: published=%s marker=%s err=%v", published, newer.At, err)
				}
				if now := r.resolveDeliveryPreconditions(&delivery); now.proceed || now.op != "nak-stale-attempt-unclaimed" {
					t.Errorf("admission after the flip = %+v, want superseded", now)
				}
			}
			before, err := st.LoadRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			leaseCtx, cancel := context.WithCancelCause(ctx)
			err = r.executeHoldingLease(leaseCtx, cancel, &delivery, pre.preRun, &countingLease{}, nopProgress{}, nil)
			if (stale && !errors.Is(err, runtime.ErrResumeSuperseded)) || (!stale && err != nil) {
				t.Fatalf("execute exact delivery: stale=%t err=%v", stale, err)
			}
			if stale {
				// These paths receive errors after an initially admitted
				// message has lost its marker. Neither may touch the new row.
				cause := errors.New("preflight failed")
				r.recordPreEngineRefusal(ctx, &delivery, cause, preEngineVerdict{
					Phase: "workflow", Code: store.FailureExecutionFailed, Disposition: store.PreEngineRetryPending,
				})
				if released := r.releaseRefusedResume(&delivery, cause, iterlog.Nop()); released != "" {
					t.Errorf("old refusal released the new attempt to %s", released)
				}
			}
			after, err := st.LoadRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if stale {
				if after.Status != store.RunStatusQueued || after.CASVersion != before.CASVersion || after.QueuedAt == nil || !after.QueuedAt.Equal(newer.At) ||
					after.CodemodeQuotaClaimedAt != nil || after.CodemodeBoundsOverride == nil || after.CodemodeBoundsOverride.StoreBytes != 500 {
					t.Fatalf("superseded delivery changed the new attempt: version %d -> %d, status=%s bounds=%+v", before.CASVersion, after.CASVersion, after.Status, after.CodemodeBoundsOverride)
				}
			} else if after.Status != store.RunStatusFinished || after.CodemodeQuotaClaimedAt == nil || !after.CodemodeQuotaClaimedAt.Equal(delivery.Resume.CodemodeQuota.QueuedAt) ||
				after.CodemodeBoundsOverride == nil || after.CodemodeBoundsOverride.StoreBytes != 800 || after.CodemodeBoundsOverride.StoreEntries != 12 {
				t.Fatalf("claim did not persist partial quota and claim proof: status=%s bounds=%+v marker=%v", after.Status, after.CodemodeBoundsOverride, after.CodemodeQuotaClaimedAt)
			}
			events, err := st.LoadEvents(ctx, id)
			if err != nil && !stale {
				t.Fatal(err)
			}
			resumed := 0
			for _, ev := range events {
				if ev.Type == store.EventRunResumed {
					resumed++
					if ev.Data["receipt_id"] != receipt {
						t.Errorf("wrong resume receipt: %v", ev.Data["receipt_id"])
					}
				}
			}
			if stale && resumed != 0 || !stale && resumed != 1 {
				t.Fatalf("wrong resume receipt count: stale=%t count=%d", stale, resumed)
			}
		})
	}
}

// An unreadable document is not evidence that a patch has been spent.
func TestRunnerDoesNotSilentlyDropCodemodeQuotaWhenAttemptReadFails(t *testing.T) {
	unavailable := errors.New("store unavailable")
	r := &Runner{cfg: Config{Store: quotaAttemptReadErrorStore{err: unavailable}}}
	msg := &queue.RunMessage{RunID: "run-unreadable", Resume: &queue.ResumeSpec{CodemodeQuota: &queue.CodemodeQuotaPatch{Bytes: 800}}}
	if err := r.spendResumeIntent(context.Background(), msg, iterlog.Nop()); !errors.Is(err, unavailable) {
		t.Fatalf("attempt error = %v, want the store error for redelivery", err)
	}
	if msg.Resume.CodemodeQuota == nil {
		t.Fatal("an unreadable attempt must not spend its patch")
	}
}

type quotaAttemptReadErrorStore struct {
	store.RunStore
	err error
}

func (s quotaAttemptReadErrorStore) LoadRun(context.Context, string) (*store.Run, error) {
	return nil, s.err
}
