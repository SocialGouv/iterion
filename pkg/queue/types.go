// Package queue defines the message contract exchanged between the
// iterion server (publisher) and the iterion runner (consumer).
//
// Today only the type definitions live here — the NATS publisher /
// consumer impl lands in plan §F T-25 (`pkg/queue/nats/`). Keeping
// the schema package separate is deliberate so editor backend tests
// can import the types without pulling in the NATS client.
//
// See cloud-ready plan §C.2 for the wire format and §J for the
// rationale behind the IRRef fallback.
package queue

import (
	"encoding/json"
	"fmt"
)

// SchemaVersion is incremented at every breaking change to the wire
// payload. Producers always set RunMessage.V = SchemaVersion;
// consumers reject any V they don't recognise so that a
// rolling-upgrade always upgrades the server first (which then never
// emits an unsupported version).
const SchemaVersion = 1

// RunMessage is the JSON envelope published on
// `iterion.queue.runs`. The runner deserialises it, takes the
// distributed lock, and runs the workflow described by IRCompiled
// (or fetches IRRef when the IR exceeds the NATS message size limit).
//
// Field order is stable to keep readable JSON diffs in tests.
type RunMessage struct {
	V              int                    `json:"v"`
	RunID          string                 `json:"run_id"`
	WorkflowName   string                 `json:"workflow_name"`
	WorkflowHash   string                 `json:"workflow_hash"`
	IRCompiled     json.RawMessage        `json:"ir_compiled,omitempty"`
	IRRef          *IRRef                 `json:"ir_ref,omitempty"`
	RepoURL        string                 `json:"repo_url,omitempty"`
	RepoSHA        string                 `json:"repo_sha,omitempty"`
	Vars           map[string]interface{} `json:"vars,omitempty"`
	SecretsRef     string                 `json:"secrets_ref,omitempty"`
	TimeoutSec     int                    `json:"timeout_sec,omitempty"`
	BackendConfig  BackendConfig          `json:"backend"`
	Resume         *ResumeSpec            `json:"resume,omitempty"`
	Trace          TraceContext           `json:"trace"`
	PublishedAtRFC string                 `json:"published_at"`
}

// IRBackend is the storage backend an IRRef points at.
type IRBackend string

const (
	IRBackendS3    IRBackend = "s3"
	IRBackendMongo IRBackend = "mongo"
)

// IRRef points at an out-of-band IR blob. Used when ast.MarshalFile
// output exceeds the NATS message size budget (~1 MB).
type IRRef struct {
	StorageKey string    `json:"storage_key"`
	Backend    IRBackend `json:"backend"`
}

// Backend is the LLM execution backend a runner picks for the run.
// "claw" is in-process; "claude_code" and "codex" fork external CLIs.
type Backend string

const (
	BackendClaw       Backend = "claw"
	BackendClaudeCode Backend = "claude_code"
	BackendCodex      Backend = "codex"
)

// BackendConfig carries the LLM backend selection per run.
type BackendConfig struct {
	Default       Backend `json:"default"`
	DelegateModel string  `json:"delegate_model,omitempty"`
}

// ResumeSpec is non-nil for resume publishes; the runner threads its
// fields into `runtime.Engine.Resume`.
type ResumeSpec struct {
	Answers map[string]interface{} `json:"answers,omitempty"`
	Force   bool                   `json:"force"`
}

// TraceContext propagates the originating editor span across NATS so
// runner-side spans inherit the parent. Encoded redundantly in the
// `traceparent` NATS header for fast extraction without decoding the
// body.
type TraceContext struct {
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`
}

// Validate enforces the invariants a runner must rely on before
// touching the workflow:
//   - schema version matches (rolling-upgrade safety)
//   - mandatory identifiers present
//   - exactly one of IRCompiled / IRRef is set (J-IR-too-large fallback)
func (m *RunMessage) Validate() error {
	if m == nil {
		return fmt.Errorf("queue: nil RunMessage")
	}
	if m.V != SchemaVersion {
		return fmt.Errorf("queue: schema version %d unsupported (want %d)", m.V, SchemaVersion)
	}
	if m.RunID == "" {
		return fmt.Errorf("queue: RunID required")
	}
	if m.WorkflowName == "" {
		return fmt.Errorf("queue: WorkflowName required")
	}
	hasIR := len(m.IRCompiled) > 0
	hasRef := m.IRRef != nil && m.IRRef.StorageKey != ""
	if hasIR == hasRef {
		// Both set OR both unset is an error: the runner must know
		// where the IR comes from.
		return fmt.Errorf("queue: exactly one of IRCompiled / IRRef must be set (got ircompiled=%t ref=%t)", hasIR, hasRef)
	}
	if hasRef {
		switch m.IRRef.Backend {
		case IRBackendS3, IRBackendMongo:
		default:
			return fmt.Errorf("queue: IRRef.Backend %q invalid (want s3|mongo)", m.IRRef.Backend)
		}
	}
	return nil
}
