package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Measured 2026-09-08. A launch sent its parameter as `inputs`, a name
// launchRunRequest does not declare. The field was dropped, the 202 said
// nothing, the run took the workflow's own default for that parameter — the
// opposite of what was asked — and ten hours of a long run were spent before
// anyone read the value back out of the run instead of out of the payload
// they had kept. From the client, a parameter refused and a parameter
// swallowed were the same answer.
func TestLaunchRefusesAFieldItDoesNotDeclare(t *testing.T) {
	_, hs := newTestServer(t)
	body := `{"bot_id":"golden-master","inputs":{"adversarial":"false"}}`
	resp, err := http.Post(hs.URL+"/api/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a swallowed parameter is a run that does the opposite of what was asked", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "inputs") {
		t.Errorf("the refusal must name the field: %s", raw)
	}
	// `inputs` and `vars` are semantically related and textually unrelated,
	// so the accepted list is what points at the right one — a suggestion by
	// string distance would not have.
	if !strings.Contains(string(raw), "vars") {
		t.Errorf("the refusal must name what IS accepted: %s", raw)
	}
}

// The other half: every field the struct declares still launches. A guard
// that refused a legitimate parameter would be worse than the defect.
func TestLaunchStillAcceptsEveryDeclaredField(t *testing.T) {
	var req launchRunRequest
	raw, err := json.Marshal(map[string]any{
		"bot_id": "golden-master", "vars": map[string]string{"adversarial": "false"},
		"preset": "p", "timeout": "30m", "merge_into": "main", "branch_name": "b",
		"merge_strategy": "squash", "auto_merge": true, "run_id": "r1",
		"budget": map[string]any{"max_duration": "8h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/runs", bytes.NewReader(raw))
	if err := readJSONStrict(r, &req); err != nil {
		t.Fatalf("a body of declared fields was refused: %v", err)
	}
	if req.Vars["adversarial"] != "false" || req.BotID != "golden-master" {
		t.Fatalf("decoded shape = %+v", req)
	}
}
