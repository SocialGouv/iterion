package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestWriteResumeError_aRefusalAnswersTheRemedyItNames: every resume refusal
// that names its remedy answers it — a lone child's, an artifact contract's,
// not only the scratch's — read from the error; an error that names none
// answers no hint.
func TestWriteResumeError_aRefusalAnswersTheRemedyItNames(t *testing.T) {
	lineage := &runtime.RuntimeError{Code: runtime.ErrCodeResumeInvalid,
		Message: "run c1 executed in its parent run p1's copy-based sandbox",
		Hint:    "cancel this child and resume the parent; or resume this child with --force"}
	contract := &runtime.RuntimeError{Code: runtime.ErrCodeResumeInvalid,
		Message: "persisted artifact contract is incompatible with this workflow",
		Hint:    "resume with --force to accept a deliberate publish, schema or workflow-revision edit",
		Cause:   fmt.Errorf("node n: %w", runtime.ErrArtifactContractIncompatible)}
	for _, tc := range []struct {
		name     string
		err      error
		code     string
		wantHint string
	}{
		{"a lone child of a copy-based sandbox", fmt.Errorf("resume: %w", lineage), "", lineage.Hint},
		{"an incompatible artifact contract", contract, artifactContractIncompatibleErrorCode, contract.Hint},
		{"an error naming no remedy", errors.New("invalid answers"), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			(&Server{}).writeResumeError(rec, httptest.NewRequest(http.MethodPost, "/api/runs/r1/resume", nil), tc.err)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
			if body["error"] == "" || body["error"] == nil {
				t.Errorf("body %s lost its human-readable error", rec.Body.String())
			}
			if code, _ := body["error_code"].(string); code != tc.code {
				t.Errorf("error_code = %q, want %q", code, tc.code)
			}
			hint, present := body["hint"]
			if tc.wantHint == "" {
				if present {
					t.Errorf("an error naming no remedy answered hint %v", hint)
				}
				return
			}
			if hint != tc.wantHint {
				t.Errorf("hint = %v, want the refusal's own %q", hint, tc.wantHint)
			}
		})
	}
}

// TestRunsWS_anAnswerRefusedOverTheScratchNamesItsConsent: the studio's
// answer to a paused run whose scratch did not travel is refused on the
// socket with the consent that clears the refusal, not the error's text
// alone.
func TestRunsWS_anAnswerRefusedOverTheScratchNamesItsConsent(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	srv, hs := newTestServer(t)
	const source = `
schema gate_out:
  approved: bool

prompt gate_prompt:
  Approve?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

workflow scratch_ws:
  entry: gate
  gate -> done when approved
  gate -> fail when not approved
`
	botPath := filepath.Join(srv.cfg.WorkDir, "scratch_ws.bot")
	if err := os.WriteFile(botPath, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	_, hash, err := runview.CompileWorkflowFromSource(botPath, source)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const runID = "run-scratch-ws-answer"
	if _, err := st.CreateRun(ctx, runID, "scratch_ws", nil); err != nil {
		t.Fatal(err)
	}
	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	run.FilePath = botPath
	run.WorkflowHash = hash
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := st.PauseRun(ctx, runID, &store.Checkpoint{NodeID: "gate", Outputs: map[string]map[string]any{},
		LoopCounters: map[string]int{}, ArtifactVersions: map[string]int{}, Vars: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{
		"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap",
	}}); err != nil {
		t.Fatal(err)
	}
	c := dialRunWS(t, hs, runID)
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeAnswer, AckID: "a1", Payload: json.RawMessage(`{"answers":{"approved":true}}`)})
	env := readEnvelope(t, c, wsTypeError)
	var p wsErrorPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Code != "resume_failed" || !strings.Contains(p.Message, "SCRATCH_NOT_PORTABLE") || !strings.Contains(p.Message, "--accept-scratch-loss") {
		t.Fatalf("socket refusal = %+v, want resume_failed naming the scratch's consent", p)
	}
}
