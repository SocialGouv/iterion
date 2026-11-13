package tools

import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/claw/internal/runtime/worker"
	"strings"
	"testing"
)

func TestWorkerCreate(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	input := map[string]any{
		"cwd": "/tmp/work",
	}
	result, err := ExecuteWorkerCreate(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["worker_id"] == nil || parsed["worker_id"] == "" {
		t.Error("expected non-empty worker_id")
	}
	if parsed["cwd"] != "/tmp/work" {
		t.Errorf("expected cwd '/tmp/work', got %v", parsed["cwd"])
	}
	if parsed["status"] != "spawning" {
		t.Errorf("expected status 'spawning', got %v", parsed["status"])
	}
}

func TestWorkerCreate_WithOptions(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	input := map[string]any{
		"cwd":                             "/tmp/work",
		"trusted_roots":                   []any{"/trusted"},
		"auto_recover_prompt_misdelivery": false,
	}
	result, err := ExecuteWorkerCreate(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["worker_id"] == nil || parsed["worker_id"] == "" {
		t.Error("expected non-empty worker_id")
	}
}

func TestWorkerCreate_MissingCwd(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	_, err := ExecuteWorkerCreate(map[string]any{}, reg)
	if err == nil {
		t.Fatal("expected error for missing cwd")
	}
	if !strings.Contains(err.Error(), "cwd") {
		t.Errorf("expected error about cwd, got: %v", err)
	}
}

func TestWorkerCreate_NilRegistry(t *testing.T) {
	_, err := ExecuteWorkerCreate(map[string]any{"cwd": "/tmp"}, nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("expected 'not available' error, got: %v", err)
	}
}

func TestWorkerGet(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	created := reg.Create("/tmp", nil, true)

	input := map[string]any{"worker_id": created.WorkerID}
	result, err := ExecuteWorkerGet(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["worker_id"] != created.WorkerID {
		t.Errorf("expected worker_id %q, got %v", created.WorkerID, parsed["worker_id"])
	}
}

func TestWorkerGet_NotFound(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	_, err := ExecuteWorkerGet(map[string]any{"worker_id": "nonexistent"}, reg)
	if err == nil {
		t.Fatal("expected error for nonexistent worker")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

func TestWorkerGet_NilRegistry(t *testing.T) {
	_, err := ExecuteWorkerGet(map[string]any{"worker_id": "w1"}, nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
}

func TestWorkerObserve(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	created := reg.Create("/tmp", nil, true)

	input := map[string]any{
		"worker_id":   created.WorkerID,
		"screen_text": "some output text",
	}
	result, err := ExecuteWorkerObserve(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestWorkerObserve_NotFound(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	_, err := ExecuteWorkerObserve(map[string]any{
		"worker_id":   "nonexistent",
		"screen_text": "text",
	}, reg)
	if err == nil {
		t.Fatal("expected error for nonexistent worker")
	}
}

func TestWorkerObserve_NilRegistry(t *testing.T) {
	_, err := ExecuteWorkerObserve(map[string]any{
		"worker_id":   "w1",
		"screen_text": "text",
	}, nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
}

func TestWorkerResolveTrust_NilRegistry(t *testing.T) {
	_, err := ExecuteWorkerResolveTrust(map[string]any{"worker_id": "w1"}, nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
}

func TestWorkerResolveTrust_InvalidState(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	created := reg.Create("/tmp", nil, true)
	// Worker is in spawning state, not trust_required.
	_, err := ExecuteWorkerResolveTrust(map[string]any{"worker_id": created.WorkerID}, reg)
	if err == nil {
		t.Fatal("expected error for invalid state")
	}
	if !strings.Contains(err.Error(), "invalid worker state") {
		t.Errorf("expected invalid state error, got: %v", err)
	}
}

func TestWorkerAwaitReady(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	created := reg.Create("/tmp", nil, true)

	result, err := ExecuteWorkerAwaitReady(map[string]any{"worker_id": created.WorkerID}, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["ready"] != false {
		t.Errorf("expected ready=false for spawning worker, got %v", parsed["ready"])
	}
}

func TestWorkerAwaitReady_NilRegistry(t *testing.T) {
	_, err := ExecuteWorkerAwaitReady(map[string]any{"worker_id": "w1"}, nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
}

func TestWorkerSendPrompt_NilRegistry(t *testing.T) {
	_, err := ExecuteWorkerSendPrompt(map[string]any{"worker_id": "w1"}, nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
}

func TestWorkerSendPrompt_InvalidState(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	created := reg.Create("/tmp", nil, true)
	prompt := "hello"
	_, err := ExecuteWorkerSendPrompt(map[string]any{
		"worker_id": created.WorkerID,
		"prompt":    prompt,
	}, reg)
	if err == nil {
		t.Fatal("expected error for invalid state (spawning)")
	}
}

func TestWorkerRestart(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	created := reg.Create("/tmp", nil, true)

	result, err := ExecuteWorkerRestart(map[string]any{"worker_id": created.WorkerID}, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["status"] != "spawning" {
		t.Errorf("expected status 'spawning' after restart, got %v", parsed["status"])
	}
}

func TestWorkerRestart_NilRegistry(t *testing.T) {
	_, err := ExecuteWorkerRestart(map[string]any{"worker_id": "w1"}, nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
}

func TestWorkerTerminate(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	created := reg.Create("/tmp", nil, true)

	result, err := ExecuteWorkerTerminate(map[string]any{"worker_id": created.WorkerID}, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["status"] != "finished" {
		t.Errorf("expected status 'finished' after terminate, got %v", parsed["status"])
	}
}

func TestWorkerTerminate_NilRegistry(t *testing.T) {
	_, err := ExecuteWorkerTerminate(map[string]any{"worker_id": "w1"}, nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
}

func TestWorkerObserveCompletion(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	created := reg.Create("/tmp", nil, true)

	result, err := ExecuteWorkerObserveCompletion(map[string]any{
		"worker_id":     created.WorkerID,
		"finish_reason": "stop",
		"tokens_output": float64(100),
	}, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["status"] != "finished" {
		t.Errorf("expected status 'finished', got %v", parsed["status"])
	}
}

func TestWorkerObserveCompletion_ProviderFailure(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	created := reg.Create("/tmp", nil, true)

	result, err := ExecuteWorkerObserveCompletion(map[string]any{
		"worker_id":     created.WorkerID,
		"finish_reason": "error",
		"tokens_output": float64(0),
	}, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["status"] != "failed" {
		t.Errorf("expected status 'failed' for provider failure, got %v", parsed["status"])
	}
}

func TestWorkerObserveCompletion_NilRegistry(t *testing.T) {
	_, err := ExecuteWorkerObserveCompletion(map[string]any{
		"worker_id":     "w1",
		"finish_reason": "stop",
		"tokens_output": float64(10),
	}, nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
}

func TestWorkerObserveCompletion_MissingFields(t *testing.T) {
	reg := worker.NewWorkerRegistry()
	_, err := ExecuteWorkerObserveCompletion(map[string]any{
		"worker_id": "w1",
	}, reg)
	if err == nil {
		t.Fatal("expected error for missing finish_reason")
	}
}

func TestWorkerFullLifecycle(t *testing.T) {
	reg := worker.NewWorkerRegistry()

	// Create.
	createResult, err := ExecuteWorkerCreate(map[string]any{"cwd": "/tmp"}, reg)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created map[string]any
	json.Unmarshal([]byte(createResult), &created)
	wid := created["worker_id"].(string)

	// Get.
	_, err = ExecuteWorkerGet(map[string]any{"worker_id": wid}, reg)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Observe ready prompt.
	_, err = ExecuteWorkerObserve(map[string]any{
		"worker_id":   wid,
		"screen_text": "╭─────────────────────────────────────╮\n│ > ready for input",
	}, reg)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}

	// Await ready.
	readyResult, err := ExecuteWorkerAwaitReady(map[string]any{"worker_id": wid}, reg)
	if err != nil {
		t.Fatalf("await_ready: %v", err)
	}
	var snap map[string]any
	json.Unmarshal([]byte(readyResult), &snap)
	if snap["ready"] != true {
		t.Errorf("expected ready=true, got %v", snap["ready"])
	}

	// Send prompt.
	_, err = ExecuteWorkerSendPrompt(map[string]any{
		"worker_id": wid,
		"prompt":    "do something",
	}, reg)
	if err != nil {
		t.Fatalf("send_prompt: %v", err)
	}

	// Terminate.
	termResult, err := ExecuteWorkerTerminate(map[string]any{"worker_id": wid}, reg)
	if err != nil {
		t.Fatalf("terminate: %v", err)
	}
	var terminated map[string]any
	json.Unmarshal([]byte(termResult), &terminated)
	if terminated["status"] != "finished" {
		t.Errorf("expected finished, got %v", terminated["status"])
	}
}
