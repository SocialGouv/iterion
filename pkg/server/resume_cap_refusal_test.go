package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
)

// capRefusal is the error SubmitResume returns when a key the resumed run
// sealed is full of other runs, wrapped as the publisher wraps it.
func capRefusal(runID string) error {
	return fmt.Errorf("cloudpublisher: resume of run %s refused before publishing, the run put back as it was: %w: api-key(anthropic) \"capped\" holds 1/1 alive runs", runID, cloudpublisher.ErrCredentialAtCapacity)
}

// seedCapRefusalRun persists a run whose resume compiles, in status, so the
// resume reaches the publisher.
func seedCapRefusalRun(t *testing.T, srv *Server, runID string, status store.RunStatus) {
	t.Helper()
	const source = "workflow cap_refusal:\n  entry: done\n"
	logicalPath := filepath.Join(srv.cfg.WorkDir, "cap_refusal.bot")
	if err := os.WriteFile(logicalPath, []byte(source), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	_, workflowHash, err := runview.CompileWorkflowFromSource(logicalPath, source, "")
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open run store: %v", err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, runID, "cap_refusal", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.FilePath = logicalPath
	r.WorkflowSource = source
	r.WorkflowHash = workflowHash
	r.Status = status
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := st.SaveCheckpoint(ctx, runID, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
}

func assertCredentialAtCapacityResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d (%s), want 429 — a resume refused on a full key is a retry-later, not a server failure", rec.Code, rec.Body.String())
	}
	var body struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	if body.ErrorCode != credentialAtCapacityErrorCode {
		t.Fatalf("error_code = %q, want %q", body.ErrorCode, credentialAtCapacityErrorCode)
	}
	if !strings.Contains(body.Error, cloudpublisher.ErrCredentialAtCapacity.Error()) {
		t.Fatalf("error = %q, want the refusal's own words", body.Error)
	}
}

// Every operator surface that resumes a run answers a refusal on a full key
// the way the documentation promises — 429 with a stable code — not only
// POST /resume: answering a human gate and delivering a host event resume
// the run too, and a 500 there tells the caller nothing a retry would fix.
// Red when a surface's error mapping has no case for the refusal.
func TestCredentialAtCapacity_EveryOperatorResumeAnswers429(t *testing.T) {
	t.Run("resume", func(t *testing.T) {
		srv := newQueueOutageHTTPTestServer(t)
		const runID = "run-cap-resume"
		installQueueOutageTestPublisher(t, srv, capRefusal(runID))
		seedCapRefusalRun(t, srv, runID, store.RunStatusPausedOperator)
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/resume", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", runID)
		rec := httptest.NewRecorder()
		srv.handleResumeRun(rec, req)
		assertCredentialAtCapacityResponse(t, rec)
	})

	t.Run("answer-human", func(t *testing.T) {
		srv := newQueueOutageHTTPTestServer(t)
		const runID = "run-cap-answer"
		installQueueOutageTestPublisher(t, srv, capRefusal(runID))
		seedCapRefusalRun(t, srv, runID, store.RunStatusPausedWaitingHuman)
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/answer-human", bytes.NewReader([]byte(`{"answers":{"q":"a"}}`)))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", runID)
		rec := httptest.NewRecorder()
		srv.handleAnswerHuman(rec, req)
		assertCredentialAtCapacityResponse(t, rec)
	})

	t.Run("host-event", func(t *testing.T) {
		f := newArmFixture(t, true)
		assistant := f.assistant(t, "run-cap-assistant", "native:card")
		// The run's own source, compiled by the resume before the publisher
		// is asked.
		botFile := filepath.Join(t.TempDir(), "chatbot.bot")
		if err := os.WriteFile(botFile, []byte("workflow chatbot:\n  entry: done\n"), 0o644); err != nil {
			t.Fatalf("write workflow: %v", err)
		}
		assistant.FilePath = botFile
		if err := f.rs.SaveRun(context.Background(), assistant); err != nil {
			t.Fatalf("SaveRun: %v", err)
		}
		f.srv.runs = newTestRunviewService(t, "", runview.WithStore(f.rs),
			runview.WithLaunchPublisher(&queueOutageTestPublisher{err: capRefusal(assistant.ID)}))
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+assistant.ID+"/host-event",
			strings.NewReader(`{"kind":"action-completed","event":{"action":"run.rewind"}}`))
		req.SetPathValue("id", assistant.ID)
		rec := httptest.NewRecorder()
		f.srv.handleDeliverHostEvent(rec, req)
		assertCredentialAtCapacityResponse(t, rec)
	})

	t.Run("ws answer", func(t *testing.T) {
		srv := newQueueOutageHTTPTestServer(t)
		const runID = "run-cap-ws"
		installQueueOutageTestPublisher(t, srv, capRefusal(runID))
		seedCapRefusalRun(t, srv, runID, store.RunStatusPausedWaitingHuman)
		c := &runConn{server: srv, runID: runID, sendCh: make(chan []byte, 8), closed: make(chan struct{})}
		payload, err := json.Marshal(map[string]any{"answers": map[string]any{"q": "a"}})
		if err != nil {
			t.Fatalf("marshal answer: %v", err)
		}
		c.handleAnswer(runWSEnvelope{Type: wsTypeAnswer, AckID: "cap", Payload: payload})
		got := firstWSError(t, c)
		if got.Code != credentialAtCapacityErrorCode {
			t.Fatalf("ws error code = %q (%s), want %q", got.Code, got.Message, credentialAtCapacityErrorCode)
		}
	})
}
