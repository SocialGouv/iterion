package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

type queueOutageTestPublisher struct {
	err error
}

func (p *queueOutageTestPublisher) SubmitLaunch(context.Context, string, runview.LaunchSpec, *ir.Workflow, string) (int, error) {
	return 0, p.err
}

func (*queueOutageTestPublisher) CancelRun(context.Context, string) error { return nil }

func (*queueOutageTestPublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}

func (p *queueOutageTestPublisher) SubmitResume(context.Context, runview.ResumeSpec, *ir.Workflow, string) error {
	return p.err
}

func installQueueOutageTestPublisher(t *testing.T, srv *Server, publishErr error) {
	t.Helper()
	svc, err := runview.NewService(srv.cfg.StoreDir,
		runview.WithLogger(iterlog.Nop()),
		runview.WithLaunchPublisher(&queueOutageTestPublisher{err: publishErr}),
	)
	if err != nil {
		t.Fatalf("NewService with queue outage publisher: %v", err)
	}
	srv.runs = svc
}

func newQueueOutageHTTPTestServer(t *testing.T) *Server {
	t.Helper()
	workDir := t.TempDir()
	srv := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		SkipProjectRegistration: true,
	}, iterlog.Nop())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

func assertQueueUnavailableResponse(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	var body struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
		Retryable bool   `json:"retryable"`
	}
	decodeJSONResp(t, resp, &body)
	if body.ErrorCode != runview.QueueUnavailableErrorCode {
		t.Errorf("error_code = %q, want %q", body.ErrorCode, runview.QueueUnavailableErrorCode)
	}
	if !body.Retryable {
		t.Error("retryable = false, want true")
	}
	if body.Error == "" {
		t.Error("human-readable error should remain populated")
	}
}

func TestWriteResumeError_SourceChangedCarriesStableCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "typed runtime error",
			err:  fmt.Errorf("resume worker: %w", runtime.ErrWorkflowSourceChanged),
		},
		{
			name: "legacy detached runner text",
			err:  errors.New("runtime: workflow source has changed since run r1 was started"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/runs/r1/resume", nil)

			(&Server{}).writeResumeError(rec, req, tt.err)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			var body struct {
				Error     string `json:"error"`
				ErrorCode string `json:"error_code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response %q: %v", rec.Body.String(), err)
			}
			if body.ErrorCode != workflowSourceChangedErrorCode {
				t.Errorf("error_code = %q, want %q", body.ErrorCode, workflowSourceChangedErrorCode)
			}
			if body.Error == "" {
				t.Error("human-readable error should remain populated")
			}
		})
	}
}

func TestWriteResumeError_UnrelatedFailureHasNoSourceChangedCode(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/r1/resume", nil)

	(&Server{}).writeResumeError(rec, req, errors.New("invalid answers"))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	if _, ok := body["error_code"]; ok {
		t.Errorf("unrelated resume error unexpectedly carried error_code: %#v", body)
	}
}

func TestWriteResumeError_QueueUnavailableIsRetryableServiceUnavailable(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/r1/resume", nil)

	(&Server{}).writeResumeError(rec, req, &runview.QueueUnavailableError{Cause: errors.New("broker unavailable")})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	if body.ErrorCode != runview.QueueUnavailableErrorCode {
		t.Errorf("error_code = %q, want %q", body.ErrorCode, runview.QueueUnavailableErrorCode)
	}
	if !body.Retryable {
		t.Error("retryable = false, want true")
	}
}

func TestQueueUnavailableLaunchAndResumeReturnRetryableServiceUnavailable(t *testing.T) {
	const source = "workflow queue_outage:\n  entry: done\n"

	t.Run("launch", func(t *testing.T) {
		srv := newQueueOutageHTTPTestServer(t)
		queueErr := &runview.QueueUnavailableError{Cause: errors.New("broker recovering")}
		// SubmitLaunch wraps the queue error in production, and may join it
		// with a failed status rollback. Keep that outer shape in the proof.
		installQueueOutageTestPublisher(t, srv, errors.Join(
			fmt.Errorf("cloudpublisher: publish: %w", queueErr),
			errors.New("status rollback also failed"),
		))
		body, err := json.Marshal(map[string]any{
			"file_path": "queue_outage.bot",
			"source":    source,
			"run_id":    "run-queue-outage-launch",
		})
		if err != nil {
			t.Fatalf("marshal launch request: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleLaunchRun(rec, req)
		assertQueueUnavailableResponse(t, rec.Result())
	})

	t.Run("resume", func(t *testing.T) {
		srv := newQueueOutageHTTPTestServer(t)
		queueErr := &runview.QueueUnavailableError{Cause: errors.New("broker recovering")}
		installQueueOutageTestPublisher(t, srv, fmt.Errorf("cloudpublisher: republish: %w", queueErr))

		logicalPath := filepath.Join(srv.cfg.WorkDir, "queue_outage.bot")
		_, workflowHash, err := runview.CompileWorkflowFromSource(logicalPath, source)
		if err != nil {
			t.Fatalf("compile workflow: %v", err)
		}
		st, err := store.New(srv.cfg.StoreDir)
		if err != nil {
			t.Fatalf("open run store: %v", err)
		}
		const runID = "run-queue-outage-resume"
		if _, err := st.CreateRun(context.Background(), runID, "queue_outage", nil); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		r, err := st.LoadRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		r.FilePath = logicalPath
		r.WorkflowSource = source
		r.WorkflowHash = workflowHash
		r.Status = store.RunStatusPausedOperator
		if err := st.SaveRun(context.Background(), r); err != nil {
			t.Fatalf("SaveRun: %v", err)
		}
		if err := st.SaveCheckpoint(context.Background(), runID, &store.Checkpoint{NodeID: "done"}); err != nil {
			t.Fatalf("SaveCheckpoint: %v", err)
		}

		body, err := json.Marshal(map[string]any{
			"file_path": logicalPath,
			"source":    source,
		})
		if err != nil {
			t.Fatalf("marshal resume request: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/resume", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", runID)
		rec := httptest.NewRecorder()
		srv.handleResumeRun(rec, req)
		assertQueueUnavailableResponse(t, rec.Result())
	})
}

func TestResumeRun_SourceChangedReturnsStableCodeBeforeAsyncLaunch(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	srv, httpServer := newTestServer(t)

	const originalSource = `
schema gate_out:
  approved: bool

prompt gate_prompt:
  Approve the original workflow?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

workflow resume_hash:
  entry: gate
  gate -> done when approved
  gate -> fail when not approved
`
	const modifiedSource = `
schema gate_out:
  approved: bool

prompt gate_prompt:
  Approve the modified workflow?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

workflow resume_hash:
  entry: gate
  gate -> done when approved
  gate -> fail when not approved
`

	botPath := filepath.Join(srv.cfg.WorkDir, "resume_hash.bot")
	if err := os.WriteFile(botPath, []byte(modifiedSource), 0o644); err != nil {
		t.Fatalf("write modified workflow: %v", err)
	}
	_, originalHash, err := runview.CompileWorkflowFromSource(botPath, originalSource)
	if err != nil {
		t.Fatalf("compile original workflow: %v", err)
	}
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open run store: %v", err)
	}
	const runID = "run-source-changed-http"
	if _, err := st.CreateRun(context.Background(), runID, "resume_hash", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	run.Status = store.RunStatusFailedResumable
	run.FilePath = botPath
	run.WorkflowHash = originalHash
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	postResume := func(body string) *http.Response {
		t.Helper()
		resp, err := http.Post(
			httpServer.URL+"/api/runs/"+runID+"/resume",
			"application/json",
			bytes.NewBufferString(body),
		)
		if err != nil {
			t.Fatalf("POST resume: %v", err)
		}
		return resp
	}

	resp := postResume(`{}`)
	if resp.StatusCode != http.StatusBadRequest {
		defer resp.Body.Close()
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	var refusal struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	decodeJSONResp(t, resp, &refusal)
	if refusal.ErrorCode != workflowSourceChangedErrorCode {
		t.Fatalf("error_code = %q, want %q", refusal.ErrorCode, workflowSourceChangedErrorCode)
	}
	if refusal.Error == "" {
		t.Fatal("human-readable error should remain populated")
	}

	// The synchronous refusal leaves the run resumable, and the explicit
	// force retry crosses the same handler/service boundary successfully.
	resp = postResume(`{"force":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("forced status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
}

func TestResumeRun_DispatcherChildOutsideWorkDirUsesPersistedSource(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	srv, httpServer := newTestServer(t)

	const source = `
schema gate_out:
  approved: bool

human gate:
  output: gate_out
  interaction: human

workflow dispatcher_child:
  entry: gate
  gate -> done when approved
  gate -> fail when not approved
`

	const runID = "run-dispatcher-child-outside-workdir"
	// Dispatcher child bots execute from issue worktrees under the managed
	// store, not beneath the Studio's WorkDir. The pipeline board omits source
	// when it answers their human gates, so resume must use the launch snapshot
	// already persisted on the child run.
	outsidePath := filepath.Join(t.TempDir(), "dispatcher", "worktree", "child.bot")
	if _, err := srv.safePath(outsidePath); err == nil {
		t.Fatalf("test setup invalid: %q should escape WorkDir", outsidePath)
	}

	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open run store: %v", err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, runID, "dispatcher_child", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	_, workflowHash, err := runview.CompileWorkflowFromSource(outsidePath, source)
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	run.FilePath = outsidePath
	run.WorkflowHash = workflowHash
	run.WorkflowSource = source
	run.ParentRunID = "parent-run"
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := st.PauseRun(ctx, runID, &store.Checkpoint{
		NodeID:           "gate",
		Outputs:          map[string]map[string]any{},
		LoopCounters:     map[string]int{},
		ArtifactVersions: map[string]int{},
		Vars:             map[string]any{},
	}); err != nil {
		t.Fatalf("PauseRun: %v", err)
	}

	resp, err := http.Post(
		httpServer.URL+"/api/runs/"+runID+"/resume",
		"application/json",
		bytes.NewBufferString(`{"answers":{"approved":true}}`),
	)
	if err != nil {
		t.Fatalf("POST resume: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		var refusal map[string]any
		decodeJSONResp(t, resp, &refusal)
		t.Fatalf("status = %d, want %d; body = %#v", resp.StatusCode, http.StatusAccepted, refusal)
	}
}
