package runner

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
)

func TestRunnerEpochAdmissionRelation(t *testing.T) {
	for _, tc := range []struct {
		self    uint64
		message uint64
		want    bool
	}{
		{self: 2, message: 0, want: true},
		{self: 2, message: 1, want: true},
		{self: 2, message: 2, want: true},
		{self: 2, message: 3, want: false},
	} {
		if got := runnerEpochAccepted(tc.self, tc.message); got != tc.want {
			t.Errorf("self=%d message=%d accepted=%t, want %t", tc.self, tc.message, got, tc.want)
		}
	}
}

func TestPlanAdmissionMismatch(t *testing.T) {
	env := queue.Envelope{V: queue.SchemaVersion + 1, RunnerEpoch: 7}
	mismatchErr := errors.New("incompatible delivery")

	for _, tc := range []struct {
		name          string
		kind          admissionMismatchKind
		wantDelay     time.Duration
		wantRunErr    string
		wantRecovery  string
		wantLostError string
	}{
		{
			name:          "schema",
			kind:          admissionMismatchSchema,
			wantDelay:     natsq.SchemaMismatchNakDelay,
			wantRunErr:    "schema version mismatch",
			wantRecovery:  "cloud-queue-schema-rollout.md",
			wantLostError: "schema version mismatch",
		},
		{
			name:          "future epoch",
			kind:          admissionMismatchFutureEpoch,
			wantDelay:     natsq.EpochMismatchNakDelay,
			wantRunErr:    "runner epoch mismatch",
			wantRecovery:  "cloud-deployment.md",
			wantLostError: "runner epoch mismatch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := planAdmissionMismatch(tc.kind, mismatchErr, 0, env, 1, 2)
			if plan.reason != string(tc.kind) {
				t.Fatalf("reason = %q, want %q", plan.reason, tc.kind)
			}
			if plan.delay != tc.wantDelay {
				t.Fatalf("delay = %v, want %v", plan.delay, tc.wantDelay)
			}
			if plan.final {
				t.Fatal("first delivery must be delayed, not parked")
			}
			if !strings.Contains(plan.parkedRunError, tc.wantRunErr) || !strings.Contains(plan.parkedRunError, tc.wantRecovery) {
				t.Fatalf("parked run error = %q, want %q and %q", plan.parkedRunError, tc.wantRunErr, tc.wantRecovery)
			}
			if got := plan.lostRunError(mismatchErr, errors.New("broker down")); !strings.Contains(got, tc.wantLostError) || !strings.Contains(got, "no queue copy remains") {
				t.Fatalf("lost run error = %q", got)
			}

			final := planAdmissionMismatch(tc.kind, mismatchErr, time.Second, env, 2, 2)
			if !final.final {
				t.Fatal("last delivery must be parked")
			}
			if final.delay != time.Second {
				t.Fatalf("configured delay = %v, want 1s", final.delay)
			}
		})
	}

	if plan := planAdmissionMismatch(admissionMismatchFutureEpoch, mismatchErr, 0, env, 99, 0); plan.final {
		t.Fatal("a missing delivery budget must remain on the delayed-Nak path")
	}
}

// The admission park stamps the same typed WHY the generic DLQ park in
// processOne writes: a parked payload is DLQ_PARKED whatever fence
// refused it (the gate notice keys on the code, and read "" as a quota
// pause on this path — #669 part 4). A payload the DLQ could not take
// keeps the refusal's own cause where one exists and stays honestly
// unknown otherwise; every outcome is final.
func TestAdmissionParkOutcome(t *testing.T) {
	cases := []struct {
		name   string
		kind   admissionMismatchKind
		parked bool
		want   store.FailureCode
	}{
		{"schema fence, parked", admissionMismatchSchema, true, store.FailureDLQParked},
		{"future-epoch fence, parked", admissionMismatchFutureEpoch, true, store.FailureDLQParked},
		{"schema fence, DLQ refused the payload", admissionMismatchSchema, false, store.FailureQueueSchemaMismatch},
		{"future-epoch fence, DLQ refused the payload", admissionMismatchFutureEpoch, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := admissionParkOutcome(c.kind, c.parked)
			if got.Code != c.want {
				t.Fatalf("code = %q, want %q", got.Code, c.want)
			}
			if got.Continuation != store.ContinuationFinal {
				t.Fatalf("continuation = %q, want final — nothing on the platform wakes an admission-parked run", got.Continuation)
			}
		})
	}
}
