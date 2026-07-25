// Package rewrite is iterion's command-output-rewriter extension point: the
// generalization of what used to be the hardcoded rtk integration. A rewriter
// is an external binary that rewrites a dev command into a token-compressed
// equivalent (e.g. "git status" → "rtk git status"). Rewriters are declared by
// plugins (plugin.RewriterSpec) and applied on iterion's three shell surfaces —
// the claude_code Bash hook, the claw bash builtin, and tool nodes — as an
// ordered Chain so several compressors can compose.
//
// The behavioural contract is exactly the proven rtk one, now driven by the
// spec instead of hardcoded: locate the binary (env → PATH → conventional
// paths), invoke `<bin> <argv with {{command}} substituted>` with a bounded
// timeout, take stdout as the rewrite when the exit code is in apply_exit_codes
// and differs from the input, and apply the per-mode transform (e.g. inject
// --ultra-compact). Any failure (binary absent, timeout, non-apply exit, empty
// or identical output) falls back to the original command and never errors.
package rewrite

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/plugin"
)

// ModeEnv sets the process-wide default compression mode (on|ultra|off) used
// when neither a node, the workflow, nor a run override sets one.
const ModeEnv = "ITERION_COMPRESS"

// DefaultRewriteTimeoutMs bounds a single rewriter invocation when the spec
// does not set one. Generous so a wedged binary can never stall a node.
const DefaultRewriteTimeoutMs = 5000

// Mode is the resolved compression level for a node or run.
type Mode int

const (
	// Off disables compression (the default).
	Off Mode = iota
	// On rewrites commands to their compressed equivalent.
	On
	// Ultra requests the rewriter's densest output (per-spec inject_flag).
	Ultra
)

func (m Mode) String() string {
	switch m {
	case On:
		return "on"
	case Ultra:
		return "ultra"
	default:
		return "off"
	}
}

// Enabled reports whether the mode performs any rewriting.
func (m Mode) Enabled() bool { return m == On || m == Ultra }

// ParseMode maps a DSL/env/CLI string to a Mode (case-insensitive). Canonical
// values are on|off|ultra; true/1 are accepted for env/CLI ergonomics.
func ParseMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "1":
		return On
	case "ultra":
		return Ultra
	default:
		return Off
	}
}

// ResolveWithDefault picks the effective mode from iterion's precedence chain,
// highest priority first: run override (CLI --compress / studio) > node DSL >
// workflow DSL > ITERION_COMPRESS env default > an explicit fallback def used
// when every level is unset. It lets compression be opt-OUT on LLM (agent/
// judge) nodes — default On when a rewriter plugin is enabled and its binary
// is present — while an explicit "off" at any precedence level (run override,
// node, workflow, or the ITERION_COMPRESS env) still wins. First non-empty
// level wins; all empty → def.
func ResolveWithDefault(override, node, workflow, envDefault string, def Mode) Mode {
	m, _ := ResolveWithDefaultSourced(override, node, workflow, envDefault, def)
	return m
}

// ResolveWithDefaultSourced is ResolveWithDefault plus the winning
// precedence level ("run_override" | "node" | "workflow" | "env" |
// "default") — the studio's settings-provenance caption reads it so an
// operator can see WHY a knob is what it is (e.g. an env var set but
// surclassed).
func ResolveWithDefaultSourced(override, node, workflow, envDefault string, def Mode) (Mode, string) {
	levels := []struct {
		value  string
		source string
	}{
		{override, "run_override"},
		{node, "node"},
		{workflow, "workflow"},
		{envDefault, "env"},
	}
	for _, l := range levels {
		if strings.TrimSpace(l.value) != "" {
			return ParseMode(l.value), l.source
		}
	}
	return def, "default"
}

// ResolveToolNode is the compression mode for a tool node. Tool-node output is
// often consumed deterministically (a review loop's `git diff` feeding a
// reviewer), so compression must be a deliberate per-node choice: a tool node
// compresses ONLY when its own `compress:` field is on/ultra. A run-level
// override can force-DISABLE everything (kill switch) but never force-ENABLE a
// tool node. Workflow/env defaults are intentionally ignored here.
func ResolveToolNode(override, node string) Mode {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "off", "false", "0":
		return Off
	}
	return ParseMode(node)
}

// Rewriter wraps a single plugin.RewriterSpec with the locate + invoke logic.
// The binary path is resolved once at construction — locating it per command
// (once per shell call in a node's tool loop) would re-walk PATH needlessly.
type Rewriter struct {
	spec    plugin.RewriterSpec
	binPath string // resolved at NewRewriter; "" when the binary is absent
}

// NewRewriter builds a Rewriter from a spec, resolving its binary once.
func NewRewriter(spec plugin.RewriterSpec) *Rewriter {
	return &Rewriter{spec: spec, binPath: locate(spec.Locate)}
}

// ID returns the rewriter's id.
func (r *Rewriter) ID() string { return r.spec.ID }

// SandboxMount returns the in-container path the binary should be bind-mounted
// to for sandboxed runs, or "" when the rewriter declares none.
func (r *Rewriter) SandboxMount() string { return r.spec.SandboxMount }

// Locate returns the resolved binary path (cached at construction), or "".
func (r *Rewriter) Locate() string { return r.binPath }

// Available reports whether this rewriter's binary was located.
func (r *Rewriter) Available() bool { return r.binPath != "" }

// locate resolves a binary path: locate.env, then PATH (locate.bin), then the
// conventional locate.paths (with ~ expansion). Returns "" when not found.
func locate(loc plugin.LocateSpec) string {
	if loc.Env != "" {
		if v := strings.TrimSpace(os.Getenv(loc.Env)); v != "" && isExecutableFile(v) {
			return v
		}
	}
	if loc.Bin != "" {
		if p, err := exec.LookPath(loc.Bin); err == nil {
			return p
		}
	}
	for _, c := range loc.Paths {
		if e := expandHome(c); e != "" && isExecutableFile(e) {
			return e
		}
	}
	return ""
}

// Rewrite returns the rewriter's compressed equivalent of cmd and true when it
// produced a usable rewrite differing from cmd; otherwise cmd and false. It
// never errors — every failure falls back to the original command.
func (r *Rewriter) Rewrite(ctx context.Context, m Mode, cmd string) (string, bool) {
	if !m.Enabled() {
		return cmd, false
	}
	bin := r.Locate()
	if bin == "" {
		return cmd, false
	}
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return cmd, false
	}

	timeout := time.Duration(r.spec.Invoke.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = DefaultRewriteTimeoutMs * time.Millisecond
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := make([]string, 0, len(r.spec.Invoke.Argv))
	for _, a := range r.spec.Invoke.Argv {
		args = append(args, strings.ReplaceAll(a, plugin.CommandPlaceholder, cmd))
	}
	c := exec.CommandContext(rctx, bin, args...)
	// Inherit the process env first so an operator who sets a rewriter's own
	// vars (e.g. re-enabling telemetry) still wins, then apply the spec env.
	c.Env = os.Environ()
	for k, v := range r.spec.Invoke.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	var stdout bytes.Buffer
	c.Stdout = &stdout
	// Stderr → /dev/null: rewriters print diagnostics there.

	err := c.Run()
	if rctx.Err() != nil {
		return cmd, false // timeout / cancellation → passthrough
	}
	if !slices.Contains(r.spec.ApplyExitCodesOrDefault(), exitCode(err)) {
		return cmd, false
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" || out == trimmed {
		return cmd, false
	}
	out = applyModeTransform(out, r.spec.Invoke.Modes, m)
	return out, true
}

// applyModeTransform applies the per-mode inject_flag (if any) to a successful
// rewrite by inserting the flag right after the binary name (first token).
func applyModeTransform(out string, modes map[string]plugin.ModeSpec, m Mode) string {
	ms, ok := modes[m.String()]
	if !ok || strings.TrimSpace(ms.InjectFlag) == "" {
		return out
	}
	flag := ms.InjectFlag
	if strings.Contains(out, flag) {
		return out
	}
	parts := strings.SplitN(out, " ", 2)
	if len(parts) == 1 {
		return parts[0] + " " + flag
	}
	return parts[0] + " " + flag + " " + parts[1]
}

// Chain is an ordered set of rewriters applied in sequence — the composed
// compression of all enabled rewriter plugins. A single-rewriter chain is
// behaviourally identical to the old single rtk integration.
type Chain struct {
	rewriters []*Rewriter
}

// NewChain builds a Chain from an ordered list of specs.
func NewChain(specs []plugin.RewriterSpec) *Chain {
	c := &Chain{}
	for _, s := range specs {
		c.rewriters = append(c.rewriters, NewRewriter(s))
	}
	return c
}

// Specs returns the underlying specs (used to carry the chain across a Task).
func (c *Chain) Specs() []plugin.RewriterSpec {
	if c == nil {
		return nil
	}
	out := make([]plugin.RewriterSpec, 0, len(c.rewriters))
	for _, r := range c.rewriters {
		out = append(out, r.spec)
	}
	return out
}

// Available reports whether the chain has at least one locatable rewriter.
func (c *Chain) Available() bool {
	if c == nil {
		return false
	}
	for _, r := range c.rewriters {
		if r.Available() {
			return true
		}
	}
	return false
}

// Rewrite applies each rewriter in order, threading the output of one into the
// next. Returns the final command and whether any rewriter changed it.
func (c *Chain) Rewrite(ctx context.Context, m Mode, cmd string) (string, bool) {
	if c == nil || !m.Enabled() {
		return cmd, false
	}
	cur := cmd
	changed := false
	for _, r := range c.rewriters {
		if next, ok := r.Rewrite(ctx, m, cur); ok {
			cur = next
			changed = true
		}
	}
	return cur, changed
}

// RewriteCommandField rewrites the "command" field of a tool-input map via the
// chain. On a successful rewrite it returns a shallow copy of the map (the
// caller's map is never mutated — PreToolUse hooks must not touch caller state)
// with the new command, plus true; otherwise the input unchanged and false.
func (c *Chain) RewriteCommandField(ctx context.Context, m Mode, input map[string]any) (map[string]any, bool) {
	if c == nil || !m.Enabled() {
		return input, false
	}
	cmd, ok := input["command"].(string)
	if !ok || cmd == "" {
		return input, false
	}
	rewritten, changed := c.Rewrite(ctx, m, cmd)
	if !changed {
		return input, false
	}
	out := maps.Clone(input)
	out["command"] = rewritten
	return out, true
}

// SandboxMount is a host→container bind-mount a rewriter requires.
type SandboxMount struct {
	HostPath      string
	ContainerPath string
}

// SandboxMounts returns the bind-mounts for every available rewriter that
// declares one, so the sandbox driver can mount the host binary in-container.
func (c *Chain) SandboxMounts() []SandboxMount {
	if c == nil {
		return nil
	}
	var out []SandboxMount
	for _, r := range c.rewriters {
		dest := r.SandboxMount()
		if dest == "" {
			continue
		}
		host := r.Locate()
		if host == "" {
			continue
		}
		out = append(out, SandboxMount{HostPath: host, ContainerPath: dest})
	}
	return out
}

// --- context carriers (claw bash builtin) ---

type ctxKeyMode struct{}
type ctxKeyChain struct{}

// WithMode returns a context carrying the resolved compression mode.
func WithMode(ctx context.Context, m Mode) context.Context {
	return context.WithValue(ctx, ctxKeyMode{}, m)
}

// ModeFromContext returns the mode stored by WithMode, or Off.
func ModeFromContext(ctx context.Context) Mode {
	if m, ok := ctx.Value(ctxKeyMode{}).(Mode); ok {
		return m
	}
	return Off
}

// WithChain returns a context carrying the active rewriter chain.
func WithChain(ctx context.Context, c *Chain) context.Context {
	return context.WithValue(ctx, ctxKeyChain{}, c)
}

// ChainFromContext returns the chain stored by WithChain, or nil.
func ChainFromContext(ctx context.Context) *Chain {
	if c, ok := ctx.Value(ctxKeyChain{}).(*Chain); ok {
		return c
	}
	return nil
}

// RewriteCommandFieldCtx rewrites a tool-input map using the chain and mode
// carried on the context. Convenience for the claw bash builtin, which has no
// direct handle to either.
func RewriteCommandFieldCtx(ctx context.Context, input map[string]any) (map[string]any, bool) {
	return ChainFromContext(ctx).RewriteCommandField(ctx, ModeFromContext(ctx), input)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func isExecutableFile(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
