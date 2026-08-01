// Package queue defines the message contract exchanged between the
// iterion server (publisher) and the iterion runner (consumer).
//
// Today only the type definitions live here — the NATS publisher /
// consumer impl lands in plan §F T-25 (`pkg/queue/nats/`). Keeping
// the schema package separate is deliberate so studio backend tests
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
//
// v=3 (2026-06-10): added BotID so cloud runners can qualify structured bot memory.
// v=4 (2026-07-11): added Budget so launch-time budget overrides reach the
// runner instead of being rejected at publish time. The version bump makes a
// stale runner reject the message loudly rather than silently dropping the
// caps the caller asked for.
// v=5 (2026-07-20): added Contributions so enabled-plugin skills and DSL
// `skills:` library references reach the runner pod, whose iterion home is
// empty. Bumped for the same reason as v=4: a stale runner must reject the
// message loudly rather than run the workflow WITHOUT the skills it was
// launched with — a run missing its deploy/platform skill still "succeeds"
// while doing the wrong thing, which is the exact façade this field prevents.
// v=6 (2026-08-01): added ModelOverrides so a launch-time model/backend/effort
// choice reaches the runner pod. Bumped for the same reason as v=4 and v=5: the
// operator picked a model in the studio and the preference was persisted, so a
// stale runner quietly executing the bot's DSL default would read as "my choice
// did nothing" with no error anywhere. Rejecting the message is the loud
// failure that keeps the choice provable.
const SchemaVersion = 6

// RunMessage is the JSON envelope published on
// `iterion.queue.runs`. The runner deserialises it, takes the
// distributed lock, and runs the workflow described by IRCompiled
// (or fetches IRRef when the IR exceeds the NATS message size limit).
//
// Field order is stable to keep readable JSON diffs in tests.
type RunMessage struct {
	V            int             `json:"v"`
	RunID        string          `json:"run_id"`
	WorkflowName string          `json:"workflow_name"`
	WorkflowHash string          `json:"workflow_hash"`
	IRCompiled   json.RawMessage `json:"ir_compiled,omitempty"`
	IRRef        *IRRef          `json:"ir_ref,omitempty"`
	RepoURL      string          `json:"repo_url,omitempty"`
	RepoSHA      string          `json:"repo_sha,omitempty"`
	// BotID is the stable bundle/bot identifier for this run. It qualifies
	// structured visibility=bot memory and is preserved on resume.
	BotID string `json:"bot_id,omitempty"`
	// Contributions carries the plugin markdown contributions and
	// skill-library skills the LAUNCHING instance resolved for this run (the
	// wire mirror of runtime.Contributions). A runner pod's iterion home is
	// ephemeral and empty, so without this an operator-installed plugin's
	// skill — or a DSL `skills:` library reference — silently never reaches
	// the workspace and only compiled-in builtins do. Nil (a message from
	// before this field, or a non-cloud publisher) makes the runner fall back
	// to local resolution, which is a no-op on a pod.
	Contributions *Contributions `json:"contributions,omitempty"`
	Vars          map[string]any `json:"vars,omitempty"`
	SecretsRef    string         `json:"secrets_ref,omitempty"`
	TimeoutSec    int            `json:"timeout_sec,omitempty"`
	// Budget carries launch-time budget-cap overrides ("non-zero wins,
	// zero inherits" — the wire mirror of ir.BudgetOverrides). The runner
	// applies it after loading the workflow and BEFORE its multitenant
	// cloud ceiling, so a tenant can only lower the effective caps.
	Budget *BudgetOverrides `json:"budget,omitempty"`
	// ModelOverrides carries the launch-time per-node/-group model, backend,
	// provider and reasoning-effort retargeting (the wire mirror of
	// store.RunModelOverride, folded back into model.ModelOverrides by the
	// runner). Without it the pod runs the bot's DSL defaults, so an operator
	// who picked a model in the studio — or persisted one as their assistant
	// preference — would see it applied to the run record and to nothing else.
	ModelOverrides []ModelOverride `json:"model_overrides,omitempty"`
	BackendConfig  BackendConfig   `json:"backend"`
	Resume         *ResumeSpec     `json:"resume,omitempty"`
	Trace          TraceContext    `json:"trace"`
	PublishedAtRFC string          `json:"published_at"`
	// TenantID is the team_id the run belongs to. Required in v=2.
	// Runners verify the loaded run document's tenant_id matches
	// before claiming the lock; a mismatch is treated as a corrupted
	// queue entry and the run is naked.
	TenantID string `json:"tenant_id"`
	// OrgID is the parent-org id the run's monthly LLM spend is
	// charged to — the same usage key the launch gate metered the
	// launch on (gateMonthlyCaps keys the counter by org so caps sum
	// across the org's teams). Optional: empty (message published
	// before this field existed, or a pre-backfill team with no org)
	// makes the runner fall back to TenantID, matching the gate's own
	// fallback for org-less teams.
	OrgID string `json:"org_id,omitempty"`
	// OwnerID is the user_id of the principal who initiated the run.
	// Used for audit logging; runners do NOT gate execution on it.
	OwnerID string `json:"owner_id,omitempty"`
	// ParentRunID is set on child runs spawned by a parent workflow
	// (e.g. by `iterion __scan-shards`). Empty for root runs. When
	// non-empty, the runner copies it into the persisted Run document
	// so the studio and inspect surfaces can render the parent/child
	// tree. See docs/security-bots-distributed.md.
	ParentRunID string `json:"parent_run_id,omitempty"`
	// ShardIndex is the 0-based index of this run within the parent's
	// shard set. Only meaningful when ParentRunID is set.
	ShardIndex int `json:"shard_index,omitempty"`
	// ShardCount is the total number of shards the parent split its
	// work into. Only meaningful when ParentRunID is set.
	ShardCount int `json:"shard_count,omitempty"`
	// ShardLabel is an optional human-friendly tag for the shard
	// (e.g. "files 100-119" or "ecosystem:npm"). Display-only.
	ShardLabel string `json:"shard_label,omitempty"`
	// CallbackURL, CallbackToken, CallbackAnswerNode carry the
	// run-completion webhook parameters (see pkg/notify) across the
	// queue so the runner pod that executes the run knows where to POST
	// the terminal-state callback and what correlation token to echo.
	// Empty for runs launched without a callback (the common case).
	CallbackURL        string `json:"callback_url,omitempty"`
	CallbackToken      string `json:"callback_token,omitempty"`
	CallbackAnswerNode string `json:"callback_answer_node,omitempty"`
}

// Contributions is the wire mirror of runtime.Contributions: the plugin
// markdown files and skill-library skills the launching instance resolved from
// ITS iterion home, shipped so the runner pod can mirror the same set into the
// workspace. Sizes are small (markdown, a few KB each) and capped at publish
// time — see cloudpublisher.
type Contributions struct {
	Plugin  []ContributionFile `json:"plugin,omitempty"`
	Library []LibrarySkillFile `json:"library,omitempty"`
}

// ContributionFile is one plugin markdown file bound for
// <workspace>/.claude/<kind>/<name>.
type ContributionFile struct {
	// Kind is the .claude/ leaf dir: "skills" | "commands" | "agents".
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Content []byte `json:"content"`
}

// LibrarySkillFile is one resolved skill-library skill. Description carries the
// frontmatter description so the runner reproduces the "## Skills" prompt hint
// without the store it does not have.
type LibrarySkillFile struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Content     []byte `json:"content"`
}

// BudgetOverrides is the wire mirror of ir.BudgetOverrides (kept local so
// this schema package stays dependency-free). Each field uses the
// "non-zero wins, zero inherits" convention; an all-zero value must be
// published as a nil pointer, not an empty object.
type BudgetOverrides struct {
	MaxCostUSD          float64 `json:"max_cost_usd,omitempty"`
	MaxTokens           int     `json:"max_tokens,omitempty"`
	MaxDuration         string  `json:"max_duration,omitempty"`
	MaxIterations       int     `json:"max_iterations,omitempty"`
	MaxParallelBranches int     `json:"max_parallel_branches,omitempty"`
}

// ModelOverride is one launch-time retargeting rule: every LLM node matching
// Selector (a node id, an id glob like "reviewer_*", or a node kind keyword
// such as "agent"/"judge") runs on the named backend/model/provider/effort
// instead of its DSL value. The wire mirror of store.RunModelOverride, kept
// local so this schema package stays dependency-free.
//
// Empty fields inherit: a rule carrying only Model retargets the model and
// leaves the node's backend alone.
type ModelOverride struct {
	Selector string `json:"selector"`
	Backend  string `json:"backend,omitempty"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	Effort   string `json:"effort,omitempty"`
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
// "claw" is in-process; every other value forks an external agent CLI.
//
// The set must stay in sync with delegate's registration names: a backend
// missing here cannot be expressed as a queued run's default, so a cloud
// run silently falls back to claw. That is how kimi and grok were
// unreachable in cloud mode until pi's arrival surfaced the gap.
type Backend string

const (
	BackendClaw       Backend = "claw"
	BackendClaudeCode Backend = "claude_code"
	BackendCodex      Backend = "codex"
	BackendKimi       Backend = "kimi"
	BackendGrok       Backend = "grok"
	BackendPi         Backend = "pi"
)

// BackendConfig carries the LLM backend selection per run.
type BackendConfig struct {
	Default       Backend `json:"default"`
	DelegateModel string  `json:"delegate_model,omitempty"`
}

// ResumeSpec is non-nil for resume publishes; the runner threads its
// fields into `runtime.Engine.Resume`.
type ResumeSpec struct {
	Answers map[string]any `json:"answers,omitempty"`
	Force   bool           `json:"force"`
}

// TraceContext propagates the originating studio span across NATS so
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
