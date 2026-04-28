package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// McpLifecyclePhase — 11 phases matching the Rust reference
// (crates/runtime/src/mcp_lifecycle_hardened.rs)
// ---------------------------------------------------------------------------

// McpLifecyclePhase represents a phase in the MCP server lifecycle.
type McpLifecyclePhase int

const (
	PhaseConfigLoad McpLifecyclePhase = iota
	PhaseServerRegistration
	PhaseSpawnConnect
	PhaseInitializeHandshake
	PhaseToolDiscovery
	PhaseResourceDiscovery
	PhaseReady
	PhaseInvocation
	PhaseErrorSurfacing
	PhaseShutdown
	PhaseCleanup
)

var phaseStrings = [...]string{
	"config_load",
	"server_registration",
	"spawn_connect",
	"initialize_handshake",
	"tool_discovery",
	"resource_discovery",
	"ready",
	"invocation",
	"error_surfacing",
	"shutdown",
	"cleanup",
}

func (p McpLifecyclePhase) String() string {
	if int(p) < len(phaseStrings) {
		return phaseStrings[p]
	}
	return "unknown"
}

func (p McpLifecyclePhase) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.String())
}

func (p *McpLifecyclePhase) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	for i, name := range phaseStrings {
		if name == s {
			*p = McpLifecyclePhase(i)
			return nil
		}
	}
	return fmt.Errorf("unknown MCP lifecycle phase: %q", s)
}

// AllPhases returns all 11 lifecycle phases in order.
func AllPhases() []McpLifecyclePhase {
	phases := make([]McpLifecyclePhase, len(phaseStrings))
	for i := range phaseStrings {
		phases[i] = McpLifecyclePhase(i)
	}
	return phases
}

// ---------------------------------------------------------------------------
// McpErrorSurface
// ---------------------------------------------------------------------------

// McpErrorSurface is a structured error with lifecycle phase context.
type McpErrorSurface struct {
	Phase       McpLifecyclePhase `json:"phase"`
	ServerName  *string           `json:"server_name,omitempty"`
	Message     string            `json:"message"`
	Context     map[string]string `json:"context,omitempty"`
	Recoverable bool              `json:"recoverable"`
	Timestamp   uint64            `json:"timestamp"`
}

// NewMcpErrorSurface creates a new error surface with the current timestamp.
func NewMcpErrorSurface(
	phase McpLifecyclePhase,
	serverName *string,
	message string,
	context map[string]string,
	recoverable bool,
) McpErrorSurface {
	return McpErrorSurface{
		Phase:       phase,
		ServerName:  serverName,
		Message:     message,
		Context:     context,
		Recoverable: recoverable,
		Timestamp:   nowSecs(),
	}
}

func (e McpErrorSurface) Error() string {
	server := "<unknown>"
	if e.ServerName != nil {
		server = *e.ServerName
	}
	recov := "non-recoverable"
	if e.Recoverable {
		recov = "recoverable"
	}
	return fmt.Sprintf("MCP lifecycle error during %s: %s (server: %s) [%s]",
		e.Phase, e.Message, server, recov)
}

// ---------------------------------------------------------------------------
// McpPhaseResult
// ---------------------------------------------------------------------------

// McpPhaseResultKind discriminates the three outcome types.
type McpPhaseResultKind int

const (
	PhaseResultSuccess McpPhaseResultKind = iota
	PhaseResultFailure
	PhaseResultTimeout
)

// McpPhaseResult records the outcome of running a lifecycle phase.
type McpPhaseResult struct {
	Kind     McpPhaseResultKind `json:"kind"`
	Phase    McpLifecyclePhase  `json:"phase"`
	Duration time.Duration      `json:"duration,omitempty"` // Success and Timeout
	Error    *McpErrorSurface   `json:"error,omitempty"`    // Failure and Timeout
}

// ---------------------------------------------------------------------------
// McpLifecycleState
// ---------------------------------------------------------------------------

// McpLifecycleState tracks the current phase, errors, timestamps, and results.
type McpLifecycleState struct {
	currentPhase    *McpLifecyclePhase
	phaseErrors     map[McpLifecyclePhase][]McpErrorSurface
	phaseTimestamps map[McpLifecyclePhase]uint64
	phaseResults    []McpPhaseResult
}

// NewMcpLifecycleState creates a new empty lifecycle state.
func NewMcpLifecycleState() *McpLifecycleState {
	return &McpLifecycleState{
		phaseErrors:     make(map[McpLifecyclePhase][]McpErrorSurface),
		phaseTimestamps: make(map[McpLifecyclePhase]uint64),
	}
}

// CurrentPhase returns the current lifecycle phase, or nil if none.
func (s *McpLifecycleState) CurrentPhase() *McpLifecyclePhase {
	return s.currentPhase
}

// ErrorsForPhase returns the errors recorded for a specific phase.
func (s *McpLifecycleState) ErrorsForPhase(phase McpLifecyclePhase) []McpErrorSurface {
	return s.phaseErrors[phase]
}

// Results returns all recorded phase results.
func (s *McpLifecycleState) Results() []McpPhaseResult {
	return s.phaseResults
}

// PhaseTimestamps returns the timestamp map for all visited phases.
func (s *McpLifecycleState) PhaseTimestamps() map[McpLifecyclePhase]uint64 {
	return s.phaseTimestamps
}

// PhaseTimestamp returns the timestamp for a specific phase, or nil.
func (s *McpLifecycleState) PhaseTimestamp(phase McpLifecyclePhase) *uint64 {
	ts, ok := s.phaseTimestamps[phase]
	if !ok {
		return nil
	}
	return &ts
}

func (s *McpLifecycleState) recordPhase(phase McpLifecyclePhase) {
	s.currentPhase = &phase
	s.phaseTimestamps[phase] = nowSecs()
}

func (s *McpLifecycleState) recordError(err McpErrorSurface) {
	s.phaseErrors[err.Phase] = append(s.phaseErrors[err.Phase], err)
}

func (s *McpLifecycleState) recordResult(result McpPhaseResult) {
	s.phaseResults = append(s.phaseResults, result)
}

// surfaceError stores the error under its originating phase (for diagnostics)
// and under ErrorSurfacing (for recovery decisions), then transitions to
// ErrorSurfacing.
func (s *McpLifecycleState) surfaceError(err McpErrorSurface) {
	s.recordError(err)
	s.phaseErrors[PhaseErrorSurfacing] = append(
		s.phaseErrors[PhaseErrorSurfacing], err)
	s.recordPhase(PhaseErrorSurfacing)
}

// canResumeAfterError returns true if the most recent error in the
// ErrorSurfacing phase is marked recoverable.
func (s *McpLifecycleState) canResumeAfterError() bool {
	errs := s.phaseErrors[PhaseErrorSurfacing]
	if len(errs) == 0 {
		return false
	}
	return errs[len(errs)-1].Recoverable
}

// ---------------------------------------------------------------------------
// McpLifecycleValidator — the core state machine
// ---------------------------------------------------------------------------

// McpLifecycleValidator enforces valid phase transitions and tracks results.
type McpLifecycleValidator struct {
	mu    sync.Mutex
	state *McpLifecycleState
}

// NewMcpLifecycleValidator creates a new validator with empty state.
func NewMcpLifecycleValidator() *McpLifecycleValidator {
	return &McpLifecycleValidator{
		state: NewMcpLifecycleState(),
	}
}

// State returns a snapshot of the current lifecycle state.
// The caller must not mutate internal slices/maps.
func (v *McpLifecycleValidator) State() *McpLifecycleState {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state
}

// ValidatePhaseTransition checks whether a transition from → to is legal.
// This is a pure function that does not mutate state.
func ValidatePhaseTransition(from, to McpLifecyclePhase) bool {
	// Shutdown is reachable from any phase except Cleanup.
	if to == PhaseShutdown && from != PhaseCleanup {
		return true
	}
	// ErrorSurfacing is reachable from any phase except Shutdown and Cleanup.
	if to == PhaseErrorSurfacing && from != PhaseShutdown && from != PhaseCleanup {
		return true
	}
	// Linear progression and specific transitions.
	switch from {
	case PhaseConfigLoad:
		return to == PhaseServerRegistration
	case PhaseServerRegistration:
		return to == PhaseSpawnConnect
	case PhaseSpawnConnect:
		return to == PhaseInitializeHandshake
	case PhaseInitializeHandshake:
		return to == PhaseToolDiscovery
	case PhaseToolDiscovery:
		return to == PhaseResourceDiscovery || to == PhaseReady
	case PhaseResourceDiscovery:
		return to == PhaseReady
	case PhaseReady:
		return to == PhaseInvocation
	case PhaseInvocation:
		return to == PhaseReady
	case PhaseErrorSurfacing:
		// Can resume to Ready only if last error was recoverable.
		// Caller must check canResumeAfterError() separately for Ready.
		// Shutdown is always allowed (handled above).
		return to == PhaseReady || to == PhaseShutdown
	case PhaseShutdown:
		return to == PhaseCleanup
	case PhaseCleanup:
		return false // Terminal — no transitions out.
	}
	return false
}

// RunPhase attempts a phase transition and records a Success result.
// Returns an error if the transition is invalid.
func (v *McpLifecycleValidator) RunPhase(phase McpLifecyclePhase) (McpPhaseResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.state.currentPhase != nil {
		from := *v.state.currentPhase
		if !ValidatePhaseTransition(from, phase) {
			return McpPhaseResult{}, fmt.Errorf(
				"invalid MCP phase transition: %s → %s", from, phase)
		}
		// Additional guard: ErrorSurfacing → Ready requires recoverable error.
		if from == PhaseErrorSurfacing && phase == PhaseReady && !v.state.canResumeAfterError() {
			return McpPhaseResult{}, fmt.Errorf(
				"cannot resume to Ready: last error is not recoverable")
		}
	} else {
		// First phase must be ConfigLoad.
		if phase != PhaseConfigLoad {
			return McpPhaseResult{}, fmt.Errorf(
				"initial MCP phase must be config_load, got %s", phase)
		}
	}

	start := time.Now()
	v.state.recordPhase(phase)
	result := McpPhaseResult{
		Kind:     PhaseResultSuccess,
		Phase:    phase,
		Duration: time.Since(start),
	}
	v.state.recordResult(result)
	return result, nil
}

// RecordFailure records an error and transitions to ErrorSurfacing.
func (v *McpLifecycleValidator) RecordFailure(err McpErrorSurface) McpPhaseResult {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.state.surfaceError(err)
	result := McpPhaseResult{
		Kind:  PhaseResultFailure,
		Phase: err.Phase,
		Error: &err,
	}
	v.state.recordResult(result)
	return result
}

// RecordTimeout records a timeout error with a waited duration.
func (v *McpLifecycleValidator) RecordTimeout(
	phase McpLifecyclePhase,
	waited time.Duration,
	serverName *string,
	ctx map[string]string,
) McpPhaseResult {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Copy the caller's map to avoid mutating it.
	merged := make(map[string]string, len(ctx)+1)
	for k, val := range ctx {
		merged[k] = val
	}
	merged["waited_ms"] = fmt.Sprintf("%d", waited.Milliseconds())

	err := McpErrorSurface{
		Phase:       phase,
		ServerName:  serverName,
		Message:     fmt.Sprintf("timeout after %s", waited),
		Context:     merged,
		Recoverable: false,
		Timestamp:   nowSecs(),
	}
	v.state.surfaceError(err)
	result := McpPhaseResult{
		Kind:     PhaseResultTimeout,
		Phase:    phase,
		Duration: waited,
		Error:    &err,
	}
	v.state.recordResult(result)
	return result
}

// ---------------------------------------------------------------------------
// McpFailedServer / McpDegradedReport — degraded mode reporting
// ---------------------------------------------------------------------------

// McpFailedServer records a server that failed during lifecycle.
type McpFailedServer struct {
	ServerName string            `json:"server_name"`
	Phase      McpLifecyclePhase `json:"phase"`
	Error      McpErrorSurface   `json:"error"`
}

// McpDegradedReport summarises a degraded MCP environment.
type McpDegradedReport struct {
	WorkingServers []string          `json:"working_servers"`
	FailedServers  []McpFailedServer `json:"failed_servers"`
	AvailableTools []string          `json:"available_tools"`
	MissingTools   []string          `json:"missing_tools"`
}

// NewMcpDegradedReport creates a report, deduplicating and sorting string lists.
func NewMcpDegradedReport(
	workingServers []string,
	failedServers []McpFailedServer,
	availableTools []string,
	expectedTools []string,
) McpDegradedReport {
	working := dedupeSorted(workingServers)
	available := dedupeSorted(availableTools)

	// Compute missing = expected - available.
	availSet := make(map[string]bool, len(available))
	for _, t := range available {
		availSet[t] = true
	}
	missing := make([]string, 0)
	for _, t := range expectedTools {
		if !availSet[t] {
			missing = append(missing, t)
		}
	}
	sort.Strings(missing)

	return McpDegradedReport{
		WorkingServers: working,
		FailedServers:  failedServers,
		AvailableTools: available,
		MissingTools:   missing,
	}
}

func dedupeSorted(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	sort.Strings(values)
	result := make([]string, 0, len(values))
	result = append(result, values[0])
	for i := 1; i < len(values); i++ {
		if values[i] != values[i-1] {
			result = append(result, values[i])
		}
	}
	return result
}

func nowSecs() uint64 {
	return uint64(time.Now().Unix())
}
