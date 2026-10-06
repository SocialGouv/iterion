package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// The relay's entry check (D14 F5/F6): a run-lifecycle event is only
// relayed when the store — the authority a compromised pod cannot
// forge — agrees with its claimed tenant and kind. A forged event is
// dropped, never matched against subscriptions.
func TestTriggerRelayVerifiesRunEventAuthority(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "store"), store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-real", "wf", nil); err != nil {
		t.Fatal(err)
	}
	r, err := st.LoadRun(ctx, "run-real")
	if err != nil {
		t.Fatal(err)
	}
	r.TenantID = "team-a"
	r.Status = store.RunStatusFinished
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}

	var calls int
	var lastSeen trigger.Event
	h := verifyRunEventAuthority(st, iterlog.Nop(), func(ctx context.Context, ev trigger.Event) error {
		calls++
		lastSeen = ev
		return nil
	})

	runEvent := func(tenant, kind, runID string) trigger.Event {
		return trigger.Event{
			Source: trigger.SourceRun, Kind: kind, TenantID: tenant,
			Subject: trigger.Subject{Type: "run", ID: runID},
		}
	}

	// The honest event relays — REBUILT from the document: an attacker
	// replaying a real (run, tenant, kind) with forged payload vars must
	// not see those vars reach the handler (Reb02b9: the launch plan
	// consumes payload vars).
	forge := runEvent("team-a", trigger.KindRunFinished, "run-real")
	forge.Payload = map[string]any{"vars": map[string]string{"injected": "attacker"}, "args": "evil"}
	forge.Subject.Title = "attacker title"
	if err := h(ctx, forge); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("honest event: %d handler calls, want 1", calls)
	}
	if _, injected := lastSeen.Payload["vars"]; injected {
		t.Fatalf("the handler saw the wire's payload vars: %+v — the event was not rebuilt from the document", lastSeen.Payload)
	}
	if lastSeen.Subject.Title == "attacker title" {
		t.Fatal("the handler saw the wire's subject title; the event was not rebuilt from the document")
	}

	// Forged tenant: dropped.
	if err := h(ctx, runEvent("team-b", trigger.KindRunFinished, "run-real")); err != nil {
		t.Fatal(err)
	}
	// Forged kind: the run is finished, not failed.
	if err := h(ctx, runEvent("team-a", trigger.KindRunFailed, "run-real")); err != nil {
		t.Fatal(err)
	}
	// Unknown run: dropped.
	if err := h(ctx, runEvent("team-a", trigger.KindRunFinished, "run-ghost")); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("forgeries relayed: %d handler calls, want 1", calls)
	}

	// A non-run source passes through unverified (its own admission governs).
	if err := h(ctx, trigger.Event{Source: "forge", Kind: "forge.push", TenantID: "team-z"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("non-run event dropped: %d handler calls, want 2", calls)
	}

	// The nil-store path (local dispatch) passes everything through.
	passthrough := verifyRunEventAuthority(nil, iterlog.Nop(), func(ctx context.Context, ev trigger.Event) error {
		calls++
		return nil
	})
	if err := passthrough(ctx, runEvent("team-b", trigger.KindRunFinished, "run-ghost")); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("nil-store passthrough broken: %d handler calls, want 3", calls)
	}

	// The wire still validates a real v24 message shape end to end.
	m := &queue.RunMessage{V: queue.SchemaVersion, RunID: "r", WorkflowName: "w", SecretsRef: "ref", IRCompiled: []byte("{}"), BundleDEK: make([]byte, 32)}
	if err := m.Validate(); err != nil {
		t.Fatalf("v24 wire contract drifted: %v", err)
	}
}

// The wiring pin: the coordinator subscribes through the verifier, not
// around it — the function without the wire is dead code, and a wire
// that skips the function is the forge the check exists to stop.
func TestTriggerRelayVerifierIsWired(t *testing.T) {
	src, err := os.ReadFile("trigger_coordinator.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "verifyRunEventAuthority(runs, logger, eval.Handle)") {
		t.Fatal("the trigger coordinator no longer subscribes through the run-event authority check (D14 F5/F6)")
	}
}
