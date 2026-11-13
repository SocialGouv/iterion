package mcp

import (
	"strings"
	"testing"
	"time"
)

func TestPhaseStringRoundTrip(t *testing.T) {
	t.Parallel()
	for _, phase := range AllPhases() {
		s := phase.String()
		if s == "unknown" {
			t.Errorf("phase %d has unknown string", phase)
		}
	}
}

func TestPhaseJSONRoundTrip(t *testing.T) {
	t.Parallel()
	for _, phase := range AllPhases() {
		data, err := phase.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON(%s) error: %v", phase, err)
		}
		var decoded McpLifecyclePhase
		if err := decoded.UnmarshalJSON(data); err != nil {
			t.Fatalf("UnmarshalJSON(%s) error: %v", string(data), err)
		}
		if decoded != phase {
			t.Errorf("round-trip mismatch: got %v, want %v", decoded, phase)
		}
	}
}

func TestPhaseUnmarshalInvalid(t *testing.T) {
	t.Parallel()
	var p McpLifecyclePhase
	if err := p.UnmarshalJSON([]byte(`"bogus_phase"`)); err == nil {
		t.Error("expected error for unknown phase")
	}
}

// ---------------------------------------------------------------------------
// Phase transition validation
// ---------------------------------------------------------------------------

func TestValidLinearProgression(t *testing.T) {
	t.Parallel()
	linear := []McpLifecyclePhase{
		PhaseConfigLoad,
		PhaseServerRegistration,
		PhaseSpawnConnect,
		PhaseInitializeHandshake,
		PhaseToolDiscovery,
		PhaseResourceDiscovery,
		PhaseReady,
		PhaseInvocation,
	}
	for i := 0; i < len(linear)-1; i++ {
		from, to := linear[i], linear[i+1]
		if !ValidatePhaseTransition(from, to) {
			t.Errorf("expected valid transition: %s → %s", from, to)
		}
	}
}

func TestToolDiscoveryCanSkipResourceDiscovery(t *testing.T) {
	t.Parallel()
	if !ValidatePhaseTransition(PhaseToolDiscovery, PhaseReady) {
		t.Error("ToolDiscovery → Ready should be valid")
	}
}

func TestReadyInvocationLoop(t *testing.T) {
	t.Parallel()
	if !ValidatePhaseTransition(PhaseReady, PhaseInvocation) {
		t.Error("Ready → Invocation should be valid")
	}
	if !ValidatePhaseTransition(PhaseInvocation, PhaseReady) {
		t.Error("Invocation → Ready should be valid")
	}
}

func TestShutdownFromAnyExceptCleanup(t *testing.T) {
	t.Parallel()
	for _, phase := range AllPhases() {
		if phase == PhaseCleanup {
			if ValidatePhaseTransition(phase, PhaseShutdown) {
				t.Error("Cleanup → Shutdown should be invalid")
			}
			continue
		}
		if !ValidatePhaseTransition(phase, PhaseShutdown) {
			t.Errorf("%s → Shutdown should be valid", phase)
		}
	}
}

func TestErrorSurfacingFromAnyExceptShutdownCleanup(t *testing.T) {
	t.Parallel()
	for _, phase := range AllPhases() {
		if phase == PhaseShutdown || phase == PhaseCleanup {
			if ValidatePhaseTransition(phase, PhaseErrorSurfacing) {
				t.Errorf("%s → ErrorSurfacing should be invalid", phase)
			}
			continue
		}
		if !ValidatePhaseTransition(phase, PhaseErrorSurfacing) {
			t.Errorf("%s → ErrorSurfacing should be valid", phase)
		}
	}
}

func TestShutdownToCleanup(t *testing.T) {
	t.Parallel()
	if !ValidatePhaseTransition(PhaseShutdown, PhaseCleanup) {
		t.Error("Shutdown → Cleanup should be valid")
	}
}

func TestCleanupIsTerminal(t *testing.T) {
	t.Parallel()
	for _, phase := range AllPhases() {
		if ValidatePhaseTransition(PhaseCleanup, phase) {
			t.Errorf("Cleanup → %s should be invalid (Cleanup is terminal)", phase)
		}
	}
}

func TestInvalidTransitions(t *testing.T) {
	t.Parallel()
	invalid := [][2]McpLifecyclePhase{
		{PhaseConfigLoad, PhaseReady},
		{PhaseReady, PhaseConfigLoad},
		{PhaseInvocation, PhaseToolDiscovery},
		{PhaseResourceDiscovery, PhaseToolDiscovery},
	}
	for _, pair := range invalid {
		if ValidatePhaseTransition(pair[0], pair[1]) {
			t.Errorf("expected invalid transition: %s → %s", pair[0], pair[1])
		}
	}
}

// ---------------------------------------------------------------------------
// Validator integration tests
// ---------------------------------------------------------------------------

func TestValidatorHappyPath(t *testing.T) {
	t.Parallel()
	v := NewMcpLifecycleValidator()

	phases := []McpLifecyclePhase{
		PhaseConfigLoad,
		PhaseServerRegistration,
		PhaseSpawnConnect,
		PhaseInitializeHandshake,
		PhaseToolDiscovery,
		PhaseReady,
	}
	for _, p := range phases {
		result, err := v.RunPhase(p)
		if err != nil {
			t.Fatalf("RunPhase(%s) error: %v", p, err)
		}
		if result.Kind != PhaseResultSuccess {
			t.Errorf("RunPhase(%s) kind = %v, want Success", p, result.Kind)
		}
		if result.Phase != p {
			t.Errorf("RunPhase(%s) phase = %v", p, result.Phase)
		}
	}

	state := v.State()
	if state.CurrentPhase() == nil || *state.CurrentPhase() != PhaseReady {
		t.Errorf("current phase = %v, want Ready", state.CurrentPhase())
	}
	if len(state.Results()) != len(phases) {
		t.Errorf("len(results) = %d, want %d", len(state.Results()), len(phases))
	}
}

func TestValidatorRejectsInvalidFirstPhase(t *testing.T) {
	t.Parallel()
	v := NewMcpLifecycleValidator()
	_, err := v.RunPhase(PhaseReady)
	if err == nil {
		t.Fatal("expected error for non-ConfigLoad first phase")
	}
}

func TestValidatorRejectsInvalidTransition(t *testing.T) {
	t.Parallel()
	v := NewMcpLifecycleValidator()
	v.RunPhase(PhaseConfigLoad)
	_, err := v.RunPhase(PhaseReady) // skip ServerRegistration
	if err == nil {
		t.Fatal("expected error for invalid transition")
	}
}

func TestValidatorRecordFailure(t *testing.T) {
	t.Parallel()
	v := NewMcpLifecycleValidator()
	v.RunPhase(PhaseConfigLoad)
	v.RunPhase(PhaseServerRegistration)
	v.RunPhase(PhaseSpawnConnect)

	server := "test-server"
	errSurface := NewMcpErrorSurface(
		PhaseSpawnConnect,
		&server,
		"connection refused",
		map[string]string{"port": "3000"},
		true,
	)
	result := v.RecordFailure(errSurface)
	if result.Kind != PhaseResultFailure {
		t.Errorf("kind = %v, want Failure", result.Kind)
	}
	if result.Error == nil {
		t.Fatal("expected error in result")
	}
	if result.Error.Message != "connection refused" {
		t.Errorf("message = %q, want %q", result.Error.Message, "connection refused")
	}

	state := v.State()
	if state.CurrentPhase() == nil || *state.CurrentPhase() != PhaseErrorSurfacing {
		t.Errorf("current phase = %v, want ErrorSurfacing", state.CurrentPhase())
	}
	errs := state.ErrorsForPhase(PhaseSpawnConnect)
	if len(errs) != 1 {
		t.Errorf("errors for SpawnConnect = %d, want 1", len(errs))
	}
}

func TestValidatorRecoverableErrorResumesToReady(t *testing.T) {
	t.Parallel()
	v := NewMcpLifecycleValidator()

	// Walk to Ready state.
	for _, p := range []McpLifecyclePhase{
		PhaseConfigLoad, PhaseServerRegistration, PhaseSpawnConnect,
		PhaseInitializeHandshake, PhaseToolDiscovery, PhaseReady,
		PhaseInvocation,
	} {
		if _, err := v.RunPhase(p); err != nil {
			t.Fatalf("RunPhase(%s) error: %v", p, err)
		}
	}

	// Record a recoverable error.
	errSurface := NewMcpErrorSurface(PhaseInvocation, nil, "transient", nil, true)
	v.RecordFailure(errSurface)

	// Should be able to resume to Ready.
	_, err := v.RunPhase(PhaseReady)
	if err != nil {
		t.Fatalf("expected resume to Ready after recoverable error, got: %v", err)
	}
}

func TestValidatorNonRecoverableErrorBlocksReady(t *testing.T) {
	t.Parallel()
	v := NewMcpLifecycleValidator()

	for _, p := range []McpLifecyclePhase{
		PhaseConfigLoad, PhaseServerRegistration, PhaseSpawnConnect,
		PhaseInitializeHandshake, PhaseToolDiscovery, PhaseReady,
	} {
		v.RunPhase(p)
	}

	// Record a non-recoverable error.
	errSurface := NewMcpErrorSurface(PhaseReady, nil, "fatal", nil, false)
	v.RecordFailure(errSurface)

	// Should NOT be able to resume to Ready.
	_, err := v.RunPhase(PhaseReady)
	if err == nil {
		t.Fatal("expected error: non-recoverable should block Ready resume")
	}

	// But Shutdown should still work.
	_, err = v.RunPhase(PhaseShutdown)
	if err != nil {
		t.Fatalf("Shutdown from ErrorSurfacing should work, got: %v", err)
	}
}

func TestValidatorRecordTimeout(t *testing.T) {
	t.Parallel()
	v := NewMcpLifecycleValidator()
	v.RunPhase(PhaseConfigLoad)
	v.RunPhase(PhaseServerRegistration)
	v.RunPhase(PhaseSpawnConnect)

	server := "slow-server"
	result := v.RecordTimeout(
		PhaseSpawnConnect,
		5*time.Second,
		&server,
		map[string]string{"reason": "deadline exceeded"},
	)

	if result.Kind != PhaseResultTimeout {
		t.Errorf("kind = %v, want Timeout", result.Kind)
	}
	if result.Duration != 5*time.Second {
		t.Errorf("duration = %v, want 5s", result.Duration)
	}
	if result.Error == nil {
		t.Fatal("expected error in timeout result")
	}
	if result.Error.Context["waited_ms"] != "5000" {
		t.Errorf("waited_ms = %q, want 5000", result.Error.Context["waited_ms"])
	}
}

// ---------------------------------------------------------------------------
// McpErrorSurface
// ---------------------------------------------------------------------------

func TestErrorSurfaceDisplayFormat(t *testing.T) {
	t.Parallel()
	server := "my-server"
	err := NewMcpErrorSurface(
		PhaseSpawnConnect,
		&server,
		"connection refused",
		nil,
		false,
	)
	msg := err.Error()
	if msg == "" {
		t.Error("expected non-empty error message")
	}
	// Verify key components are present.
	for _, want := range []string{"spawn_connect", "connection refused", "my-server", "non-recoverable"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing %q", msg, want)
		}
	}
}

func TestErrorSurfaceNoServer(t *testing.T) {
	t.Parallel()
	err := NewMcpErrorSurface(PhaseReady, nil, "oops", nil, true)
	msg := err.Error()
	if !strings.Contains(msg, "<unknown>") {
		t.Errorf("expected <unknown> for nil server, got: %s", msg)
	}
	if !strings.Contains(msg, "recoverable") {
		t.Errorf("expected recoverable label, got: %s", msg)
	}
}

// ---------------------------------------------------------------------------
// DegradedReport
// ---------------------------------------------------------------------------

func TestDegradedReportDeduplication(t *testing.T) {
	t.Parallel()
	report := NewMcpDegradedReport(
		[]string{"b", "a", "b", "a"},
		nil,
		[]string{"tool_c", "tool_a", "tool_c"},
		[]string{"tool_a", "tool_b", "tool_c", "tool_d"},
	)

	if len(report.WorkingServers) != 2 {
		t.Errorf("working servers = %v, want 2 deduped", report.WorkingServers)
	}
	if report.WorkingServers[0] != "a" || report.WorkingServers[1] != "b" {
		t.Errorf("working servers not sorted: %v", report.WorkingServers)
	}
	if len(report.AvailableTools) != 2 {
		t.Errorf("available tools = %v, want 2 deduped", report.AvailableTools)
	}
	if len(report.MissingTools) != 2 {
		t.Errorf("missing tools = %v, want [tool_b, tool_d]", report.MissingTools)
	}
}

func TestDegradedReportEmptyInputs(t *testing.T) {
	t.Parallel()
	report := NewMcpDegradedReport(nil, nil, nil, nil)
	if len(report.WorkingServers) != 0 {
		t.Errorf("expected empty working servers, got %v", report.WorkingServers)
	}
	if len(report.MissingTools) != 0 {
		t.Errorf("expected empty missing tools, got %v", report.MissingTools)
	}
}
