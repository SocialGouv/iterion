package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
)

// The DLQ admin surface (`iterion remote admin dlq show|replay|delete`
// → GET/POST/DELETE /api/admin/dlq*) is the operator's only handle on a
// poisoned run message: it lists what the queue parked, re-enqueues a
// message for another attempt, or drops it for good. Replaying is a
// privileged, side-effectful act — it puts a run back on the live work
// queue — so the endpoints must be super-admin-only and must leave an
// audit trail naming the run.
//
// The queue itself is a live NATS JetStream broker (nats-server is not
// vendored), so these tests drive the REST surface against an in-process
// QueueBackend double holding parked messages. What is asserted is what
// the SERVER does: who it lets through, which message it acts on, the
// state transition the operator observes on the next list, and the
// platform audit row that lands.

// fakeDLQQueue is an in-process QueueBackend holding a handful of parked
// messages. Replay/discard mutate it, so a list after either shows the
// message gone — the transition an operator sees in the admin view.
type fakeDLQQueue struct {
	mu sync.Mutex
	// parked maps DLQ sequence → the parked message + its raw payload.
	parked map[uint64]parkedMsg
	// republished records the run ids handed back to the live subject,
	// in call order — the side effect a non-admin must never trigger.
	republished []string
	discarded   []uint64
	failReplay  error
}

type parkedMsg struct {
	view    natsq.DLQMessage
	payload json.RawMessage
}

func newFakeDLQQueue() *fakeDLQQueue {
	return &fakeDLQQueue{parked: map[uint64]parkedMsg{}}
}

func (q *fakeDLQQueue) park(seq uint64, runID, reason string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.parked[seq] = parkedMsg{
		view: natsq.DLQMessage{
			Seq: seq, RunID: runID, TenantID: "team-1", Reason: reason,
			NumDelivered: "8", ParkedAt: time.Unix(1700000000, 0).UTC(), Size: 42,
		},
		payload: json.RawMessage(fmt.Sprintf(`{"run_id":%q,"schema_version":6}`, runID)),
	}
}

func (q *fakeDLQQueue) ListDLQ(_ context.Context, cursorSeq uint64, limit int) ([]natsq.DLQMessage, uint64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	seqs := make([]uint64, 0, len(q.parked))
	for s := range q.parked {
		if s >= cursorSeq {
			seqs = append(seqs, s)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	out := make([]natsq.DLQMessage, 0, limit)
	var next uint64
	for i, s := range seqs {
		if i >= limit {
			next = s
			break
		}
		out = append(out, q.parked[s].view)
	}
	return out, next, nil
}

func (q *fakeDLQQueue) PeekDLQ(_ context.Context, seq uint64) (natsq.DLQMessage, json.RawMessage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	m, ok := q.parked[seq]
	if !ok {
		return natsq.DLQMessage{}, nil, fmt.Errorf("dlq get %d: message not found", seq)
	}
	return m.view, m.payload, nil
}

func (q *fakeDLQQueue) RepublishDLQ(_ context.Context, seq uint64) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failReplay != nil {
		return "", q.failReplay
	}
	m, ok := q.parked[seq]
	if !ok {
		return "", fmt.Errorf("dlq get %d: message not found", seq)
	}
	delete(q.parked, seq)
	q.republished = append(q.republished, m.view.RunID)
	return m.view.RunID, nil
}

func (q *fakeDLQQueue) DiscardDLQ(_ context.Context, seq uint64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.parked[seq]; !ok {
		return fmt.Errorf("dlq delete %d: message not found", seq)
	}
	delete(q.parked, seq)
	q.discarded = append(q.discarded, seq)
	return nil
}

func (q *fakeDLQQueue) DLQDepth(_ context.Context) (uint64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return uint64(len(q.parked)), nil
}

func (q *fakeDLQQueue) IsRunLocked(context.Context, string) (bool, error) { return false, nil }

func (q *fakeDLQQueue) snapshot() ([]string, []uint64, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.republished...), append([]uint64(nil), q.discarded...), len(q.parked)
}

// newDLQAdminServer boots a cloud-shaped server (auth stack + audit
// store + queue) through the real New()/routes() path, so the DLQ
// endpoints are registered and reached through the production auth
// middleware. Returns the live HTTP server plus a super-admin and a
// plain-member bearer.
func newDLQAdminServer(t *testing.T) (*fakeDLQQueue, audit.Store, *httptest.Server, string, string) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	svc, err := auth.NewService(auth.Config{
		Store:      identity.NewMemoryStore(),
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	q := newFakeDLQQueue()
	auditStore := audit.NewMemoryStore()
	s := New(Config{
		WorkDir:                 t.TempDir(),
		SkipProjectRegistration: true,
		AuthService:             svc,
		AuthSigner:              signer,
		Audit:                   auditStore,
		Queue:                   q,
	}, iterlog.New(iterlog.LevelError, nil))

	adminTok, _, err := signer.IssueAccess(auth.Identity{UserID: "root", IsSuperAdmin: true})
	if err != nil {
		t.Fatalf("issue admin token: %v", err)
	}
	userTok, _, err := signer.IssueAccess(auth.Identity{UserID: "u1", TeamID: "team-1", Role: identity.RoleAdmin})
	if err != nil {
		t.Fatalf("issue user token: %v", err)
	}
	hs := httptest.NewServer(s.handler)
	t.Cleanup(hs.Close)
	return q, auditStore, hs, adminTok, userTok
}

func dlqDo(t *testing.T, hs *httptest.Server, method, path, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, hs.URL+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 512)
	buf := make([]byte, 512)
	for {
		n, rerr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, body
}

// waitForAudit polls the platform audit log for an action — the writes
// are detached (goSafe) so they land shortly after the response.
func waitForAudit(t *testing.T, st audit.Store, action string) audit.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs, err := st.ListPlatform(context.Background(), audit.Page{})
		if err != nil {
			t.Fatalf("list platform audit: %v", err)
		}
		for _, e := range evs {
			if e.Action == action {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no platform audit row for %q (got %d rows)", action, len(evs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A parked message is listed, peeked with its payload, replayed back
// onto the live queue and then gone from the DLQ — the operator's whole
// recovery loop, plus the audit row that records who did it.
func TestDLQAdmin_ListPeekReplayLeavesAuditTrail(t *testing.T) {
	q, auditStore, hs, adminTok, _ := newDLQAdminServer(t)
	q.park(11, "run-poison", "schema version 99 from a newer server")
	q.park(12, "run-other", "max deliver exhausted")

	code, body := dlqDo(t, hs, "GET", "/api/admin/dlq", adminTok)
	if code != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", code, body)
	}
	var list struct {
		Messages   []natsq.DLQMessage `json:"messages"`
		NextCursor uint64             `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list: %v (%s)", err, body)
	}
	if len(list.Messages) != 2 {
		t.Fatalf("expected both parked messages, got %d: %s", len(list.Messages), body)
	}
	if list.Messages[0].Seq != 11 || list.Messages[0].RunID != "run-poison" || list.Messages[0].Reason == "" {
		t.Fatalf("first parked message lost its identifying headers: %+v", list.Messages[0])
	}

	// Peek must hand back the verbatim RunMessage payload — that is what
	// the operator inspects before deciding to replay.
	code, body = dlqDo(t, hs, "GET", "/api/admin/dlq/11", adminTok)
	if code != http.StatusOK {
		t.Fatalf("peek: status=%d body=%s", code, body)
	}
	var peek struct {
		Message natsq.DLQMessage `json:"message"`
		Payload json.RawMessage  `json:"payload"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		t.Fatalf("decode peek: %v (%s)", err, body)
	}
	if peek.Message.Seq != 11 {
		t.Fatalf("peek returned seq %d, want 11", peek.Message.Seq)
	}
	var payload map[string]any
	if err := json.Unmarshal(peek.Payload, &payload); err != nil {
		t.Fatalf("peek payload is not the raw run message: %v (%s)", err, peek.Payload)
	}
	if payload["run_id"] != "run-poison" {
		t.Fatalf("peek payload = %v, want the parked run message", payload)
	}

	// Replay re-enqueues THAT message (not another one) and reports the run.
	code, body = dlqDo(t, hs, "POST", "/api/admin/dlq/11/replay", adminTok)
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%s", code, body)
	}
	var replay struct {
		Status string `json:"status"`
		RunID  string `json:"run_id"`
	}
	if err := json.Unmarshal(body, &replay); err != nil {
		t.Fatalf("decode replay: %v (%s)", err, body)
	}
	if replay.Status != "replayed" || replay.RunID != "run-poison" {
		t.Fatalf("replay = %+v, want the replayed run id", replay)
	}
	republished, _, remaining := q.snapshot()
	if len(republished) != 1 || republished[0] != "run-poison" {
		t.Fatalf("republished = %v, want exactly run-poison back on the live subject", republished)
	}
	if remaining != 1 {
		t.Fatalf("replayed message still parked (remaining=%d)", remaining)
	}

	// The operator's next list no longer shows it.
	code, body = dlqDo(t, hs, "GET", "/api/admin/dlq", adminTok)
	if code != http.StatusOK {
		t.Fatalf("list after replay: status=%d body=%s", code, body)
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Messages) != 1 || list.Messages[0].Seq != 12 {
		t.Fatalf("after replay the DLQ should hold only seq 12, got %s", body)
	}

	ev := waitForAudit(t, auditStore, "dlq.replayed")
	if ev.TargetID != "run-poison" {
		t.Errorf("audit row target = %q, want the replayed run id", ev.TargetID)
	}
	if ev.ActorID != "root" || ev.ActorKind != "super_admin" {
		t.Errorf("audit row actor = %q/%q, want the super-admin who replayed", ev.ActorID, ev.ActorKind)
	}
	if fmt.Sprintf("%v", ev.Meta["seq"]) != "11" {
		t.Errorf("audit meta seq = %v, want 11", ev.Meta["seq"])
	}
}

// Discarding drops the message for good (204) and is recorded.
func TestDLQAdmin_DiscardRemovesAndAudits(t *testing.T) {
	q, auditStore, hs, adminTok, _ := newDLQAdminServer(t)
	q.park(7, "run-dead", "unrecoverable")

	code, body := dlqDo(t, hs, "DELETE", "/api/admin/dlq/7", adminTok)
	if code != http.StatusNoContent {
		t.Fatalf("discard: status=%d body=%s", code, body)
	}
	republished, discarded, remaining := q.snapshot()
	if len(discarded) != 1 || discarded[0] != 7 {
		t.Fatalf("discarded = %v, want seq 7", discarded)
	}
	if len(republished) != 0 {
		t.Fatalf("discard must not re-enqueue anything, got %v", republished)
	}
	if remaining != 0 {
		t.Fatalf("message still parked after discard (remaining=%d)", remaining)
	}
	// Discarding an already-gone sequence is a 404, not a silent success.
	if code, _ := dlqDo(t, hs, "DELETE", "/api/admin/dlq/7", adminTok); code != http.StatusNotFound {
		t.Errorf("second discard: status=%d, want 404", code)
	}
	ev := waitForAudit(t, auditStore, "dlq.discarded")
	if fmt.Sprintf("%v", ev.Meta["seq"]) != "7" {
		t.Errorf("audit meta seq = %v, want 7", ev.Meta["seq"])
	}
}

// The DLQ is a platform surface: a team admin (any non-super-admin) is
// refused, and — the part that matters — the queue is never touched.
// An anonymous caller doesn't even reach the gate.
func TestDLQAdmin_NonSuperAdminCannotReplayOrDiscard(t *testing.T) {
	q, _, hs, _, userTok := newDLQAdminServer(t)
	q.park(3, "run-poison", "max deliver exhausted")

	cases := []struct {
		method, path string
		token        string
		want         int
	}{
		{"GET", "/api/admin/dlq", userTok, http.StatusForbidden},
		{"POST", "/api/admin/dlq/3/replay", userTok, http.StatusForbidden},
		{"DELETE", "/api/admin/dlq/3", userTok, http.StatusForbidden},
		{"POST", "/api/admin/dlq/3/replay", "", http.StatusUnauthorized},
		{"DELETE", "/api/admin/dlq/3", "", http.StatusUnauthorized},
		{"POST", "/api/admin/dlq/3/replay", "not-a-jwt", http.StatusUnauthorized},
	}
	for _, c := range cases {
		if code, body := dlqDo(t, hs, c.method, c.path, c.token); code != c.want {
			t.Errorf("%s %s (token=%q): status=%d want %d body=%s", c.method, c.path, c.token, code, c.want, body)
		}
	}
	republished, discarded, remaining := q.snapshot()
	if len(republished) != 0 || len(discarded) != 0 || remaining != 1 {
		t.Fatalf("an unprivileged caller reached the queue: republished=%v discarded=%v remaining=%d",
			republished, discarded, remaining)
	}
}

// A malformed or zero sequence is rejected before the queue is touched,
// and a replay the queue itself refuses surfaces as 502 (the message
// stays parked) rather than a false "replayed".
func TestDLQAdmin_BadSequenceAndBackendFailure(t *testing.T) {
	q, _, hs, adminTok, _ := newDLQAdminServer(t)
	q.park(5, "run-x", "boom")

	for _, path := range []string{"/api/admin/dlq/abc/replay", "/api/admin/dlq/0/replay"} {
		if code, body := dlqDo(t, hs, "POST", path, adminTok); code != http.StatusBadRequest {
			t.Errorf("POST %s: status=%d want 400 body=%s", path, code, body)
		}
	}
	if code, _ := dlqDo(t, hs, "GET", "/api/admin/dlq/abc", adminTok); code != http.StatusBadRequest {
		t.Errorf("peek with a bad seq: status=%d want 400", code)
	}
	if _, _, remaining := q.snapshot(); remaining != 1 {
		t.Fatalf("a rejected request touched the queue (remaining=%d)", remaining)
	}

	q.failReplay = fmt.Errorf("queue/nats: dlq replay publish: no responders")
	code, body := dlqDo(t, hs, "POST", "/api/admin/dlq/5/replay", adminTok)
	if code != http.StatusBadGateway {
		t.Fatalf("failed replay: status=%d want 502 body=%s", code, body)
	}
	if republished, _, remaining := q.snapshot(); len(republished) != 0 || remaining != 1 {
		t.Fatalf("a failed replay must leave the message parked: republished=%v remaining=%d", republished, remaining)
	}
}
