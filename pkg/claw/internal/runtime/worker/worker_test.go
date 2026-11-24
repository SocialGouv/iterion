package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCreateWorkerSpawningStatus(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/home/test", nil, false)
	if w.Status != StatusSpawning {
		t.Errorf("expected Spawning, got %s", w.Status)
	}
	if len(w.Events) == 0 {
		t.Fatal("expected spawning event")
	}
	if w.Events[0].Kind != EventSpawning {
		t.Errorf("expected EventSpawning, got %s", w.Events[0].Kind)
	}
	if w.WorkerID == "" {
		t.Error("expected non-empty worker ID")
	}
}

func TestCreateWorkerTrustAutoResolve(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/home/test/project", []string{"/home/test"}, false)
	if !w.TrustAutoResolve {
		t.Error("expected TrustAutoResolve=true when cwd is under trusted root")
	}
}

func TestCreateWorkerNoTrustAutoResolve(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/other/dir", []string{"/home/test"}, false)
	if w.TrustAutoResolve {
		t.Error("expected TrustAutoResolve=false when cwd is not under trusted root")
	}
}

func TestObserveTrustPrompt(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	w2, err := r.Observe(w.WorkerID, "Do you trust the files in this folder?")
	if err != nil {
		t.Fatalf("Observe failed: %v", err)
	}
	if w2.Status != StatusTrustRequired {
		t.Errorf("expected TrustRequired, got %s", w2.Status)
	}
}

func TestObserveTrustPromptAutoResolve(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/home/test/project", []string{"/home/test"}, false)

	w2, err := r.Observe(w.WorkerID, "Do you trust the files in this folder?")
	if err != nil {
		t.Fatalf("Observe failed: %v", err)
	}
	if !w2.TrustGateCleared {
		t.Error("expected TrustGateCleared after auto-resolve")
	}
	// Check for TrustResolved event
	found := false
	for _, ev := range w2.Events {
		if ev.Kind == EventTrustResolved {
			found = true
			if ev.Payload == nil || ev.Payload.Resolution == nil {
				t.Error("TrustResolved event should have resolution payload")
			} else if *ev.Payload.Resolution != TrustAutoAllowlisted {
				t.Errorf("expected AutoAllowlisted resolution, got %s", *ev.Payload.Resolution)
			}
		}
	}
	if !found {
		t.Error("expected TrustResolved event after auto-resolve")
	}
}

func TestResolveTrustManual(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	r.Observe(w.WorkerID, "Do you trust the files in this folder?")

	w2, err := r.ResolveTrust(w.WorkerID)
	if err != nil {
		t.Fatalf("ResolveTrust failed: %v", err)
	}
	if !w2.TrustGateCleared {
		t.Error("expected TrustGateCleared after manual resolve")
	}
	found := false
	for _, ev := range w2.Events {
		if ev.Kind == EventTrustResolved {
			if ev.Payload != nil && ev.Payload.Resolution != nil && *ev.Payload.Resolution == TrustManualApproval {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected TrustResolved event with ManualApproval")
	}
}

func TestResolveTrustWrongState(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	_, err := r.ResolveTrust(w.WorkerID)
	if err == nil {
		t.Error("expected error when resolving trust in non-TrustRequired state")
	}
}

func TestSendPrompt(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	r.Observe(w.WorkerID, "ready for input")

	prompt := "Write a test"
	w2, err := r.SendPrompt(w.WorkerID, &prompt)
	if err != nil {
		t.Fatalf("SendPrompt failed: %v", err)
	}
	if w2.Status != StatusRunning {
		t.Errorf("expected Running, got %s", w2.Status)
	}
	if !w2.PromptInFlight {
		t.Error("expected PromptInFlight=true")
	}
	if w2.LastPrompt == nil || *w2.LastPrompt != prompt {
		t.Error("expected LastPrompt to be set")
	}
	if w2.PromptDeliveryAttempts != 1 {
		t.Errorf("expected 1 delivery attempt, got %d", w2.PromptDeliveryAttempts)
	}
}

func TestSendPromptReplay(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, true) // auto-recover enabled

	r.Observe(w.WorkerID, "ready for input")

	prompt := "Write code"
	r.SendPrompt(w.WorkerID, &prompt)

	// Simulate prompt misdelivery (shell)
	r.Observe(w.WorkerID, "Write code\ncommand not found")

	w2 := r.Get(w.WorkerID)
	if w2.ReplayPrompt == nil {
		t.Fatal("expected ReplayPrompt set after auto-recovery")
	}

	w3, err := r.SendPrompt(w.WorkerID, nil)
	if err != nil {
		t.Fatalf("SendPrompt replay failed: %v", err)
	}
	if w3.Status != StatusRunning {
		t.Errorf("expected Running, got %s", w3.Status)
	}
	if w3.LastPrompt == nil || *w3.LastPrompt != prompt {
		t.Errorf("expected replay of original prompt")
	}
}

func TestSendPromptWrongState(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	prompt := "test"
	_, err := r.SendPrompt(w.WorkerID, &prompt)
	if err == nil {
		t.Error("expected error when sending prompt in Spawning state")
	}
}

func TestMarkRunningAndFinished(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	r.Observe(w.WorkerID, "ready for input")
	prompt := "test"
	r.SendPrompt(w.WorkerID, &prompt)

	w2, err := r.Observe(w.WorkerID, "thinking...")
	if err != nil {
		t.Fatalf("Observe failed: %v", err)
	}
	if w2.Status != StatusRunning {
		t.Errorf("expected Running, got %s", w2.Status)
	}

	w3, err := r.Terminate(w.WorkerID)
	if err != nil {
		t.Fatalf("Terminate failed: %v", err)
	}
	if w3.Status != StatusFinished {
		t.Errorf("expected Finished, got %s", w3.Status)
	}
}

func TestObserveCompletionSuccess(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	w2, err := r.ObserveCompletion(w.WorkerID, "end_turn", 100)
	if err != nil {
		t.Fatalf("ObserveCompletion failed: %v", err)
	}
	if w2.Status != StatusFinished {
		t.Errorf("expected Finished, got %s", w2.Status)
	}
}

func TestObserveCompletionProviderFailure(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	w2, err := r.ObserveCompletion(w.WorkerID, "unknown", 0)
	if err != nil {
		t.Fatalf("ObserveCompletion failed: %v", err)
	}
	if w2.Status != StatusFailed {
		t.Errorf("expected Failed, got %s", w2.Status)
	}
	if w2.LastError == nil || w2.LastError.Kind != FailureProvider {
		t.Error("expected provider failure")
	}
}

func TestObserveCompletionError(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	w2, err := r.ObserveCompletion(w.WorkerID, "error", 0)
	if err != nil {
		t.Fatalf("ObserveCompletion failed: %v", err)
	}
	if w2.Status != StatusFailed {
		t.Errorf("expected Failed, got %s", w2.Status)
	}
}

func TestRestart(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	r.Observe(w.WorkerID, "ready for input")

	w2, err := r.Restart(w.WorkerID)
	if err != nil {
		t.Fatalf("Restart failed: %v", err)
	}
	if w2.Status != StatusSpawning {
		t.Errorf("expected Spawning after restart, got %s", w2.Status)
	}
	if w2.PromptDeliveryAttempts != 0 {
		t.Error("expected delivery attempts reset")
	}
}

func TestAwaitReady(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	snap, err := r.AwaitReady(w.WorkerID)
	if err != nil {
		t.Fatalf("AwaitReady failed: %v", err)
	}
	if snap.Ready {
		t.Error("expected not ready in Spawning")
	}

	r.Observe(w.WorkerID, "ready for input")
	snap2, _ := r.AwaitReady(w.WorkerID)
	if !snap2.Ready {
		t.Error("expected ready after observe")
	}
}

func TestGetReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	w1 := r.Get(w.WorkerID)
	w1.Status = StatusFinished

	w2 := r.Get(w.WorkerID)
	if w2.Status != StatusSpawning {
		t.Error("Get should return defensive copy")
	}
}

func TestGetUnknownWorker(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Get("nonexistent")
	if w != nil {
		t.Error("expected nil for unknown worker")
	}
}

func TestConcurrentRegistryAccess(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := r.Create("/tmp", nil, false)
			r.Get(w.WorkerID)
			r.Observe(w.WorkerID, "ready for input")
			snap, _ := r.AwaitReady(w.WorkerID)
			if snap == nil {
				t.Error("snapshot should not be nil")
			}
		}()
	}

	wg.Wait()
}

func TestDetectTrustPromptPatterns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		{"do you trust the files in this folder", true},
		{"Trust the files in this folder", true},
		{"trust this folder", true},
		{"allow and continue", true},
		{"yes, proceed", true},
		{"normal output text", false},
		{"building project...", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := detectTrustPrompt(strings.ToLower(tt.input))
			if got != tt.want {
				t.Errorf("detectTrustPrompt(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestDetectReadyForPromptPatterns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		{"Ready for input", true},
		{"Ready for your input", true},
		{"send a message", true},
		{"ready for prompt", true},
		{"> ", true},
		{"› ", true},
		{"❯ ", true},
		{"│ > ", true},
		{"$ ", false},
		{"building...", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			lowered := strings.ToLower(tt.input)
			got := detectReadyForPrompt(tt.input, lowered)
			if got != tt.want {
				t.Errorf("detectReadyForPrompt(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestDetectRunningCue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		{"thinking", true},
		{"working", true},
		{"running tests", true},
		{"inspecting", true},
		{"analyzing", true},
		{"idle", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := detectRunningCue(tt.input)
			if got != tt.want {
				t.Errorf("detectRunningCue(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestPathMatchesAllowlist(t *testing.T) {
	t.Parallel()
	tests := []struct {
		cwd, root string
		want      bool
	}{
		{"/home/user/project", "/home/user", true},
		{"/home/user", "/home/user", true},
		{"/other/dir", "/home/user", false},
		{"/home/user-evil", "/home/user", false},
	}
	for _, tt := range tests {
		got := pathMatchesAllowlist(tt.cwd, tt.root)
		if got != tt.want {
			t.Errorf("pathMatchesAllowlist(%q, %q) = %v, want %v", tt.cwd, tt.root, got, tt.want)
		}
	}
}

func TestPromptPreview(t *testing.T) {
	t.Parallel()
	short := "short"
	if got := promptPreview(short); got != short {
		t.Errorf("expected %q, got %q", short, got)
	}
	long := "this is a very long prompt that exceeds the forty-eight character limit significantly"
	got := promptPreview(long)
	if len([]rune(got)) > 49 {
		t.Errorf("expected truncated preview, got len=%d: %q", len(got), got)
	}
	// Rust trims leading/trailing whitespace before truncation.
	whitespace := "  \n  hello world  \n  "
	if got := promptPreview(whitespace); got != "hello world" {
		t.Errorf("expected trimmed %q, got %q", "hello world", got)
	}
	// Trailing whitespace after truncation is trimmed.
	trailingWS := "this is a long prompt with trailing spaces       that exceeds the limit"
	gotTW := promptPreview(trailingWS)
	runes := []rune(gotTW)
	if len(runes) > 0 && (runes[len(runes)-1] == ' ' || runes[len(runes)-1] == '\t') {
		t.Errorf("expected trailing whitespace trimmed before ellipsis, got %q", gotTW)
	}
}

func TestPromptMisdeliveryDetail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		target WorkerPromptTarget
		want   string
	}{
		{TargetShell, "shell misdelivery detected"},
		{TargetWrongTarget, "prompt landed in wrong target"},
		{TargetUnknown, "prompt delivery failure detected"},
	}
	for _, tt := range tests {
		got := promptMisdeliveryDetail(tt.target)
		if got != tt.want {
			t.Errorf("promptMisdeliveryDetail(%s) = %q, want %q", tt.target, got, tt.want)
		}
	}
}

func TestLooksLikeCwdLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		{"/home/user", true},
		{"~/project", true},
		{"./dir", true},
		{"some/path", true},
		{"word", false},
	}
	for _, tt := range tests {
		got := looksLikeCwdLabel(tt.input)
		if got != tt.want {
			t.Errorf("looksLikeCwdLabel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestIsShellPrompt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		{"user@host:~$", true},
		{"$", true},
		{"%", true},
		{"#", true},
		{">", false},
		{"text", false},
	}
	for _, tt := range tests {
		got := isShellPrompt(tt.input)
		if got != tt.want {
			t.Errorf("isShellPrompt(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestWorkerStatusJSONRoundTrip(t *testing.T) {
	t.Parallel()
	for _, s := range []WorkerStatus{StatusSpawning, StatusTrustRequired, StatusReadyForPrompt, StatusRunning, StatusFinished, StatusFailed} {
		data, err := s.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON(%s): %v", s, err)
		}
		var got WorkerStatus
		if err := got.UnmarshalJSON(data); err != nil {
			t.Fatalf("UnmarshalJSON(%s): %v", string(data), err)
		}
		if got != s {
			t.Errorf("round-trip: got %s, want %s", got, s)
		}
	}
}

func TestPromptDeliveryDetectsWrongTarget(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp/repo-target-a", nil, true)

	r.Observe(w.WorkerID, "Ready for input\n>")

	prompt := "Run the worker bootstrap tests"
	r.SendPrompt(w.WorkerID, &prompt)

	recovered, err := r.Observe(w.WorkerID,
		"/tmp/repo-target-b % Run the worker bootstrap tests\nzsh: command not found: Run")
	if err != nil {
		t.Fatalf("Observe failed: %v", err)
	}
	if recovered.Status != StatusReadyForPrompt {
		t.Errorf("expected ReadyForPrompt, got %s", recovered.Status)
	}
	if recovered.ReplayPrompt == nil || *recovered.ReplayPrompt != prompt {
		t.Error("expected replay prompt to be set")
	}
}

func TestEmitStateFileWritesJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := NewWorkerRegistry()
	w := r.Create(dir, nil, false)

	statePath := filepath.Join(dir, ".claw", "worker-state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state file should exist: %v", err)
	}

	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("state file should be valid JSON: %v", err)
	}
	if state["status"] != "spawning" {
		t.Errorf("status = %v, want 'spawning'", state["status"])
	}

	// Transition to ready
	r.Observe(w.WorkerID, "Ready for input\n>")
	data2, _ := os.ReadFile(statePath)
	json.Unmarshal(data2, &state)
	if state["status"] != "ready_for_prompt" {
		t.Errorf("status after observe = %v, want 'ready_for_prompt'", state["status"])
	}
}

func TestNormalizePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"/home/user/project/", "/home/user/project"},
		{"/home/user/../user/project", "/home/user/project"},
		{".", "."},
		{"/", "/"},
	}
	for _, tt := range tests {
		got := NormalizePath(tt.input)
		if got != tt.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsDegraded(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	degraded, err := r.IsDegraded(w.WorkerID, 3600)
	if err != nil {
		t.Fatalf("IsDegraded: %v", err)
	}
	if degraded {
		t.Error("expected not degraded (just created)")
	}

	// Unknown worker.
	_, err = r.IsDegraded("nonexistent", 3600)
	if err == nil {
		t.Error("expected error for unknown worker")
	}
}

func TestIsDegradedTerminalState(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)
	r.Terminate(w.WorkerID)

	degraded, _ := r.IsDegraded(w.WorkerID, 0) // threshold 0 = always degraded if not terminal
	if degraded {
		t.Error("terminal state should never be degraded")
	}
}

func TestReplayArmedPrompt(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, true) // auto-recover enabled

	r.Observe(w.WorkerID, "ready for input")
	prompt := "Write code"
	r.SendPrompt(w.WorkerID, &prompt)

	// Simulate prompt misdelivery.
	r.Observe(w.WorkerID, "Write code\ncommand not found")

	w2 := r.Get(w.WorkerID)
	if w2.ReplayPrompt == nil {
		t.Fatal("expected replay prompt armed")
	}

	// Replay.
	w3, err := r.ReplayArmedPrompt(w.WorkerID)
	if err != nil {
		t.Fatalf("ReplayArmedPrompt: %v", err)
	}
	if w3.Status != StatusRunning {
		t.Errorf("expected Running, got %s", w3.Status)
	}
	if w3.LastPrompt == nil || *w3.LastPrompt != prompt {
		t.Error("expected replayed prompt")
	}
	if w3.ReplayPrompt != nil {
		t.Error("expected replay prompt cleared after replay")
	}
}

func TestReplayArmedPromptNoReplay(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)
	r.Observe(w.WorkerID, "ready for input")

	_, err := r.ReplayArmedPrompt(w.WorkerID)
	if err == nil {
		t.Error("expected error when no replay prompt armed")
	}
}

func TestObserveCompletionWithEndTurnAndTokens(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	w2, err := r.ObserveCompletion(w.WorkerID, "end_turn", 500)
	if err != nil {
		t.Fatalf("ObserveCompletion: %v", err)
	}
	if w2.Status != StatusFinished {
		t.Errorf("expected Finished, got %s", w2.Status)
	}
}

func TestObserveCompletionMaxTokens(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	w := r.Create("/tmp", nil, false)

	w2, err := r.ObserveCompletion(w.WorkerID, "max_tokens", 1000)
	if err != nil {
		t.Fatalf("ObserveCompletion: %v", err)
	}
	if w2.Status != StatusFinished {
		t.Errorf("expected Finished for max_tokens with tokens, got %s", w2.Status)
	}
}

func TestConcurrentRegistryAccessExtended(t *testing.T) {
	t.Parallel()
	r := NewWorkerRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := r.Create("/tmp", nil, true)
			r.Get(w.WorkerID)
			r.Observe(w.WorkerID, "Do you trust the files in this folder?")
			r.Observe(w.WorkerID, "Ready for input\n>")
			prompt := "test prompt"
			r.SendPrompt(w.WorkerID, &prompt)
			r.Observe(w.WorkerID, "thinking...")
			r.ObserveCompletion(w.WorkerID, "stop", 100)
		}()
	}
	wg.Wait()
}
