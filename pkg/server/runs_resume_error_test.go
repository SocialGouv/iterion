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

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

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

	botPath := filepath.Join(srv.cfg.WorkDir, "dispatcher_child.bot")
	if err := os.WriteFile(botPath, []byte(source), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	const runID = "run-dispatcher-child-outside-workdir"
	launched, err := srv.runs.Launch(context.Background(), runview.LaunchSpec{
		RunID:       runID,
		FilePath:    botPath,
		ParentRunID: "parent-run",
	})
	if err != nil {
		t.Fatalf("launch child: %v", err)
	}
	select {
	case <-launched.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not reach its human gate")
	}

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
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("status = %q, want %q", run.Status, store.RunStatusPausedWaitingHuman)
	}
	if run.WorkflowSource == "" {
		t.Fatal("launch did not persist WorkflowSource")
	}
	run.FilePath = outsidePath
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun: %v", err)
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
