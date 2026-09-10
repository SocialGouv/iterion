package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
)

// A launch or resume the cloud publisher refused for want of an LLM
// credential (Config.RequireLLMCredential, #841) is not a malformed request
// and not an outage: the same request succeeds once a credential is
// provisioned. Both surfaces answer 422 with a stable code and the
// publisher's own sentence, which names the providers to provision.

func assertNoLLMCredentialResponse(t *testing.T, code int, body []byte) {
	t.Helper()
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body=%s", code, http.StatusUnprocessableEntity, body)
	}
	var resp struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	if resp.ErrorCode != runview.NoLLMCredentialErrorCode {
		t.Errorf("error_code = %q, want %q", resp.ErrorCode, runview.NoLLMCredentialErrorCode)
	}
	if !errors.Is(runview.ErrNoLLMCredential, runview.ErrNoLLMCredential) || resp.Error == "" || !bytes.Contains([]byte(resp.Error), []byte("anthropic")) {
		t.Errorf("error = %q, want the publisher's sentence naming the provider", resp.Error)
	}
}

func TestLaunchRefusedForWantOfCredentialIs422(t *testing.T) {
	srv := newQueueOutageHTTPTestServer(t)
	installQueueOutageTestPublisher(t, srv, fmt.Errorf("cloudpublisher: %w: every route pins anthropic and no tier holds a credential for it", runview.ErrNoLLMCredential))
	body, err := json.Marshal(map[string]any{
		"file_path": "nocred.bot",
		"source":    "workflow nocred:\n  entry: done\n",
		"run_id":    "run-nocred-launch",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleLaunchRun(rec, req)
	assertNoLLMCredentialResponse(t, rec.Code, rec.Body.Bytes())
}

func TestWriteResumeError_NoLLMCredentialIs422(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/r1/resume", nil)
	(&Server{}).writeResumeError(rec, req, fmt.Errorf("cloudpublisher: republish: %w: every route pins anthropic", runview.ErrNoLLMCredential))
	assertNoLLMCredentialResponse(t, rec.Code, rec.Body.Bytes())
}
