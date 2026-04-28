// Package trust implements centralized trust-prompt detection and resolution.
//
// When a delegated subprocess (e.g. claude_code, codex) asks the user to
// approve a workspace, the TrustResolver evaluates the working directory
// against pre-configured allowlisted/denied roots and emits structured events
// describing the decision.
package trust

import (
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/strutil"
	"path/filepath"
	"strings"
)

// trustPromptCues are the known case-insensitive phrases that indicate a trust
// prompt is being shown in the subprocess screen output.
var trustPromptCues = []string{
	"do you trust the files in this folder",
	"trust the files in this folder",
	"trust this folder",
	"allow and continue",
	"yes, proceed",
}

// ---------------------------------------------------------------------------
// TrustPolicy
// ---------------------------------------------------------------------------

// TrustPolicy describes the resolved trust posture for a working directory.
type TrustPolicy int

const (
	AutoTrust       TrustPolicy = iota // Pre-approved — auto-accept the prompt.
	RequireApproval                    // Unknown directory — require human approval.
	Deny                               // Explicitly denied — reject the prompt.
)

var trustPolicyNames = [...]string{"auto_trust", "require_approval", "deny"}

func (p TrustPolicy) String() string {
	if int(p) < len(trustPolicyNames) {
		return trustPolicyNames[p]
	}
	return fmt.Sprintf("TrustPolicy(%d)", int(p))
}

func (p TrustPolicy) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.String())
}

func (p *TrustPolicy) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	for i, name := range trustPolicyNames {
		if name == s {
			*p = TrustPolicy(i)
			return nil
		}
	}
	return fmt.Errorf("unknown TrustPolicy %q", s)
}

// ---------------------------------------------------------------------------
// TrustEventKind
// ---------------------------------------------------------------------------

// TrustEventKind discriminates the variant of a TrustEvent.
type TrustEventKind int

const (
	TrustRequired TrustEventKind = iota
	TrustResolved
	TrustDenied
)

var trustEventKindNames = [...]string{"trust_required", "trust_resolved", "trust_denied"}

func (k TrustEventKind) String() string {
	if int(k) < len(trustEventKindNames) {
		return trustEventKindNames[k]
	}
	return fmt.Sprintf("TrustEventKind(%d)", int(k))
}

// ---------------------------------------------------------------------------
// TrustEvent
// ---------------------------------------------------------------------------

// TrustEvent records a single trust-related observation during resolution.
type TrustEvent struct {
	Kind   TrustEventKind `json:"kind"`
	Cwd    string         `json:"cwd"`
	Policy *TrustPolicy   `json:"policy,omitempty"` // Set for TrustResolved.
	Reason string         `json:"reason,omitempty"` // Set for TrustDenied.
}

// ---------------------------------------------------------------------------
// TrustConfig
// ---------------------------------------------------------------------------

// TrustConfig holds allowlisted and denied path roots for trust evaluation.
type TrustConfig struct {
	allowlisted []string
	denied      []string
}

// NewTrustConfig returns an empty TrustConfig.
func NewTrustConfig() *TrustConfig {
	return &TrustConfig{}
}

// WithAllowlisted adds a path to the allowlist and returns the config for
// chaining.
func (c *TrustConfig) WithAllowlisted(path string) *TrustConfig {
	c.allowlisted = append(c.allowlisted, path)
	return c
}

// WithDenied adds a path to the deny list and returns the config for chaining.
func (c *TrustConfig) WithDenied(path string) *TrustConfig {
	c.denied = append(c.denied, path)
	return c
}

// ---------------------------------------------------------------------------
// TrustDecision
// ---------------------------------------------------------------------------

// TrustDecision is the output of TrustResolver.Resolve.
type TrustDecision struct {
	required bool
	policy   TrustPolicy
	events   []TrustEvent
}

// NotRequired returns a TrustDecision indicating no trust prompt was detected.
func NotRequired() TrustDecision {
	return TrustDecision{required: false}
}

// Required returns a TrustDecision with the given policy and events.
func Required(policy TrustPolicy, events []TrustEvent) TrustDecision {
	return TrustDecision{required: true, policy: policy, events: events}
}

// IsRequired returns true if the decision carries a trust policy.
func (d TrustDecision) IsRequired() bool { return d.required }

// Policy returns the resolved trust policy, or nil if not required.
func (d TrustDecision) Policy() *TrustPolicy {
	if !d.required {
		return nil
	}
	p := d.policy
	return &p
}

// Events returns the trust events collected during resolution.
func (d TrustDecision) Events() []TrustEvent {
	if !d.required {
		return nil
	}
	return d.events
}

// ---------------------------------------------------------------------------
// TrustResolver
// ---------------------------------------------------------------------------

// TrustResolver evaluates subprocess screen output for trust prompts and
// resolves them against a pre-configured set of allowed/denied roots.
type TrustResolver struct {
	config *TrustConfig
}

// NewTrustResolver creates a TrustResolver from the given config.
func NewTrustResolver(config *TrustConfig) *TrustResolver {
	return &TrustResolver{config: config}
}

// Resolve inspects screenText for trust prompt cues and returns a decision
// based on the working directory's relationship to configured roots.
//
// Resolution order:
//  1. If no trust prompt detected → NotRequired.
//  2. If cwd matches any denied root → Deny (takes precedence over allowlist).
//  3. If cwd matches any allowlisted root → AutoTrust.
//  4. Otherwise → RequireApproval.
func (r *TrustResolver) Resolve(cwd, screenText string) TrustDecision {
	if !DetectTrustPrompt(screenText) {
		return NotRequired()
	}

	events := []TrustEvent{{Kind: TrustRequired, Cwd: cwd}}

	// Denied roots take precedence.
	for _, root := range r.config.denied {
		if pathMatches(cwd, root) {
			reason := fmt.Sprintf("cwd matches denied trust root: %s", root)
			events = append(events, TrustEvent{
				Kind:   TrustDenied,
				Cwd:    cwd,
				Reason: reason,
			})
			return Required(Deny, events)
		}
	}

	// Allowlisted roots.
	for _, root := range r.config.allowlisted {
		if pathMatches(cwd, root) {
			policy := AutoTrust
			events = append(events, TrustEvent{
				Kind:   TrustResolved,
				Cwd:    cwd,
				Policy: &policy,
			})
			return Required(AutoTrust, events)
		}
	}

	return Required(RequireApproval, events)
}

// Trusts returns true if the cwd is allowlisted and not denied.
func (r *TrustResolver) Trusts(cwd string) bool {
	for _, root := range r.config.denied {
		if pathMatches(cwd, root) {
			return false
		}
	}
	for _, root := range r.config.allowlisted {
		if pathMatches(cwd, root) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Exported helpers
// ---------------------------------------------------------------------------

// DetectTrustPrompt returns true if the screen text contains any known trust
// prompt cue (case-insensitive, ASCII-only lowering).
func DetectTrustPrompt(screenText string) bool {
	lowered := strutil.ASCIIToLower(screenText)
	for _, cue := range trustPromptCues {
		if strings.Contains(lowered, cue) {
			return true
		}
	}
	return false
}

// PathMatchesTrustedRoot checks whether cwd falls under trustedRoot using
// component-boundary matching.
func PathMatchesTrustedRoot(cwd, trustedRoot string) bool {
	return pathMatches(cwd, trustedRoot)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// pathMatches returns true if candidate equals root or is a subdirectory of
// root. Uses component-boundary matching: /tmp/worktrees-other does NOT match
// /tmp/worktrees.
func pathMatches(candidate, root string) bool {
	candidate = normalizePath(candidate)
	root = normalizePath(root)
	if candidate == root {
		return true
	}
	// Component-boundary: candidate must start with root + "/"
	return strings.HasPrefix(candidate, root+"/")
}

// normalizePath cleans and resolves a path to absolute form.
// Unlike Rust's canonicalize, this does not resolve symlinks.
func normalizePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}
