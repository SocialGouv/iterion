package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newAlertTestServer is newTestServer with the run console's alerts wired:
// the runview service builds its alert manager over the native store, which
// is what puts the registry lane behind the run socket.
func newAlertTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	workDir := t.TempDir()
	logger := iterlog.New(iterlog.LevelError, os.Stderr) // quiet
	srv := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		SkipProjectRegistration: true,
		Alerts:                  &runview.AlertSettings{},
	}, logger)
	if srv.runs == nil {
		t.Fatalf("expected run console service to be wired")
	}
	if srv.runs.AlertManager() == nil {
		t.Fatalf("expected the alert manager to be wired")
	}
	streams := &testOwnedRunStreams{Source: srv.runs.StreamSource()}
	runview.WithStreamSource(streams)(srv.runs)
	hs := httptest.NewServer(srv.mux)
	t.Cleanup(func() {
		hs.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		if err := streams.Close(); err != nil {
			t.Errorf("join run streams: %v", err)
		}
	})
	return srv, hs
}

// TR7 (R54 slice 3): a REAL manager alert rides the whole new path on the
// wire — the tailer feeds the manager, the alert's captured route publishes
// to the registry lane, the native merge ships it, and the run socket
// delivers the envelope. The direct-injection test
// (TestRunsWS_AlertEventBypassesSnapshotDedup) stays as the legacy
// broker-lane control; this one fires the manager itself.
func TestRunsWS_ManagerAlertRidesTheRegistryLane(t *testing.T) {
	srv, hs := newAlertTestServer(t)
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	const runID = "run-registry-alert"
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for _, evt := range []store.Event{
		{Type: store.EventRunStarted, RunID: runID},
		{Type: store.EventNodeStarted, RunID: runID, NodeID: "analyze"},
	} {
		if _, err := st.AppendEvent(context.Background(), runID, evt); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	c := dialRunWS(t, hs, runID)
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
	snapEnv := readEnvelope(t, c, wsTypeSnapshot)
	var snap runview.RunSnapshot
	if err := json.Unmarshal(snapEnv.Payload, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	awaitBrokerSubscriber(t, srv, runID)

	// A budget warning appended to the run: the tailer observes it, the
	// manager fires on its captured registration, and the registry lane
	// delivers the frame through the merge to this socket.
	r, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindRunValueFormat(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(bound, runID, store.Event{Type: store.EventBudgetWarning, RunID: runID, NodeID: "analyze", Data: map[string]any{"dimension": "tokens"}}); err != nil {
		t.Fatalf("append budget warning: %v", err)
	}
	// The persisted budget row ships first (the tailer's republication);
	// keep reading until the manager's own alert frame arrives.
	var ev store.Event
	deadline := time.Now().Add(10 * time.Second)
	for {
		env := readEnvelopeWithin(t, c, time.Until(deadline), wsTypeEvent)
		if err := json.Unmarshal(env.Payload, &ev); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if ev.Type == store.EventAlert {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the manager alert never reached the wire within 10s")
		}
	}
	// The origin stamp rides the event's private field, which the wire
	// encoding does not carry; the in-process hop that stamps it is pinned
	// by the runview suite (TR2/TR3).
}
