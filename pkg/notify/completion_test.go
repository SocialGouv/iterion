package notify

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// newTestStore builds a filesystem-backed RunStore rooted in a temp dir.
func newTestStore(t *testing.T) *store.FilesystemRunStore {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return st
}

// seedRun creates a run, then patches the callback + status fields and
// (optionally) writes a final_answer artifact on answerNode.
func seedRun(t *testing.T, st *store.FilesystemRunStore, runID, callbackURL, token, answerNode, answer string, status store.RunStatus) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, runID, "clarify", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	run.CallbackURL = callbackURL
	run.CallbackToken = token
	run.CallbackAnswerNode = answerNode
	run.Status = status
	if answer != "" {
		if err := st.WriteArtifact(ctx, &store.Artifact{
			RunID:     runID,
			NodeID:    answerNode,
			Version:   0,
			Data:      map[string]any{DefaultAnswerField: answer},
			WrittenAt: time.Unix(0, 0).UTC(),
		}); err != nil {
			t.Fatalf("WriteArtifact: %v", err)
		}
		// WriteArtifact stamps Run.ArtifactIndex; reload so SaveRun below
		// preserves it.
		if run, err = st.LoadRun(ctx, runID); err != nil {
			t.Fatalf("LoadRun (post-artifact): %v", err)
		}
		run.CallbackURL = callbackURL
		run.CallbackToken = token
		run.CallbackAnswerNode = answerNode
		run.Status = status
	}
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
}

// captureServer is an httptest server that records the single payload it
// receives.
type captureServer struct {
	*httptest.Server
	mu      sync.Mutex
	payload *CompletionPayload
	hits    int
}

func newCaptureServer(t *testing.T) *captureServer {
	t.Helper()
	cs := &captureServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p CompletionPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		cs.mu.Lock()
		cs.payload = &p
		cs.hits++
		cs.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *captureServer) got() (*CompletionPayload, int) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.payload, cs.hits
}

// allowPrivateNotifier returns a Notifier that permits loopback URLs so
// tests can target httptest's 127.0.0.1 server.
func allowPrivateNotifier() *Notifier {
	return New(nil, 2*time.Second, WithAllowPrivate(true))
}

func TestFireForRun_FinishedWithAnswer(t *testing.T) {
	st := newTestStore(t)
	cs := newCaptureServer(t)
	seedRun(t, st, "run-1", cs.URL, "thread-42", "answer", "Here is the clarification.", store.RunStatusFinished)

	allowPrivateNotifier().FireForRun(context.Background(), st, "run-1")

	p, hits := cs.got()
	if hits != 1 {
		t.Fatalf("want 1 delivery, got %d", hits)
	}
	if p.V != PayloadVersion {
		t.Errorf("V = %d, want %d", p.V, PayloadVersion)
	}
	if p.Status != string(store.RunStatusFinished) {
		t.Errorf("Status = %q, want finished", p.Status)
	}
	if p.FinalAnswer != "Here is the clarification." {
		t.Errorf("FinalAnswer = %q", p.FinalAnswer)
	}
	if p.FinalAnswerN != "answer" {
		t.Errorf("FinalAnswerN = %q, want answer", p.FinalAnswerN)
	}
	if p.CallbackToken != "thread-42" {
		t.Errorf("CallbackToken = %q, want thread-42", p.CallbackToken)
	}
}

func TestFireForRun_FailedDeliversWithErrorNoAnswer(t *testing.T) {
	st := newTestStore(t)
	cs := newCaptureServer(t)
	seedRun(t, st, "run-2", cs.URL, "thread-9", "answer", "", store.RunStatusFailed)
	// Patch the error message onto the run.
	ctx := context.Background()
	run, _ := st.LoadRun(ctx, "run-2")
	run.Error = "boom"
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	allowPrivateNotifier().FireForRun(ctx, st, "run-2")

	p, hits := cs.got()
	if hits != 1 {
		t.Fatalf("want 1 delivery, got %d", hits)
	}
	if p.Status != string(store.RunStatusFailed) {
		t.Errorf("Status = %q, want failed", p.Status)
	}
	if p.Error != "boom" {
		t.Errorf("Error = %q, want boom", p.Error)
	}
	if p.FinalAnswer != "" {
		t.Errorf("FinalAnswer = %q, want empty", p.FinalAnswer)
	}
}

func TestFireForRun_NoCallbackURL_NoDelivery(t *testing.T) {
	st := newTestStore(t)
	cs := newCaptureServer(t)
	seedRun(t, st, "run-3", "", "", "answer", "ignored", store.RunStatusFinished)

	allowPrivateNotifier().FireForRun(context.Background(), st, "run-3")

	if _, hits := cs.got(); hits != 0 {
		t.Fatalf("want 0 deliveries (no callback URL), got %d", hits)
	}
}

func TestFireForRun_RunningStatus_NoDelivery(t *testing.T) {
	st := newTestStore(t)
	cs := newCaptureServer(t)
	seedRun(t, st, "run-4", cs.URL, "t", "answer", "x", store.RunStatusRunning)

	allowPrivateNotifier().FireForRun(context.Background(), st, "run-4")

	if _, hits := cs.got(); hits != 0 {
		t.Fatalf("want 0 deliveries (still running), got %d", hits)
	}
}

func TestFireForRun_SSRFGuardBlocksLoopback(t *testing.T) {
	st := newTestStore(t)
	cs := newCaptureServer(t)
	seedRun(t, st, "run-5", cs.URL, "t", "answer", "x", store.RunStatusFinished)

	// Default notifier (no WithAllowPrivate) must refuse the loopback
	// httptest URL — no delivery reaches the server.
	New(nil, 2*time.Second).FireForRun(context.Background(), st, "run-5")

	if _, hits := cs.got(); hits != 0 {
		t.Fatalf("want 0 deliveries (SSRF guard), got %d", hits)
	}
}

// stubResolver answers the guard's lookup without touching DNS (#2080): it
// records the hosts it was asked for and answers each with one fixed IP.
// The default is TEST-NET-3 (203.0.113.10) — never routable, yet public
// unicast per httpdial.IsPublicUnicast, which blocks the conventional SSRF
// categories (loopback, private, link-local, multicast, unspecified) plus
// the cloud metadata endpoints.
type stubResolver struct {
	mu      sync.Mutex
	lookups []string
	ip      net.IP
}

func newStubResolver(ip string) *stubResolver {
	return &stubResolver{ip: net.ParseIP(ip)}
}

// LookupIPAddr implements httpdial.HostResolver.
func (s *stubResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookups = append(s.lookups, host)
	return []net.IPAddr{{IP: s.ip}}, nil
}

func (s *stubResolver) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lookups...)
}

func TestVetURL(t *testing.T) {
	tests := []struct {
		name         string
		url          string
		allowPrivate bool
		wantErr      bool
		wantLookup   bool // a hostname the guard must resolve through the injected stub
	}{
		{"https public", "https://example.com/hook", false, false, true},
		{"http public", "http://example.com/hook", false, false, true},
		{"ftp scheme", "ftp://example.com", false, true, false},
		{"no host", "https://", false, true, false},
		{"loopback blocked", "http://127.0.0.1:9000/cb", false, true, false},
		{"loopback allowed", "http://127.0.0.1:9000/cb", true, false, false},
		{"private blocked", "http://10.0.0.5/cb", false, true, false},
		{"cluster alias blocked", "http://foo.svc.cluster.local/cb", false, true, false},
		{"metadata alias blocked", "http://metadata.google.internal/cb", false, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Every row vets through the stub, so the table is hermetic: the
			// public rows prove the seam is what resolves, the literal and
			// alias rows prove the guard refuses BEFORE any lookup.
			stub := newStubResolver("203.0.113.10")
			n := New(nil, time.Second, WithAllowPrivate(tc.allowPrivate), WithResolver(stub))
			err := n.vetURL(tc.url)
			if tc.wantErr && err == nil {
				t.Errorf("vetURL(%q) = nil, want error", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("vetURL(%q) = %v, want nil", tc.url, err)
			}
			if tc.wantLookup {
				if asked := stub.asked(); len(asked) == 0 {
					t.Errorf("vetURL(%q) answered without the injected resolver — the test was reading live DNS", tc.url)
				}
				// The pinned delivery resolves through the same seam, so the
				// address it dials is the one the stub answered with.
				ip, rerr := n.resolveCallbackIP(context.Background(), "example.com")
				if rerr != nil {
					t.Errorf("resolveCallbackIP(example.com): %v", rerr)
				} else if !ip.Equal(stub.ip) {
					t.Errorf("resolveCallbackIP(example.com) = %v, want the stub's %v", ip, stub.ip)
				}
			}
			if !tc.wantLookup {
				if asked := stub.asked(); len(asked) != 0 {
					t.Errorf("vetURL(%q) looked up %v: the guard must refuse literals and reserved aliases before any DNS", tc.url, asked)
				}
			}
		})
	}
}

func TestNilNotifierIsNoop(t *testing.T) {
	var n *Notifier
	// Must not panic.
	n.FireForRun(context.Background(), newTestStore(t), "missing")
}

// TestFireForRun_UsageParkWaitsForSettlement is #2707's witness: a run
// parked on a provider usage wall is terminal for POLLING but not done —
// the callback is keyed on the persisted wall (RetryState.Wall), so a run
// parked twice on one wall delivers NOTHING while it waits, and ONE
// callback at settlement, when the retry is retired and the wall is gone.
func TestFireForRun_UsageParkWaitsForSettlement(t *testing.T) {
	st := newTestStore(t)
	cs := newCaptureServer(t)
	seedRun(t, st, "run-parked", cs.URL, "thread-7", "", "", store.RunStatusFailedResumable)

	ctx := context.Background()
	wall := time.Now().UTC().Add(4 * 24 * time.Hour).Truncate(time.Second)
	wake := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	run, err := st.LoadRun(ctx, "run-parked")
	if err != nil {
		t.Fatal(err)
	}
	run.RetryState = &store.RunRetryState{
		RetryAfter: &wake, Wall: &wall,
		Reason: store.RetryReasonUsageWindow, Code: "USAGE_LIMIT_BLOCKED", Attempts: 1,
	}
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	n := allowPrivateNotifier()
	// Parked, then re-parked on the SAME wall (a probe's free re-arm): the
	// run is waiting, and the receiver that asked for "done" hears nothing.
	n.FireForRun(ctx, st, "run-parked")
	n.FireForRun(ctx, st, "run-parked")
	if _, hits := cs.got(); hits != 0 {
		t.Fatalf("a run parked twice on one wall delivered %d callbacks, want 0 — a usage park is a wait, not a completion", hits)
	}

	// Settlement: the retry is retired (the sweeper's abandon disarm), so
	// the wall is gone and the run delivers — once, and as what it is.
	run, err = st.LoadRun(ctx, "run-parked")
	if err != nil {
		t.Fatal(err)
	}
	run.RetryState.RetryAfter = nil
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	n.FireForRun(ctx, st, "run-parked")
	payload, hits := cs.got()
	if hits != 1 {
		t.Fatalf("the settled run delivered %d callbacks, want 1", hits)
	}
	if payload.Status != string(store.RunStatusFailedResumable) {
		t.Errorf("status = %q, want %q", payload.Status, store.RunStatusFailedResumable)
	}
	if payload.CallbackToken != "thread-7" {
		t.Errorf("callback_token = %q, want thread-7", payload.CallbackToken)
	}
}

// TestFireForRun_LegacyParkedRowKeepsThePerTransitionBehaviour pins the
// boundary the wall's contract states: a row armed before the wall was
// persisted carries none, nothing can be told apart per wall, and it keeps
// what it shipped with — a delivery per park — rather than going silent on
// a run nobody will settle.
func TestFireForRun_LegacyParkedRowKeepsThePerTransitionBehaviour(t *testing.T) {
	st := newTestStore(t)
	cs := newCaptureServer(t)
	seedRun(t, st, "run-legacy", cs.URL, "", "", "", store.RunStatusFailedResumable)

	ctx := context.Background()
	wake := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	run, err := st.LoadRun(ctx, "run-legacy")
	if err != nil {
		t.Fatal(err)
	}
	run.RetryState = &store.RunRetryState{
		RetryAfter: &wake,
		Reason:     store.RetryReasonUsageWindow, Code: "USAGE_LIMIT_BLOCKED", Attempts: 1,
	}
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	allowPrivateNotifier().FireForRun(ctx, st, "run-legacy")
	if _, hits := cs.got(); hits != 1 {
		t.Fatalf("a legacy parked row (no persisted wall) delivered %d callbacks, want 1 — the per-transition behaviour it shipped with", hits)
	}
}
