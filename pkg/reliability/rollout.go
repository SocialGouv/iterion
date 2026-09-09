// Package reliability centralises the staged rollout knobs and the small
// compatibility/baseline reports operators need while enabling workflow
// reliability contracts. It is intentionally read-only: generating a report
// never rewrites a legacy run or silently upgrades its policy.
package reliability

import (
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/retrycoord"
	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	EnvMode = "ITERION_RELIABILITY_MODE"
	// EnvContextPolicyAlias is the narrower, pre-existing switch this rollout
	// generalises. It stays readable so a deployment that already carries it
	// keeps working, but EnvMode is AUTHORITATIVE: the documented rollback
	// (EnvMode=legacy) has to take effect on exactly those hosts, and a lever
	// the older variable can silently shadow is not an emergency lever.
	EnvContextPolicyAlias      = "ITERION_EXECUTION_CONTEXT_POLICY"
	ModeLegacy            Mode = "legacy"
	ModeReport            Mode = "report"
	ModeEnforce           Mode = "enforce"
)

// Mode controls the compatibility gate during rollout. Legacy is the safe
// default: old runs remain readable and no admission policy is inferred from
// the new fields.
type Mode string

// ModeFromEnv resolves the ONE rollout mode every surface must agree on.
// EnvMode decides whenever it is set — including to an unrecognised value,
// which resolves to legacy, the safe end of the dial, rather than falling
// through to a variable the operator did not just touch. EnvContextPolicyAlias
// is consulted only when EnvMode is unset.
func ModeFromEnv() Mode {
	raw := strings.TrimSpace(os.Getenv(EnvMode))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv(EnvContextPolicyAlias))
	}
	switch mode := Mode(strings.ToLower(raw)); mode {
	case ModeReport, ModeEnforce:
		return mode
	default:
		return ModeLegacy
	}
}

// ContextPolicyFromEnv is the single resolution the launch surfaces share —
// runview.ExecutionContextPolicyFromEnv delegates here. One implementation is
// what keeps a compatibility report from naming a policy the launches do not
// actually apply.
func ContextPolicyFromEnv() store.ContextPolicy {
	return ModeFromEnv().ContextPolicy()
}

// ContextPolicy maps the rollout mode onto the persisted execution-context
// policy. The two vocabularies are deliberately identical.
func (m Mode) ContextPolicy() store.ContextPolicy {
	switch m {
	case ModeReport:
		return store.ContextPolicyReport
	case ModeEnforce:
		return store.ContextPolicyEnforce
	default:
		return store.ContextPolicyLegacy
	}
}

// Config is the operator-facing rollout configuration. The retry circuit's
// threshold/cooldown are read by pkg/retrycoord; they are repeated here only
// so a report can print one coherent pilot configuration.
//
// Mode and the retry-circuit pair are knobs an operator can actually turn.
// WatcherCursorsEnabled is not one: FromEnv sets it to a constant, and whether
// a coordinator persists its cursor is in fact decided per launch surface
// (Coordinator.cursorStore) rather than by any deployment-wide setting — which
// is why `iterion reliability report` answers that question per run, from the
// cursors a run actually wrote, instead of printing this field.
//
// The output correction budget deliberately is not a field here at all:
// correction fires only for an
// engine built with runtime.WithOutputValidation AND an executor implementing
// runtime.OutputCorrector, and neither exists on a production path today
// (the sole corrector in the tree is a test double, and the production
// ClawExecutor validates and retries upstream of the engine's optional path).
// An env var for it would read as an emergency lever and do nothing, which is
// worse than no lever at all — runtime.WithOutputCorrectionBudget stays the
// explicit API for a custom or test engine.
type Config struct {
	Mode                  Mode
	RetryCircuitThreshold int
	RetryCircuitCooldown  time.Duration
	WatcherCursorsEnabled bool
}

func FromEnv() Config {
	circuit := retrycoord.FromEnv()
	return Config{Mode: ModeFromEnv(), RetryCircuitThreshold: circuit.Threshold, RetryCircuitCooldown: circuit.Cooldown, WatcherCursorsEnabled: true}
}

func (c Config) ContextPolicy() store.ContextPolicy { return c.Mode.ContextPolicy() }

// CompatibilityReport is a safe, read-only projection of one run's rollout
// state. The booleans are deliberately explicit so an operator can decide to
// roll a pilot back without parsing opaque JSON or event prose.
type CompatibilityReport struct {
	RunID             string `json:"run_id"`
	WorkflowHash      string `json:"workflow_hash,omitempty"`
	ContextVersion    int    `json:"context_version,omitempty"`
	ContextPolicy     string `json:"context_policy"`
	LegacyContext     bool   `json:"legacy_context"`
	AdmissionRecorded bool   `json:"admission_recorded"`
	// PublishingNodeCount is the number of DISTINCT nodes the run's artifact
	// index records as having published — not a count of artifacts. The index
	// is a node_id → latest-version map (store.Run.ArtifactIndex), so one node
	// publishing versions 0, 1 and 2 contributes 1, and it is a cache the
	// store may legitimately leave incomplete (see FilesystemRunStore's
	// ErrRunNotFound branch). Distinct publishers is the number the index can
	// actually back, and the one that answers the promote-vs-rollback
	// question: did the same nodes publish under the new contract?
	PublishingNodeCount    int  `json:"publishing_node_count"`
	CorrectionEpisodeCount int  `json:"correction_episode_count"`
	WatcherCursorCount     int  `json:"watcher_cursor_count"`
	RollbackSafe           bool `json:"rollback_safe"`
}

func ReportForRun(run *store.Run) CompatibilityReport {
	r := CompatibilityReport{ContextPolicy: string(store.ContextPolicyLegacy)}
	if run == nil {
		return r
	}
	r.RollbackSafe = true
	r.RunID = run.ID
	r.WorkflowHash = run.WorkflowHash
	r.AdmissionRecorded = run.Admission != nil
	r.PublishingNodeCount = len(run.ArtifactIndex)
	r.CorrectionEpisodeCount = len(run.OutputCorrections)
	r.WatcherCursorCount = len(run.WatcherCursors)
	if run.ExecutionContext == nil {
		r.LegacyContext = true
	} else {
		r.ContextVersion = run.ExecutionContext.Version
		r.ContextPolicy = string(run.ExecutionContext.Policy)
		if run.ExecutionContext.Policy == store.ContextPolicyEnforce {
			r.RollbackSafe = false
		}
	}
	for _, episode := range run.OutputCorrections {
		if !correctionEpisodeSettled(episode) {
			r.RollbackSafe = false
			break
		}
	}
	return r
}

// correctionSucceeded is the one episode status that leaves nothing for a
// rollback to strand: the output was repaired and published. The vocabulary
// is store.OutputCorrectionEpisode.Status, whose values are spelled out in
// pkg/runtime/node_output.go (active | succeeded | exhausted | unchanged) —
// unexported there, so this is a literal by necessity, not by choice.
const correctionSucceeded = "succeeded"

// correctionEpisodeSettled decides whether one episode leaves the run safe to
// roll back. The SAFE set is the closed one, deliberately: enumerating the
// UNSAFE statuses is what made "unchanged" — the runtime's no-progress stop,
// which it groups with "exhausted" in its own terminated-episode guard — read
// as green, on the very artifact an operator uses to decide promote vs
// rollback. It is the mistake the rollout doc names one level up: a missing
// or unrecognised field means pre-pilot, never successful. An episode status
// this package has not seen (a future state, or an empty one on a partially
// written ledger) is therefore unsafe, not safe.
func correctionEpisodeSettled(episode store.OutputCorrectionEpisode) bool {
	return episode.Status == correctionSucceeded
}

// Baseline summarises the current fleet without changing any run. It is the
// before/after artifact for a pilot: failed and resumable rates, plus counts
// showing how many runs actually exercised the new ledgers.
type Baseline struct {
	Total                  int `json:"total"`
	Finished               int `json:"finished"`
	Failed                 int `json:"failed"`
	FailedResumable        int `json:"failed_resumable"`
	Paused                 int `json:"paused"`
	Queued                 int `json:"queued"`
	RetryArmed             int `json:"retry_armed"`
	RunsWithCorrections    int `json:"runs_with_corrections"`
	RunsWithWatcherCursors int `json:"runs_with_watcher_cursors"`
}

func Summarize(runs []*store.Run) Baseline {
	var b Baseline
	for _, run := range runs {
		if run == nil {
			continue
		}
		b.Total++
		switch run.Status {
		case store.RunStatusFinished:
			b.Finished++
		case store.RunStatusFailed:
			b.Failed++
		case store.RunStatusFailedResumable:
			b.FailedResumable++
		case store.RunStatusQueued:
			b.Queued++
		}
		// The paused bucket asks the store's own contract rather than
		// re-listing the two paused statuses: a hand-rolled set here is the
		// drift ADR-095's negative-space guard exists to catch, and it would
		// go unnoticed because that sweep walks a fixed package list this
		// one is not on.
		if run.Status.IsPaused() {
			b.Paused++
		}
		if run.RetryState != nil && run.RetryState.RetryAfter != nil {
			b.RetryArmed++
		}
		if len(run.OutputCorrections) > 0 {
			b.RunsWithCorrections++
		}
		if len(run.WatcherCursors) > 0 {
			b.RunsWithWatcherCursors++
		}
	}
	return b
}

// RollbackPlan describes the non-destructive rollback contract. Rollback
// disables new enforcement but retains evidence already written, so a pilot
// can be stopped without deleting run state or making a resume ambiguous.
type RollbackPlan struct {
	DisableEnforcement bool     `json:"disable_enforcement"`
	PreserveEvidence   bool     `json:"preserve_evidence"`
	Actions            []string `json:"actions"`
}

func (c Config) Rollback() RollbackPlan {
	return RollbackPlan{
		DisableEnforcement: true,
		PreserveEvidence:   true,
		Actions: []string{
			"set " + EnvMode + "=legacy (authoritative; it overrides " + EnvContextPolicyAlias + ", which does not need unsetting)",
			"keep execution_context, admission, correction and watcher ledgers for audit",
		},
	}
}
