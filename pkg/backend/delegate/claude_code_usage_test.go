package delegate

import (
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

func ptrF(v float64) *float64 { return &v }
func ptrI(v int64) *int64     { return &v }

// The two scale conversions are the whole risk of this handler: the CLI
// reports utilization as a FRACTION and resetsAt as Unix SECONDS. Reading
// either wrong turns a cap into a number that means nothing — 0.92 read as
// 92% consumed vs 0.92% consumed decides whether a run stops.
func TestHandleRateLimitEvent_Conversion(t *testing.T) {
	var got usagecap.Reading
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	task := Task{NodeID: "implement", Hooks: TaskHooks{
		OnUsageWindow: func(r usagecap.Reading) error { got = r; return nil },
	}}

	err := b.handleRateLimitEvent(&claudesdk.RateLimitEvent{
		Info: claudesdk.RateLimitInfo{
			Status:        "allowed_warning",
			RateLimitType: "seven_day",
			Utilization:   ptrF(0.92),
			ResetsAt:      ptrI(1787086800),
		},
	}, task, func() {})
	if err != nil {
		t.Fatalf("observer returning nil must let the session run on, got %v", err)
	}
	if got.Window != usagecap.WindowSevenDay {
		t.Errorf("window = %q", got.Window)
	}
	if got.Utilization != 0.92 || got.Percent() != 92 {
		t.Errorf("utilization = %v (%.0f%%), want the fraction 0.92 read as 92%%", got.Utilization, got.Percent())
	}
	if want := time.Unix(1787086800, 0).UTC(); !got.ResetsAt.Equal(want) {
		t.Errorf("resetsAt = %v, want %v — epoch SECONDS", got.ResetsAt, want)
	}
	if got.Status != "allowed_warning" || got.ObservedAt.IsZero() {
		t.Errorf("reading = %+v, want the status carried and an observation instant", got)
	}
}

// A refusal carries a status and nothing else. Absent numbers must stay
// zero-valued rather than being invented, and the policy layer decides.
func TestHandleRateLimitEvent_AbsentNumbers(t *testing.T) {
	var got usagecap.Reading
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	task := Task{Hooks: TaskHooks{OnUsageWindow: func(r usagecap.Reading) error { got = r; return nil }}}

	if err := b.handleRateLimitEvent(&claudesdk.RateLimitEvent{
		Info: claudesdk.RateLimitInfo{Status: "rejected", RateLimitType: "five_hour"},
	}, task, func() {}); err != nil {
		t.Fatal(err)
	}
	if got.Utilization != 0 || !got.ResetsAt.IsZero() {
		t.Errorf("reading = %+v, want absent numbers left absent", got)
	}
	if got.Status != usagecap.StatusRejected {
		t.Errorf("status = %q, want the refusal carried through", got.Status)
	}
}

// The enforcement seam: the backend reports and obeys, the executor decides.
func TestHandleRateLimitEvent_HookErrorAbortsTheSession(t *testing.T) {
	sentinel := errors.New("usage cap: week at 99%")
	cancelled := false
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	task := Task{Hooks: TaskHooks{OnUsageWindow: func(usagecap.Reading) error { return sentinel }}}

	err := b.handleRateLimitEvent(&claudesdk.RateLimitEvent{
		Info: claudesdk.RateLimitInfo{Status: "allowed_warning", RateLimitType: "seven_day", Utilization: ptrF(0.99)},
	}, task, func() { cancelled = true })

	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the hook's own error surfaced verbatim", err)
	}
	if !cancelled {
		// Without this the CLI subprocess keeps streaming — and keeps
		// spending — after iterion decided to stop.
		t.Error("the stream must be cancelled when the cap fires")
	}
}

// Nobody watching must never be a reason for a session to fail.
func TestHandleRateLimitEvent_NoObserver(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	cancelled := false
	if err := b.handleRateLimitEvent(&claudesdk.RateLimitEvent{
		Info: claudesdk.RateLimitInfo{Status: "rejected"},
	}, Task{}, func() { cancelled = true }); err != nil {
		t.Fatalf("unexpected error with no hook wired: %v", err)
	}
	if cancelled {
		t.Error("cancelled a session with no cap configured")
	}
	if err := b.handleRateLimitEvent(nil, Task{Hooks: TaskHooks{
		OnUsageWindow: func(usagecap.Reading) error { return errors.New("must not run") },
	}}, func() {}); err != nil {
		t.Fatalf("a nil event must be ignored, got %v", err)
	}
}
