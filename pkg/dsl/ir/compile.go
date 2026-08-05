package ir

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// ---------------------------------------------------------------------------
// Compiler diagnostics
// ---------------------------------------------------------------------------

// DiagCode identifies the kind of compilation diagnostic.
type DiagCode string

const (
	DiagUnknownNode           DiagCode = "C001" // edge references unknown node
	DiagUnknownSchema         DiagCode = "C002" // node references unknown schema
	DiagUnknownPrompt         DiagCode = "C003" // node references unknown prompt
	DiagBadTemplateRef        DiagCode = "C004" // malformed template reference
	DiagDuplicateLoop         DiagCode = "C005" // conflicting loop definitions
	DiagNoWorkflow            DiagCode = "C006" // no workflow found in file
	DiagMultipleWorkflow      DiagCode = "C007" // multiple workflows (unsupported in V1)
	DiagMissingEntry          DiagCode = "C008" // entry node not found
	DiagMissingModelOrBackend DiagCode = "C018" // agent/judge has neither model nor backend
	DiagDuplicateMCPServer    DiagCode = "C024" // duplicate top-level mcp_server name
	DiagInvalidMCPServer      DiagCode = "C025" // invalid MCP server config
	DiagCodexDiscouraged      DiagCode = "C030" // codex backend is supported but discouraged
	DiagComputeNoExpr         DiagCode = "C039" // compute node has no expressions
	DiagBadExpr               DiagCode = "C040" // expression failed to parse
	DiagDuplicateNodeID       DiagCode = "C041" // two declarations share a node ID
	DiagReservedNodeName      DiagCode = "C042" // user node uses reserved name (done/fail)
	DiagInvalidSandboxMode    DiagCode = "C044" // sandbox mode value is not one of "", none, auto
	DiagSandboxAutoNoConfig   DiagCode = "C045" // sandbox: auto requested but no .devcontainer/devcontainer.json found
	DiagBudgetCostInvalid     DiagCode = "C046" // budget.max_cost_usd negative, NaN or Inf
	DiagResourceCapInvalid    DiagCode = "C194" // resources.<name> capacity ≤ 0
)

// codexBackendName is the literal value of the discouraged backend.
// Hardcoded here (rather than imported from delegate/) to avoid an ir → delegate
// dependency, which the package layout intentionally forbids.
const codexBackendName = "codex"

// Severity indicates the severity of a diagnostic.
type Severity int

const (
	SeverityError Severity = iota
	SeverityWarning
)

func (s Severity) String() string {
	if s == SeverityWarning {
		return "warning"
	}
	return "error"
}

// Diagnostic represents a compilation error or warning.
//
// NodeID and EdgeID are best-effort attribution fields used by tooling (the
// studio renders them as inline badges). They may be empty when the diagnostic
// is global (e.g. "no workflow"). EdgeID follows the canonical "<from>-><to>"
// format the studio uses; when multiple edges share endpoints the first
// matching one wins.
//
// Hint is a one-line, user-facing fix suggestion when one is known. The
// authoritative documentation still lives in `docs/diagnostics.md`; Hint is
// for UIs that want a quick tooltip without round-tripping to docs.
type Diagnostic struct {
	Code     DiagCode
	Severity Severity
	Message  string
	NodeID   string
	EdgeID   string
	Hint     string
}

func (d Diagnostic) Error() string {
	return fmt.Sprintf("%s [%s]: %s", d.Severity, d.Code, d.Message)
}

// ---------------------------------------------------------------------------
// CompileResult
// ---------------------------------------------------------------------------

// CompileResult holds the compiled IR workflow and any diagnostics.
type CompileResult struct {
	Workflow    *Workflow
	Diagnostics []Diagnostic
}

// HasErrors returns true if any diagnostic is an error.
func (r *CompileResult) HasErrors() bool {
	for _, d := range r.Diagnostics {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Compiler
// ---------------------------------------------------------------------------

// compiler holds state during compilation.
type compiler struct {
	file    *ast.File
	diags   []Diagnostic
	nodes   map[string]Node
	schemas map[string]*Schema
	prompts map[string]*Prompt
	mcp     map[string]*MCPServer

	autoBackendOnce   sync.Once
	autoBackendCached bool
}

// workflowInteractionDefault returns the workflow-level interaction default,
// or InteractionNone if none is set.
func (c *compiler) workflowInteractionDefault() InteractionMode {
	if len(c.file.Workflows) > 0 {
		wf := c.file.Workflows[0]
		if wf.Interaction != nil {
			return *wf.Interaction
		}
	}
	return InteractionNone
}

func (c *compiler) errorf(code DiagCode, format string, args ...any) {
	c.diags = append(c.diags, Diagnostic{
		Code:     code,
		Severity: SeverityError,
		Message:  fmt.Sprintf(format, args...),
	})
}

func (c *compiler) warnf(code DiagCode, format string, args ...any) {
	c.diags = append(c.diags, Diagnostic{
		Code:     code,
		Severity: SeverityWarning,
		Message:  fmt.Sprintf(format, args...),
	})
}

// errorfAt is a variant of errorf that attaches authoritative attribution
// (nodeID and/or edgeID) so downstream tooling can render the diagnostic on
// the precise node or edge instead of guessing from the message text.
func (c *compiler) errorfAt(code DiagCode, nodeID, edgeID string, format string, args ...any) {
	c.diags = append(c.diags, Diagnostic{
		Code:     code,
		Severity: SeverityError,
		Message:  fmt.Sprintf(format, args...),
		NodeID:   nodeID,
		EdgeID:   edgeID,
	})
}

// warnfAt is the warning counterpart to errorfAt.
func (c *compiler) warnfAt(code DiagCode, nodeID, edgeID string, format string, args ...any) {
	c.diags = append(c.diags, Diagnostic{
		Code:     code,
		Severity: SeverityWarning,
		Message:  fmt.Sprintf(format, args...),
		NodeID:   nodeID,
		EdgeID:   edgeID,
	})
}

// edgeID builds the canonical "<from>-><to>" identifier the studio uses so
// inline diagnostic badges can match attributed diagnostics to the right edge.
func edgeID(from, to string) string {
	return from + "->" + to
}

// warnCodexDiscouraged emits a C030 warning when a node uses the codex backend.
// Codex is still supported but has known limitations (cannot configure tool set,
// tends to fill its own context window, weaker iterion integration). New
// workflows should prefer 'claude_code' for tool-using agents or 'claw' with an
// OpenAI model (e.g. model: "openai/gpt-5.4-mini") for judges/reviewers.
func (c *compiler) warnCodexDiscouraged(kind, name, backend string) {
	if backend != codexBackendName {
		return
	}
	c.warnfAt(DiagCodexDiscouraged, name, "",
		"%s %q uses 'codex' backend which is supported but discouraged: codex cannot configure its tool set, tends to fill its context window, and has weaker integration; prefer 'claude_code' for tool-using agents or 'claw' with an OpenAI model (e.g. model: \"openai/gpt-5.4-mini\") for judges/reviewers",
		kind, name)
}

// compileSandboxBlock translates an AST SandboxBlock (which represents
// either the short form `sandbox: <ident>` or the full block form)
// into an IR SandboxSpec.
//
// Mode validity:
//
//   - ""      → returns (nil, no diagnostic) — caller treats this as
//     "inherit". Used by node-level blocks that only override
//     network rules without changing the activation mode.
//   - "none"  → opt-out, no body fields propagate
//   - "auto"  → reads .devcontainer/devcontainer.json at runtime;
//     body fields are accepted but only Network is meaningful
//   - "inline"→ body fields are the spec source
//
// Unknown modes raise DiagInvalidSandboxMode and the function returns
// nil so the rest of compilation proceeds.
//
// scope/name describe the surrounding declaration ("workflow main",
// "agent reviewer") and are used only in diagnostic messages.
func (c *compiler) compileSandboxBlock(blk *ast.SandboxBlock, scope, name string) *SandboxSpec {
	if blk == nil {
		return nil
	}
	switch blk.Mode {
	case "", "none", "auto", "inline":
	default:
		c.errorfAt(DiagInvalidSandboxMode, name, "",
			"%s %q has invalid sandbox mode %q (want \"\", \"none\", \"auto\", or \"inline\")",
			scope, name, blk.Mode)
		return nil
	}

	switch blk.HostState {
	case "", "auto", "none":
	default:
		c.errorfAt(DiagInvalidSandboxMode, name, "",
			"%s %q has invalid sandbox.host_state %q (want \"\", \"auto\", or \"none\")",
			scope, name, blk.HostState)
		return nil
	}

	spec := &SandboxSpec{
		Mode:            blk.Mode,
		Image:           blk.Image,
		User:            blk.User,
		WorkspaceFolder: blk.WorkspaceFolder,
		HostState:       blk.HostState,
		PostCreate:      blk.PostCreate,
	}
	if len(blk.Env) > 0 {
		spec.Env = make(map[string]string, len(blk.Env))
		for k, v := range blk.Env {
			spec.Env[k] = v
		}
	}
	if len(blk.Mounts) > 0 {
		spec.Mounts = append([]string(nil), blk.Mounts...)
	}
	if blk.Network != nil {
		spec.Network = &SandboxNetwork{
			Mode:    blk.Network.Mode,
			Preset:  blk.Network.Preset,
			Rules:   append([]string(nil), blk.Network.Rules...),
			Inherit: blk.Network.Inherit,
		}
	}
	if blk.Build != nil {
		spec.Build = &SandboxBuild{
			Dockerfile: blk.Build.Dockerfile,
			Context:    blk.Build.Context,
		}
		if len(blk.Build.Args) > 0 {
			spec.Build.Args = make(map[string]string, len(blk.Build.Args))
			for k, v := range blk.Build.Args {
				spec.Build.Args[k] = v
			}
		}
	}

	// Inline mode requires either an image or a build (V2-6) to be
	// set — otherwise the spec is incoherent and the runtime would
	// error out at Driver.Prepare time. Surface it as a compile-time
	// diagnostic so the user fixes the workflow source.
	if spec.Mode == "inline" && spec.Image == "" && spec.Build == nil {
		c.errorfAt(DiagInvalidSandboxMode, name, "",
			"%s %q has sandbox mode=inline but no image: declare an image or build, or use mode=auto with a .devcontainer/devcontainer.json",
			scope, name)
		return nil
	}
	if spec.Image != "" && spec.Build != nil {
		c.errorfAt(DiagInvalidSandboxMode, name, "",
			"%s %q has both sandbox.image and sandbox.build set; they are mutually exclusive (use image: for a pre-built ref or build: for a Dockerfile)",
			scope, name)
		return nil
	}
	return spec
}

// validateNodeNames enforces two cross-kind invariants on the AST node
// declarations BEFORE c.nodes is populated:
//
//  1. No user-declared node may use a reserved name ("done" / "fail").
//     Without this guard a JSON workflow declaring e.g. `agent done:`
//     would be silently replaced by the implicit DoneNode added later
//     in compile() — a different node kind with different semantics
//     than what was authored or reviewed.
//
//  2. Two declarations must not share an ID, whether within the same
//     kind or across kinds. The IR stores nodes in a single
//     map[string]Node so duplicates are last-wins; a second `agent foo`
//     would silently shadow the first with no diagnostic. Validation
//     would then run only against the survivor, which is the precise
//     trust-boundary hole an attacker can use to slip an unaudited
//     agent past a review pipeline that only inspects the first
//     occurrence.
//
// The parser already rejects reserved names for prompts/schemas/agents/
// judges/computes, but NOT for routers/humans/tools, AND the JSON AST
// path bypasses the parser entirely. Centralising the check here means
// both source paths (DSL and JSON) fail closed.
func (c *compiler) validateNodeNames() {
	type decl struct {
		kind string
		name string
	}
	all := make([]decl, 0,
		len(c.file.Agents)+len(c.file.Judges)+len(c.file.Routers)+
			len(c.file.Humans)+len(c.file.Tools)+len(c.file.Computes))
	for _, d := range c.file.Agents {
		all = append(all, decl{"agent", d.Name})
	}
	for _, d := range c.file.Judges {
		all = append(all, decl{"judge", d.Name})
	}
	for _, d := range c.file.Routers {
		all = append(all, decl{"router", d.Name})
	}
	for _, d := range c.file.Humans {
		all = append(all, decl{"human", d.Name})
	}
	for _, d := range c.file.Tools {
		all = append(all, decl{"tool", d.Name})
	}
	for _, d := range c.file.Computes {
		all = append(all, decl{"compute", d.Name})
	}
	for _, d := range c.file.Subbots {
		all = append(all, decl{"subbot", d.Name})
	}

	seen := make(map[string]string, len(all)) // name → first kind to claim it
	for _, d := range all {
		if d.name == "" {
			// The parser already emits a positional error; skip.
			continue
		}
		if ast.ReservedTargets[d.name] {
			c.errorfAt(DiagReservedNodeName, d.name, "",
				"%s %q uses reserved name %q: 'done' and 'fail' are implicit terminal nodes and cannot be declared",
				d.kind, d.name, d.name)
			continue
		}
		if firstKind, dup := seen[d.name]; dup {
			c.errorfAt(DiagDuplicateNodeID, d.name, "",
				"duplicate node ID %q: already declared as %s, redeclared as %s — node IDs must be unique across all kinds",
				d.name, firstKind, d.kind)
			continue
		}
		seen[d.name] = d.kind
	}
}

// Compile transforms an AST File into a canonical IR Workflow.
// In V1, exactly one workflow per file is supported.
func Compile(file *ast.File) *CompileResult {
	c := &compiler{
		file:    file,
		nodes:   make(map[string]Node),
		schemas: make(map[string]*Schema),
		prompts: make(map[string]*Prompt),
		mcp:     make(map[string]*MCPServer),
	}
	w := c.compile()
	return &CompileResult{
		Workflow:    w,
		Diagnostics: c.diags,
	}
}

func (c *compiler) compile() *Workflow {
	// Validate workflow count.
	if len(c.file.Workflows) == 0 {
		c.errorf(DiagNoWorkflow, "no workflow declaration found")
		return nil
	}
	if len(c.file.Workflows) > 1 {
		c.errorf(DiagMultipleWorkflow, "multiple workflows not supported in V1; found %d", len(c.file.Workflows))
	}

	// Expand `use <group>` instantiations into concrete prefixed nodes +
	// edges BEFORE any node compile pass, so groups are a pure compile-time
	// macro (never reaching the IR or runtime).
	c.expandGroups()

	// Compile shared declarations.
	c.compileMCPServers()
	c.compileSchemas()
	c.compilePrompts()
	cursors := c.compileCursors()
	supervisors := c.compileSupervisors()

	// Cross-kind node-name validation, run BEFORE the per-kind compile
	// passes that populate c.nodes. The parser already rejects reserved
	// names for prompts/schemas/agents/judges/computes — but NOT for
	// routers/humans/tools, and the JSON AST entry point (jsonenc.go
	// UnmarshalFile) bypasses the parser entirely. The compiler is the
	// single convergence point for both source paths, so we enforce two
	// invariants here:
	//
	//   1. No user node may be named "done" or "fail" (those slots are
	//      reserved for the implicit terminal nodes added below). Without
	//      this guard a hostile JSON workflow can declare e.g.
	//      `agent done:` with elevated tools, then have it silently
	//      shadowed by the DoneNode written at l.226-227 — but only AFTER
	//      validation has run against the user node, so the diagnostic
	//      would be wrong.
	//   2. Two declarations must not share a node ID across (or within)
	//      kinds. Last-wins semantics on the c.nodes map means a second
	//      `agent foo` block silently shadows the first; downstream
	//      validation runs only against the surviving node, which is
	//      exactly the trust-boundary hole an attacker can use to slip
	//      a tool-using agent past review.
	c.validateNodeNames()

	// Compile nodes from all node declarations.
	c.compileAgents()
	c.compileJudges()
	c.compileRouters()
	c.compileHumans()
	c.compileTools()
	c.compileComputes()
	c.compileEmits()
	c.compileWaits()
	c.compileAwaitAnswers()
	c.compileSubbots()

	// Add terminal nodes. Safe by construction now: validateNodeNames
	// above rejects any user node named "done"/"fail" before this point.
	c.nodes["done"] = &DoneNode{BaseNode: BaseNode{ID: "done"}}
	c.nodes["fail"] = &FailNode{BaseNode: BaseNode{ID: "fail"}}

	wf := c.file.Workflows[0]

	// Validate entry node.
	if _, ok := c.nodes[wf.Entry]; !ok {
		c.errorf(DiagMissingEntry, "entry node %q not found", wf.Entry)
	}

	// Compile vars (merge top-level + workflow-level).
	vars := c.compileVars(c.file.Vars, wf.Vars)

	// Compile secrets (top-level only).
	secrets := c.compileSecrets(c.file.Secrets, vars)

	// Compile presets (depend on vars for type coercion + name validation).
	presets := c.compilePresets(c.file.Presets, vars)

	// Compile attachments (merge top-level + workflow-level).
	attachments := c.compileAttachments(c.file.Attachments, wf.Attachments, vars)

	// Compile edges.
	edges, loops, foreaches := c.compileEdges(wf.Edges)

	// Compile budget.
	var budget *Budget
	if wf.Budget != nil {
		budget = c.compileBudget(wf.Budget)
	}

	// Compile resources (named counting semaphores + optional lease pools).
	resources, resourceMembers := c.compileResources(wf.Resources)

	// Compile workflow-level compaction overrides.
	compaction := compileCompaction(wf.Compaction)

	// Compile workflow-level interaction default.
	var interaction *InteractionMode
	if wf.Interaction != nil {
		im := *wf.Interaction
		interaction = &im
	}

	w := &Workflow{
		Name:            wf.Name,
		Entry:           wf.Entry,
		DefaultBackend:  wf.DefaultBackend,
		ToolPolicy:      wf.ToolPolicy,
		Capabilities:    wf.Capabilities,
		Skills:          wf.Skills,
		Nodes:           c.nodes,
		Edges:           edges,
		Schemas:         c.schemas,
		Prompts:         c.prompts,
		Vars:            vars,
		Secrets:         secrets,
		Presets:         presets,
		Attachments:     attachments,
		Loops:           loops,
		Foreaches:       foreaches,
		Budget:          budget,
		Resources:       resources,
		ResourceMembers: resourceMembers,
		Compaction:      compaction,
		MCP:             convertMCPConfig(wf.MCP),
		MCPServers:      c.mcp,
		Cursors:         cursors,
		Supervisors:     supervisors,
		Interaction:     interaction,
		Worktree:        defaultWorktreeMode(wf.Worktree),
		Compress:        wf.Compress,
		AutoMemory:      wf.AutoMemory,
		Permission:      wf.Permission,
		PermissionAllow: wf.Allow,
		PermissionAsk:   wf.Ask,
		PermissionDeny:  wf.Deny,
		Sandbox:         c.compileSandboxBlock(wf.Sandbox, "workflow", wf.Name),
	}

	// Compute each loop's body — the set of nodes that participate in
	// the loop's iteration cycle. Required so the runtime can reset a
	// loop's counter on re-entry from outside the body (turns a
	// run-global budget into a per-entry budget).
	computeLoopBodies(w)

	// Static validation pass (P2-02).
	c.validate(w)

	// Supervisor cross-references (watched nodes exist + are agents,
	// system prompt declared) — after nodes + prompts are on w.
	c.validateSupervisors(w)

	return w
}

// ---------------------------------------------------------------------------
// MCP servers
// ---------------------------------------------------------------------------

func (c *compiler) compileMCPServers() {
	for _, s := range c.file.MCPServers {
		if _, exists := c.mcp[s.Name]; exists {
			c.errorf(DiagDuplicateMCPServer, "mcp_server %q declared more than once", s.Name)
			continue
		}
		server := &MCPServer{
			Name:      s.Name,
			Transport: s.Transport,
			Command:   s.Command,
			Args:      append([]string(nil), s.Args...),
			URL:       s.URL,
			Auth:      compileMCPAuth(s.Auth),
		}
		c.validateMCPServer(server)
		c.mcp[s.Name] = server
	}
}

// compileMCPAuth converts an AST auth declaration to its IR form.
// Returns nil when the AST node is nil so a missing block stays absent.
func compileMCPAuth(decl *ast.MCPAuthDecl) *MCPAuth {
	if decl == nil {
		return nil
	}
	return &MCPAuth{
		Type:      decl.Type,
		AuthURL:   decl.AuthURL,
		TokenURL:  decl.TokenURL,
		RevokeURL: decl.RevokeURL,
		ClientID:  decl.ClientID,
		Scopes:    append([]string(nil), decl.Scopes...),
	}
}

func (c *compiler) validateMCPServer(s *MCPServer) {
	switch s.Transport {
	case MCPTransportStdio:
		if s.Command == "" {
			c.errorf(DiagInvalidMCPServer, "mcp_server %q with transport stdio must set 'command'", s.Name)
		}
		if s.URL != "" {
			c.errorf(DiagInvalidMCPServer, "mcp_server %q with transport stdio cannot set 'url'", s.Name)
		}
	case MCPTransportHTTP, MCPTransportSSE:
		// HTTP and SSE share the same StreamableClientTransport at
		// runtime: both require a URL and forbid Command/Args.
		if s.URL == "" {
			c.errorf(DiagInvalidMCPServer, "mcp_server %q with transport %s must set 'url'", s.Name, s.Transport)
		}
		if s.Command != "" {
			c.errorf(DiagInvalidMCPServer, "mcp_server %q with transport %s cannot set 'command'", s.Name, s.Transport)
		}
		if len(s.Args) > 0 {
			c.errorf(DiagInvalidMCPServer, "mcp_server %q with transport %s cannot set 'args'", s.Name, s.Transport)
		}
	case MCPTransportUnknown:
		c.errorf(DiagInvalidMCPServer, "mcp_server %q must set a supported 'transport'", s.Name)
	}
}

// ---------------------------------------------------------------------------
// Schemas
// ---------------------------------------------------------------------------

func (c *compiler) compileSchemas() {
	seen := make(map[string]bool, len(c.file.Schemas))
	for _, s := range c.file.Schemas {
		if seen[s.Name] {
			// Without this check a second `schema foo:` silently
			// overwrote the first in c.schemas — every downstream
			// validation then only saw the survivor, so an attacker
			// could slip an unaudited schema past a review pipeline
			// that inspected only the first occurrence.
			c.errorf(DiagDuplicateNodeID,
				"duplicate schema name %q: schemas must be unique within a file", s.Name)
			continue
		}
		seen[s.Name] = true
		fields := make([]*SchemaField, len(s.Fields))
		for i, f := range s.Fields {
			fields[i] = &SchemaField{
				Name:       f.Name,
				Type:       f.Type,
				EnumValues: f.EnumValues,
			}
		}
		c.schemas[s.Name] = &Schema{
			Name:   s.Name,
			Fields: fields,
		}
	}
}

func convertMCPConfig(cfg *ast.MCPConfigDecl) *MCPConfig {
	if cfg == nil {
		return nil
	}
	return &MCPConfig{
		AutoloadProject: cloneBool(cfg.AutoloadProject),
		Inherit:         cloneBool(cfg.Inherit),
		Servers:         append([]string(nil), cfg.Servers...),
		Disable:         append([]string(nil), cfg.Disable...),
	}
}

func cloneBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func resolveSupervisorModel(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return os.Getenv("ITERION_DEFAULT_SUPERVISOR_MODEL")
}

// defaultWorktreeMode resolves the workflow's `worktree:` field into the
// runtime-canonical value. Worktree isolation is the DEFAULT so no bot
// ever dirties the live checkout — empty/unset resolves to "auto" while
// the explicit "none" opt-out is preserved verbatim. The runtime owns the
// "is this even a git repo?" guard and degrades gracefully to in-place
// when setup isn't possible (see pkg/runtime/engine.go), so this helper
// can keep the IR side strictly value-based.
func defaultWorktreeMode(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "none":
		return "none"
	case "auto":
		return "auto"
	case "":
		return "auto"
	default:
		// Unknown values flow through untouched. Validation already
		// rejects them at the AST surface (the parser only accepts
		// idents and the doctor flags strangers); preserving the raw
		// value here keeps any future strict diagnostic actionable.
		return v
	}
}

// canAutoResolveBackend reports whether the detect package can pick a
// backend from the host environment, allowing an agent/judge with
// neither `model:` nor `backend:` to validate. Cached per-compile —
// 20-agent workflows would otherwise re-stat credentials.json and
// re-LookPath the CLIs 40 times.
func (c *compiler) canAutoResolveBackend() bool {
	c.autoBackendOnce.Do(func() {
		report := detect.Detect(context.Background())
		c.autoBackendCached = detect.Resolve(report.PreferenceOrder, report.Backends) != ""
	})
	return c.autoBackendCached
}

// ---------------------------------------------------------------------------
// Prompts
// ---------------------------------------------------------------------------

func (c *compiler) compilePrompts() {
	seen := make(map[string]bool, len(c.file.Prompts))
	for _, p := range c.file.Prompts {
		if seen[p.Name] {
			// Mirror compileSchemas: a second `prompt foo:` used to
			// silently overwrite the first in c.prompts, leaving the
			// audit-relevant earlier body invisible.
			c.errorf(DiagDuplicateNodeID,
				"duplicate prompt name %q: prompts must be unique within a file", p.Name)
			continue
		}
		seen[p.Name] = true
		// Expand {{include "..."}} markers once, at compile time, before
		// ParseRefs sees the body — the injected file content becomes part
		// of the resolved prompt (auditable, no runtime file reads).
		// Resolve relative to the .bot source directory, carried on the
		// declaration's span (like MergeBundlePrompts reads bundle files
		// relative to the bundle dir).
		body, incErrs := expandPromptIncludes(p.Body, filepath.Dir(p.Span.Start.File))
		for _, e := range incErrs {
			c.errorf(DiagBadPromptInclude, "prompt %q: %v", p.Name, e)
		}
		refs, err := ParseRefs(body)
		if err != nil {
			c.errorf(DiagBadTemplateRef, "prompt %q: %v", p.Name, err)
		}
		c.prompts[p.Name] = &Prompt{
			Name:         p.Name,
			Body:         body,
			TemplateRefs: refs,
		}
	}
}

// ---------------------------------------------------------------------------
// Nodes — Agent
// ---------------------------------------------------------------------------

// buildLLMNodeShared runs the validation + resolution shared by agent and
// judge declarations (which embed the same ast.LLMDecl) and returns the
// three field groups common to AgentNode/JudgeNode. ok is false when the
// node must be skipped: a duplicate ID (validateNodeNames already emitted
// the diagnostic — first-wins keeps validation running against the FIRST
// declaration, the one that survives upstream review pipelines, instead of
// letting a later duplicate silently shadow it) or a reserved target name.
// kind ("agent"/"judge") only shapes diagnostics. The remaining flat node
// fields stay in each caller because AgentNode and JudgeNode are distinct
// types (constructed with field literals across the test suite), so they
// can't share an embedded *carrier* without breaking that construction API.
// They DO share a getter *interface* (LLMNode in ir.go): adding read
// methods leaves the struct layout and JSON encoding untouched, so every
// field-read site iterates over both via `case LLMNode` instead of an
// AgentNode/JudgeNode ladder.
func (c *compiler) buildLLMNodeShared(kind, name string, d *ast.LLMDecl) (LLMFields, SchemaFields, InteractionFields, bool) {
	if _, exists := c.nodes[name]; exists {
		return LLMFields{}, SchemaFields{}, InteractionFields{}, false
	}
	if ast.ReservedTargets[name] {
		return LLMFields{}, SchemaFields{}, InteractionFields{}, false
	}
	c.validateSchemaRef(name, "input", d.Input)
	c.validateSchemaRef(name, "output", d.Output)
	c.validatePromptRef(name, "system", d.System)
	c.validatePromptRef(name, "user", d.User)
	model := resolveSupervisorModel(d.Model)
	if model == "" && d.Backend == "" && !c.canAutoResolveBackend() {
		c.errorfAt(DiagMissingModelOrBackend, name, "", "%s %q must set 'model' or 'backend' (or configure a credential the runtime can detect — see docs/backends.md)", kind, name)
	}
	c.warnCodexDiscouraged(kind, name, d.Backend)

	// Apply workflow-level interaction default when node doesn't set one.
	interaction := d.Interaction
	if interaction == InteractionNone {
		interaction = c.workflowInteractionDefault()
	}

	return LLMFields{
			Model:           model,
			Backend:         d.Backend,
			Provider:        d.Provider,
			Command:         d.Command,
			SystemPrompt:    d.System,
			UserPrompt:      d.User,
			MaxTokens:       d.MaxTokens,
			ReasoningEffort: d.ReasoningEffort,
			Timeout:         d.Timeout,
			Readonly:        d.Readonly,
			FullAccess:      d.FullAccess,
			Images:          d.Images,
		}, SchemaFields{
			InputSchema:  d.Input,
			OutputSchema: d.Output,
		}, InteractionFields{
			Interaction:       interaction,
			InteractionPrompt: d.InteractionPrompt,
			InteractionModel:  d.InteractionModel,
		}, true
}

func (c *compiler) compileAgents() {
	for _, a := range c.file.Agents {
		llm, sch, inter, ok := c.buildLLMNodeShared("agent", a.Name, &a.LLMDecl)
		if !ok {
			continue
		}
		c.nodes[a.Name] = &AgentNode{
			BaseNode:          BaseNode{ID: a.Name, Description: a.Description},
			LLMFields:         llm,
			SchemaFields:      sch,
			InteractionFields: inter,
			MCP:               convertMCPConfig(a.MCP),
			Publish:           a.Publish,
			PublishLabels:     a.ArtifactLabels,
			Session:           a.Session,
			Tools:             a.Tools,
			ToolPolicy:        a.ToolPolicy,
			Capabilities:      a.Capabilities,
			Skills:            a.Skills,
			ToolMaxSteps:      a.ToolMaxSteps,
			AwaitMode:         a.Await,
			Compaction:        compileCompaction(a.Compaction),
			Memory:            compileMemory(a.Memory),
			Sandbox:           c.compileSandboxBlock(a.Sandbox, "agent", a.Name),
			Cursors:           compileCursorInvocation(a.Cursors),
			Compress:          a.Compress,
			AutoMemory:        a.AutoMemory,
			Permission:        a.Permission,
			Needs:             a.Needs,
		}
	}
}

// ---------------------------------------------------------------------------
// Nodes — Judge
// ---------------------------------------------------------------------------

func (c *compiler) compileJudges() {
	for _, j := range c.file.Judges {
		llm, sch, inter, ok := c.buildLLMNodeShared("judge", j.Name, &j.LLMDecl)
		if !ok {
			continue
		}
		c.nodes[j.Name] = &JudgeNode{
			BaseNode:          BaseNode{ID: j.Name, Description: j.Description},
			LLMFields:         llm,
			SchemaFields:      sch,
			InteractionFields: inter,
			MCP:               convertMCPConfig(j.MCP),
			Publish:           j.Publish,
			Session:           j.Session,
			Tools:             j.Tools,
			ToolPolicy:        j.ToolPolicy,
			Capabilities:      j.Capabilities,
			Skills:            j.Skills,
			ToolMaxSteps:      j.ToolMaxSteps,
			AwaitMode:         j.Await,
			Compaction:        compileCompaction(j.Compaction),
			Memory:            compileMemory(j.Memory),
			Sandbox:           c.compileSandboxBlock(j.Sandbox, "judge", j.Name),
			Cursors:           compileCursorInvocation(j.Cursors),
			Compress:          j.Compress,
			AutoMemory:        j.AutoMemory,
			Permission:        j.Permission,
			Needs:             j.Needs,
		}
	}
}

// ---------------------------------------------------------------------------
// Nodes — Router
// ---------------------------------------------------------------------------

func (c *compiler) compileRouters() {
	for _, r := range c.file.Routers {
		if _, exists := c.nodes[r.Name]; exists {
			continue
		}
		if ast.ReservedTargets[r.Name] {
			continue
		}
		mode := r.Mode
		node := &RouterNode{
			BaseNode:   BaseNode{ID: r.Name, Description: r.Description},
			RouterMode: mode,
			Needs:      r.Needs,
		}
		if mode != RouterLLM {
			if r.Model != "" {
				c.errorf(DiagRouterLLMOnlyProperty, "router %q property 'model' is only valid with mode: llm", r.Name)
			}
			if r.Backend != "" {
				c.errorf(DiagRouterLLMOnlyProperty, "router %q property 'backend' is only valid with mode: llm", r.Name)
			}
			if r.System != "" {
				c.errorf(DiagRouterLLMOnlyProperty, "router %q property 'system' is only valid with mode: llm", r.Name)
			}
			if r.User != "" {
				c.errorf(DiagRouterLLMOnlyProperty, "router %q property 'user' is only valid with mode: llm", r.Name)
			}
			if r.Multi {
				c.errorf(DiagRouterLLMOnlyProperty, "router %q property 'multi' is only valid with mode: llm", r.Name)
			}
			if r.ReasoningEffort != "" {
				c.errorf(DiagRouterLLMOnlyProperty, "router %q property 'reasoning_effort' is only valid with mode: llm", r.Name)
			}
		}
		if mode == RouterLLM {
			model := resolveSupervisorModel(r.Model)
			if model == "" && r.Backend == "" {
				c.warnf(DiagMissingModelOrBackend, "router %q with mode llm has no model or backend; will use built-in default at runtime", r.Name)
			}
			node.Model = model
			node.Backend = r.Backend
			node.Provider = r.Provider
			c.warnCodexDiscouraged("router", r.Name, r.Backend)
			if r.System != "" {
				c.validatePromptRef(r.Name, "system", r.System)
				node.SystemPrompt = r.System
			}
			if r.User != "" {
				c.validatePromptRef(r.Name, "user", r.User)
				node.UserPrompt = r.User
			}
			node.RouterMulti = r.Multi
			node.ReasoningEffort = r.ReasoningEffort
		}
		// Data-driven fan-out config (RouterFanOutEach only).
		if mode == RouterFanOutEach {
			if r.Over == "" {
				c.errorf(DiagFanOutEachMissingOver,
					"router %q with mode fan_out_each requires an 'over:' array source (e.g. over: \"{{outputs.decompose.tickets}}\")", r.Name)
			} else {
				refs, err := ParseRefs(r.Over)
				if err != nil {
					c.errorf(DiagFanOutEachMissingOver, "router %q 'over' is not a valid template: %v", r.Name, err)
				}
				node.Over = r.Over
				node.OverRefs = refs
			}
			node.ItemBinding = r.As
			if node.ItemBinding == "" {
				node.ItemBinding = "item"
			}
			node.KeyField = r.Key
			node.DepsField = r.DependsOn
			// depends_on without key is ambiguous (no id to resolve deps against).
			if r.DependsOn != "" && r.Key == "" {
				c.errorf(DiagFanOutEachMissingOver, "router %q has 'depends_on' but no 'key'; a 'key' field is required to identify items for DAG scheduling", r.Name)
			}
		} else {
			if r.Over != "" {
				c.errorf(DiagFanOutEachOnlyProperty, "router %q property 'over' is only valid with mode: fan_out_each", r.Name)
			}
			if r.As != "" {
				c.errorf(DiagFanOutEachOnlyProperty, "router %q property 'as' is only valid with mode: fan_out_each", r.Name)
			}
			if r.Key != "" || r.DependsOn != "" {
				c.errorf(DiagFanOutEachOnlyProperty, "router %q properties 'key'/'depends_on' are only valid with mode: fan_out_each", r.Name)
			}
		}
		c.nodes[r.Name] = node
	}
}

// ---------------------------------------------------------------------------
// Nodes — Human
// ---------------------------------------------------------------------------

func (c *compiler) compileHumans() {
	for _, h := range c.file.Humans {
		if _, exists := c.nodes[h.Name]; exists {
			continue
		}
		if ast.ReservedTargets[h.Name] {
			continue
		}
		c.validateSchemaRef(h.Name, "input", h.Input)
		c.validateSchemaRef(h.Name, "output", h.Output)
		c.validatePromptRef(h.Name, "instructions", h.Instructions)

		interaction := h.Interaction
		// Human nodes default to InteractionHuman; workflow-level default
		// can override when the node doesn't set interaction explicitly.
		if h.Interaction == 0 {
			wfDefault := c.workflowInteractionDefault()
			if wfDefault != InteractionNone && wfDefault != InteractionAsync {
				interaction = wfDefault
			} else {
				interaction = InteractionHuman
			}
		}
		// interaction: async is an agent/judge posture (post questions, keep
		// working) — a human node IS the blocking question, so async on it is
		// a contradiction, not a mode.
		if interaction == InteractionAsync {
			c.errorfAt(DiagAsyncOnHuman, h.Name, "",
				"human %q cannot use interaction: async — async questions are posted by agent/judge nodes (use interaction: async on the asking node and an await_answers node as the sync point)", h.Name)
			interaction = InteractionHuman
		}
		node := &HumanNode{
			BaseNode: BaseNode{ID: h.Name, Description: h.Description},
			SchemaFields: SchemaFields{
				InputSchema:  h.Input,
				OutputSchema: h.Output,
			},
			InteractionFields: InteractionFields{
				Interaction:       interaction,
				InteractionPrompt: h.InteractionPrompt,
				InteractionModel:  h.InteractionModel,
			},
			Publish:       h.Publish,
			PublishLabels: h.ArtifactLabels,
			MinAnswers:    h.MinAnswers,
			Instructions:  h.Instructions,
			AwaitMode:     h.Await,
		}

		// LLM-driven interaction modes (llm, llm_or_human, review) require a
		// companion model and an output schema.
		if interaction == InteractionLLM || interaction == InteractionLLMOrHuman || interaction == InteractionReview {
			model := h.InteractionModel
			if model == "" {
				model = h.Model
			}
			if model == "" {
				c.errorf(DiagMissingModelOrBackend, "human %q with interaction %s must set 'model' or 'interaction_model'", h.Name, interaction)
			}
			if h.Output == "" {
				c.errorf(DiagMissingModelOrBackend, "human %q with interaction %s must set 'output'", h.Name, interaction)
			}
			node.Model = h.Model
			if h.InteractionModel != "" {
				node.InteractionModel = h.InteractionModel
			}
			if h.System != "" {
				c.validatePromptRef(h.Name, "system", h.System)
				node.SystemPrompt = h.System
			}
		}

		// Review-gate configuration: defaults + ReviewURL ref parsing.
		if interaction == InteractionReview {
			node.Posture = h.Posture
			if node.Posture == "" {
				node.Posture = PostureHumanRequired
			}
			node.MergeStrategy = h.MergeStrategy
			if node.MergeStrategy == "" {
				node.MergeStrategy = "squash"
			}
			node.MergeInto = h.MergeInto
			if node.MergeInto == "" {
				node.MergeInto = "current"
			}
			node.MaxTurns = h.MaxTurns
			if node.MaxTurns <= 0 {
				node.MaxTurns = DefaultReviewMaxTurns
			}
			node.ReviewURL = h.ReviewURL
			if h.ReviewURL != "" {
				refs, err := ParseRefs(h.ReviewURL)
				if err != nil {
					c.errorf(DiagBadTemplateRef, "human %q review_url: %v", h.Name, err)
				} else {
					node.ReviewURLRefs = refs
				}
			}
		}

		c.nodes[h.Name] = node
	}
}

// ---------------------------------------------------------------------------
// Nodes — Tool
// ---------------------------------------------------------------------------

func (c *compiler) compileTools() {
	for _, t := range c.file.Tools {
		if _, exists := c.nodes[t.Name]; exists {
			continue
		}
		if ast.ReservedTargets[t.Name] {
			continue
		}
		c.validateSchemaRef(t.Name, "output", t.Output)
		if t.Input != "" {
			c.validateSchemaRef(t.Name, "input", t.Input)
		}

		// command and script are mutually exclusive; exactly one must be set.
		switch {
		case t.Command == "" && t.Script == "":
			c.errorf(DiagBadTemplateRef, "tool %q: must declare either `command:` or `script:`", t.Name)
		case t.Command != "" && t.Script != "":
			c.errorf(DiagBadTemplateRef, "tool %q: `command:` and `script:` are mutually exclusive", t.Name)
		case t.Script == "" && t.Language != "":
			// language without script makes no sense.
			c.errorf(DiagBadTemplateRef, "tool %q: `language:` is only valid alongside `script:`", t.Name)
		}

		var cmdRefs []*Ref
		if t.Command != "" {
			if refs, err := ParseRefs(t.Command); err != nil {
				c.errorf(DiagBadTemplateRef, "tool %q command: %v", t.Name, err)
			} else {
				cmdRefs = refs
			}
		}

		var scriptRefs []*Ref
		if t.Script != "" {
			if refs, err := ParseRefs(t.Script); err != nil {
				c.errorf(DiagBadTemplateRef, "tool %q script: %v", t.Name, err)
			} else {
				scriptRefs = refs
			}
		}

		// Validate language token if provided.
		if t.Language != "" {
			switch t.Language {
			case "js", "node", "py", "python", "python3", "sh", "bash":
				// known
			default:
				c.errorf(DiagBadTemplateRef, "tool %q: unsupported language %q (want one of: js, node, py, python, python3, sh, bash)", t.Name, t.Language)
			}
		}

		// Verified Action quad (ADR-044). Parse postcondition refs (same
		// template machinery as command), default the policy when a
		// postcondition is present, and compile the recovery bounds.
		var postcondRefs []*Ref
		if t.Postcondition != "" {
			if refs, err := ParseRefs(t.Postcondition); err != nil {
				c.errorf(DiagBadTemplateRef, "tool %q postcondition: %v", t.Name, err)
			} else {
				postcondRefs = refs
			}
		}
		policy := t.Policy
		if policy == "" && t.Postcondition != "" {
			// Postcondition without an explicit policy → required (the
			// postcondition becomes truth; no recovery — the safe default).
			policy = PolicyRequired
		}
		var recovery *RecoverySpec
		if t.Recovery != nil {
			recovery = &RecoverySpec{
				MaxRepairAttempts: t.Recovery.MaxRepairAttempts,
				MaxAgentAttempts:  t.Recovery.MaxAgentAttempts,
				Model:             t.Recovery.Model,
				AgentTools:        t.Recovery.AgentTools,
			}
		}
		// Under recover, ensure a spec exists and default to one self-repair
		// attempt when the author left both bounds unset; agent recovery stays
		// opt-in (0).
		if policy == PolicyRecover {
			if recovery == nil {
				recovery = &RecoverySpec{}
			}
			if recovery.MaxRepairAttempts == 0 && recovery.MaxAgentAttempts == 0 {
				recovery.MaxRepairAttempts = 1
			}
		}

		c.nodes[t.Name] = &ToolNode{
			BaseNode: BaseNode{ID: t.Name, Description: t.Description},
			SchemaFields: SchemaFields{
				InputSchema:  t.Input,
				OutputSchema: t.Output,
			},
			Command:       t.Command,
			CommandRefs:   cmdRefs,
			Script:        t.Script,
			ScriptRefs:    scriptRefs,
			Language:      t.Language,
			Publish:       t.Publish,
			PublishLabels: t.ArtifactLabels,
			AwaitMode:     t.Await,
			Sandbox:       c.compileSandboxBlock(t.Sandbox, "tool", t.Name),
			Compress:      t.Compress,
			Permission:    t.Permission,
			Needs:         t.Needs,
			Goal:          t.Goal,
			Postcondition: t.Postcondition,
			PostcondRefs:  postcondRefs,
			Policy:        policy,
			Recovery:      recovery,
			ParallelSafe:  t.ParallelSafe,
		}
	}
}

// ---------------------------------------------------------------------------
// Nodes — Compute
// ---------------------------------------------------------------------------

func (c *compiler) compileComputes() {
	for _, cd := range c.file.Computes {
		if _, exists := c.nodes[cd.Name]; exists {
			continue
		}
		if ast.ReservedTargets[cd.Name] {
			continue
		}
		c.validateSchemaRef(cd.Name, "output", cd.Output)
		if cd.Input != "" {
			c.validateSchemaRef(cd.Name, "input", cd.Input)
		}
		if len(cd.Expr) == 0 {
			c.errorfAt(DiagComputeNoExpr, cd.Name, "",
				"compute %q has no `expr:` block — at least one expression is required", cd.Name)
		}
		exprs := make([]*ComputeExpr, 0, len(cd.Expr))
		for _, e := range cd.Expr {
			ast, err := expr.Parse(e.Expr)
			if err != nil {
				c.errorfAt(DiagBadExpr, cd.Name, "",
					"compute %q field %q: invalid expression %q: %v", cd.Name, e.Key, e.Expr, err)
				continue
			}
			exprs = append(exprs, &ComputeExpr{
				Key: e.Key,
				AST: ast,
				Raw: e.Expr,
			})
		}
		c.nodes[cd.Name] = &ComputeNode{
			BaseNode: BaseNode{ID: cd.Name, Description: cd.Description},
			SchemaFields: SchemaFields{
				InputSchema:  cd.Input,
				OutputSchema: cd.Output,
			},
			Exprs:         exprs,
			Publish:       cd.Publish,
			PublishLabels: cd.ArtifactLabels,
			AwaitMode:     cd.Await,
		}
	}
}

func (c *compiler) compileSubbots() {
	for _, sd := range c.file.Subbots {
		if _, exists := c.nodes[sd.Name]; exists {
			continue
		}
		if ast.ReservedTargets[sd.Name] {
			continue
		}
		if sd.Source == "" {
			c.errorfAt(DiagSubbotNoSource, sd.Name, "", "subbot %q has no `source:` — a child .bot path is required", sd.Name)
		}
		if sd.Output != "" {
			c.validateSchemaRef(sd.Name, "output", sd.Output)
		}
		c.nodes[sd.Name] = &SubbotNode{
			BaseNode:     BaseNode{ID: sd.Name, Description: sd.Description},
			Source:       sd.Source,
			With:         c.compileWithMappings(sd.Name, sd.With),
			OutputSchema: sd.Output,
			Needs:        sd.Needs,
			Isolated:     sd.Isolated,
		}
	}
}

// compileWithMappings parses a node's `with { ... }` entries into DataMappings,
// emitting C004 for any malformed template. Shared by the node kinds that carry
// a `with:` payload (emit, subbot).
func (c *compiler) compileWithMappings(nodeID string, entries []*ast.WithEntry) []*DataMapping {
	with := make([]*DataMapping, 0, len(entries))
	for _, w := range entries {
		refs, err := ParseRefs(w.Value)
		if err != nil {
			c.errorfAt(DiagBadTemplateRef, nodeID, "", "%s: with key %q: %v", nodeID, w.Key, err)
		}
		with = append(with, &DataMapping{Key: w.Key, Refs: refs, Raw: w.Value})
	}
	return with
}

// compileEmits compiles `emit` nodes (ADR-051): a named event plus an optional
// immutable payload resolved from the With data-mappings.
func (c *compiler) compileEmits() {
	for _, ed := range c.file.Emits {
		if _, exists := c.nodes[ed.Name]; exists {
			continue
		}
		if ast.ReservedTargets[ed.Name] {
			continue
		}
		if ed.Event == "" {
			c.errorfAt(DiagEventNoName, ed.Name, "", "emit %q has no `event:` name", ed.Name)
		}
		c.nodes[ed.Name] = &EmitNode{
			BaseNode: BaseNode{ID: ed.Name, Description: ed.Description},
			Event:    ed.Event,
			With:     c.compileWithMappings(ed.Name, ed.With),
		}
	}
}

// compileWaits compiles `wait` nodes (ADR-051): a named event plus a mandatory
// timeout (the no-silent-infinity invariant) and an optional payload schema.
func (c *compiler) compileWaits() {
	for _, wd := range c.file.Waits {
		if _, exists := c.nodes[wd.Name]; exists {
			continue
		}
		if ast.ReservedTargets[wd.Name] {
			continue
		}
		if wd.Event == "" {
			c.errorfAt(DiagEventNoName, wd.Name, "", "wait %q has no `event:` name", wd.Name)
		}
		var timeout time.Duration
		if wd.Timeout == "" {
			c.errorfAt(DiagWaitNoTimeout, wd.Name, "",
				"wait %q has no `timeout:` — a mandatory bound is required (the no-silent-infinity invariant)", wd.Name)
		} else if d, err := time.ParseDuration(wd.Timeout); err != nil {
			c.errorfAt(DiagWaitNoTimeout, wd.Name, "", "wait %q has an invalid `timeout:` %q: %v", wd.Name, wd.Timeout, err)
		} else if d <= 0 {
			c.errorfAt(DiagWaitNoTimeout, wd.Name, "", "wait %q `timeout:` must be positive, got %q", wd.Name, wd.Timeout)
		} else {
			timeout = d
		}
		if wd.Output != "" {
			c.validateSchemaRef(wd.Name, "output", wd.Output)
		}
		c.nodes[wd.Name] = &WaitNode{
			BaseNode:     BaseNode{ID: wd.Name, Description: wd.Description},
			SchemaFields: SchemaFields{OutputSchema: wd.Output},
			Event:        wd.Event,
			Timeout:      timeout,
		}
	}
}

// compileAwaitAnswers compiles `await_answers` nodes (ADR-081): the
// deterministic sync point for async human questions, with a mandatory timeout
// (the no-silent-infinity invariant) and an optional `from:` scope.
func (c *compiler) compileAwaitAnswers() {
	for _, ad := range c.file.AwaitAnswers {
		if _, exists := c.nodes[ad.Name]; exists {
			continue
		}
		if ast.ReservedTargets[ad.Name] {
			continue
		}
		var timeout time.Duration
		if ad.Timeout == "" {
			c.errorfAt(DiagAwaitAnswersNoTimeout, ad.Name, "",
				"await_answers %q has no `timeout:` — a mandatory bound is required (the no-silent-infinity invariant)", ad.Name)
		} else if d, err := time.ParseDuration(ad.Timeout); err != nil {
			c.errorfAt(DiagAwaitAnswersNoTimeout, ad.Name, "", "await_answers %q has an invalid `timeout:` %q: %v", ad.Name, ad.Timeout, err)
		} else if d <= 0 {
			c.errorfAt(DiagAwaitAnswersNoTimeout, ad.Name, "", "await_answers %q `timeout:` must be positive, got %q", ad.Name, ad.Timeout)
		} else {
			timeout = d
		}
		c.nodes[ad.Name] = &AwaitAnswersNode{
			BaseNode: BaseNode{ID: ad.Name, Description: ad.Description},
			From:     ad.From,
			Timeout:  timeout,
		}
	}
}

// ---------------------------------------------------------------------------
// Edges
// ---------------------------------------------------------------------------

func (c *compiler) compileEdges(astEdges []*ast.Edge) ([]*Edge, map[string]*Loop, map[string]*Foreach) {
	loops := make(map[string]*Loop)
	foreaches := make(map[string]*Foreach)
	edges := make([]*Edge, 0, len(astEdges))

	for _, ae := range astEdges {
		// Validate node references.
		if _, ok := c.nodes[ae.From]; !ok {
			c.errorf(DiagUnknownNode, "edge source %q not found", ae.From)
		}
		if _, ok := c.nodes[ae.To]; !ok {
			c.errorf(DiagUnknownNode, "edge target %q not found", ae.To)
		}

		e := &Edge{
			From:   ae.From,
			To:     ae.To,
			IsElse: ae.IsElse,
		}

		// Condition: either a simple field name (legacy) or a parsed expression.
		if ae.When != nil {
			if ae.When.Expr != "" {
				ast, err := expr.Parse(ae.When.Expr)
				if err != nil {
					c.errorfAt(DiagBadExpr, "", edgeID(ae.From, ae.To),
						"edge %s -> %s: invalid `when` expression %q: %v",
						ae.From, ae.To, ae.When.Expr, err)
				} else {
					e.Expression = ast
					e.ExpressionSrc = ae.When.Expr
				}
			} else {
				e.Condition = ae.When.Condition
				e.Negated = ae.When.Negated
			}
		}

		// Loop.
		if ae.Loop != nil {
			e.LoopName = ae.Loop.Name
			if existing, ok := loops[ae.Loop.Name]; ok {
				// Multiple edges can share a loop, but the cap must
				// agree across both literal-int and template forms —
				// otherwise the runtime resolution would be ambiguous.
				if existing.MaxIterations != ae.Loop.MaxIterations ||
					existing.MaxIterationsExpr != ae.Loop.MaxIterationsExpr ||
					existing.Unbounded != ae.Loop.Unbounded {
					c.errorf(DiagDuplicateLoop,
						"loop %q has conflicting max_iterations: %d/%q vs %d/%q",
						ae.Loop.Name,
						existing.MaxIterations, existing.MaxIterationsExpr,
						ae.Loop.MaxIterations, ae.Loop.MaxIterationsExpr)
				}
			} else {
				loop := &Loop{
					Name:              ae.Loop.Name,
					MaxIterations:     ae.Loop.MaxIterations,
					MaxIterationsExpr: ae.Loop.MaxIterationsExpr,
					Unbounded:         ae.Loop.Unbounded,
					FuelCap:           ae.Loop.FuelCap,
				}
				if ae.Loop.MaxIterationsExpr != "" {
					refs, err := ParseRefs(ae.Loop.MaxIterationsExpr)
					if err != nil {
						c.errorf(DiagBadTemplateRef,
							"loop %q: template cap %q: %v",
							ae.Loop.Name, ae.Loop.MaxIterationsExpr, err)
					}
					// A cap expr with no template refs is a static string
					// (the parser catches the `as fix("2")` DSL form, but
					// group `${}` substitution and AST-JSON import can also
					// land a bare literal here). It would silently resolve to
					// MaxIterations=0 and skip the loop edge as exhausted on
					// the first traversal, so fold a plain integer into the
					// literal cap and reject anything else outright.
					if len(refs) == 0 {
						if n, aerr := strconv.Atoi(strings.TrimSpace(ae.Loop.MaxIterationsExpr)); aerr == nil {
							loop.MaxIterations = n
							loop.MaxIterationsExpr = ""
						} else {
							c.errorf(DiagBadTemplateRef,
								"loop %q: cap %q has no template refs and is not an integer — a static non-numeric cap would silently limit the loop to 0 iterations",
								ae.Loop.Name, ae.Loop.MaxIterationsExpr)
						}
					}
					loop.MaxIterationsExprRefs = refs
				}
				loops[ae.Loop.Name] = loop
			}
		}

		// Foreach (sequential collection iteration; mutually exclusive with Loop).
		if ae.Foreach != nil {
			if ae.Loop != nil {
				c.errorf(DiagForeachConflictsLoop, "edge %s -> %s: cannot combine `as foreach` with `as <loop>`", ae.From, ae.To)
			}
			e.ForeachName = ae.Foreach.Name
			if _, ok := foreaches[ae.Foreach.Name]; !ok {
				fe := &Foreach{
					Name:          ae.Foreach.Name,
					Item:          ae.Foreach.Item,
					CollectionRaw: ae.Foreach.Collection,
				}
				refs, err := ParseRefs(ae.Foreach.Collection)
				if err != nil {
					c.errorf(DiagBadTemplateRef, "foreach %q: collection %q: %v", ae.Foreach.Name, ae.Foreach.Collection, err)
				}
				fe.CollectionRefs = refs
				foreaches[ae.Foreach.Name] = fe
			}
		}

		// Data mappings.
		if len(ae.With) > 0 {
			e.With = make([]*DataMapping, len(ae.With))
			for i, w := range ae.With {
				refs, err := ParseRefs(w.Value)
				if err != nil {
					c.errorf(DiagBadTemplateRef, "edge %s -> %s, with key %q: %v",
						ae.From, ae.To, w.Key, err)
				}
				e.With[i] = &DataMapping{
					Key:  w.Key,
					Refs: refs,
					Raw:  w.Value,
				}
			}
		}

		edges = append(edges, e)
	}

	return edges, loops, foreaches
}

// ---------------------------------------------------------------------------
// Vars
// ---------------------------------------------------------------------------

func (c *compiler) compileVars(topLevel *ast.VarsBlock, workflowLevel *ast.VarsBlock) map[string]*Var {
	vars := make(map[string]*Var)

	addVars := func(vb *ast.VarsBlock) {
		if vb == nil {
			return
		}
		for _, f := range vb.Fields {
			v := &Var{
				Name: f.Name,
				Type: convertVarType(f.Type),
			}
			if len(f.EnumValues) > 0 {
				v.EnumValues = c.compileVarEnum(f)
			}
			if f.Default != nil {
				v.HasDefault = true
				// Reuse the preset coercion so a var default is validated and
				// normalized identically to a preset value (incl. int→float
				// widening — the old hand-rolled switch stored an int on a
				// float var). A scalar default whose literal type doesn't match
				// the declared type is C109; json / string[] accept loose
				// literals so they are never flagged. The natural value is kept
				// regardless so downstream {{vars.X}} refs still resolve.
				if coerced, ok := coercePresetLiteral(f.Default, v.Type); ok {
					v.Default = coerced
				} else {
					v.Default = literalNaturalValue(f.Default)
					if v.Type != VarJSON && v.Type != VarStringArray {
						c.errorf(DiagVarDefaultTypeMismatch,
							"var %q default value has wrong type (expected %s)", f.Name, v.Type.String())
					}
				}
			}
			// A default on an enum-constrained var must be one of the
			// declared values. A non-string default is C109 territory
			// (type mismatch) and deliberately not double-flagged here.
			if len(v.EnumValues) > 0 && v.HasDefault {
				if s, ok := v.Default.(string); ok && !slices.Contains(v.EnumValues, s) {
					c.errorf(DiagVarDefaultNotInEnum,
						"var %q default %q is not one of the enum values (%s)", f.Name, s, quoteList(v.EnumValues))
				}
			}
			vars[f.Name] = v
		}
	}

	// Top-level vars first, then workflow-level (workflow overrides).
	addVars(topLevel)
	addVars(workflowLevel)

	return vars
}

// compileVarEnum validates a var's `[enum: ...]` constraint: enums are
// only meaningful on string vars (C125), and duplicate values are
// deduplicated with a warning (C127). Returns the deduplicated value
// set, or nil when the constraint is invalid for the var's type.
func (c *compiler) compileVarEnum(f *ast.VarField) []string {
	vt := convertVarType(f.Type)
	if vt != VarString {
		c.errorf(DiagVarEnumNonString,
			"var %q: [enum: ...] is only valid on string vars, not %s", f.Name, vt.String())
		return nil
	}
	seen := make(map[string]bool, len(f.EnumValues))
	vals := make([]string, 0, len(f.EnumValues))
	for _, ev := range f.EnumValues {
		if seen[ev] {
			c.warnf(DiagVarEnumDuplicate,
				"var %q: duplicate enum value %q — keeping first occurrence", f.Name, ev)
			continue
		}
		seen[ev] = true
		vals = append(vals, ev)
	}
	return vals
}

// quoteList renders enum values as `"a", "b"` for diagnostics.
func quoteList(vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(quoted, ", ")
}

// compileSecrets resolves a top-level `secrets:` block into the IR map.
// Values stay as raw expressions (${ENV} / {{vars.X}}); the runtime
// resolves them to plaintext at run start. Returns nil when no block is
// declared.
func (c *compiler) compileSecrets(block *ast.SecretsBlock, vars map[string]*Var) map[string]*Secret {
	if block == nil || len(block.Fields) == 0 {
		return nil
	}
	secrets := make(map[string]*Secret, len(block.Fields))
	for _, f := range block.Fields {
		if _, dup := secrets[f.Name]; dup {
			c.errorf(DiagDuplicateSecret, "duplicate secret %q — keeping first declaration", f.Name)
			continue
		}
		if _, clash := vars[f.Name]; clash {
			c.errorf(DiagSecretVarConflict, "secret %q collides with a variable of the same name", f.Name)
		}
		as := f.As
		if as == "" {
			as = "value"
		}
		secrets[f.Name] = &Secret{
			Name:        f.Name,
			Value:       f.Value,
			As:          as,
			MountPath:   f.MountPath,
			Env:         f.Env,
			Optional:    f.Optional,
			Hosts:       f.Hosts,
			Description: f.Description,
		}
	}
	return secrets
}

// ---------------------------------------------------------------------------
// Presets
// ---------------------------------------------------------------------------

// compilePresets converts the AST `presets:` block to its IR map form,
// validating that each value targets a declared variable and coercing the
// literal to the variable's declared type. Type mismatches and unknown
// var references emit diagnostics; the offending entry is dropped from
// the resulting preset.
func (c *compiler) compilePresets(pb *ast.PresetsBlock, vars map[string]*Var) map[string]Preset {
	if pb == nil || len(pb.Entries) == 0 {
		return nil
	}
	out := make(map[string]Preset, len(pb.Entries))
	for _, entry := range pb.Entries {
		if _, dup := out[entry.Name]; dup {
			c.errorf(DiagDuplicatePreset, "preset %q declared more than once", entry.Name)
			continue
		}
		values := make(map[string]any, len(entry.Values))
		for _, pv := range entry.Values {
			v, ok := vars[pv.Key]
			if !ok {
				c.errorf(DiagPresetUnknownVar,
					"preset %q references unknown variable %q (declare it in vars:)",
					entry.Name, pv.Key)
				continue
			}
			coerced, ok := coercePresetLiteral(pv.Value, v.Type)
			if !ok {
				c.errorf(DiagPresetTypeMismatch,
					"preset %q: value for variable %q has wrong type (expected %s)",
					entry.Name, pv.Key, v.Type.String())
				continue
			}
			values[pv.Key] = coerced
		}
		out[entry.Name] = Preset{Name: entry.Name, Values: values}
	}
	return out
}

// coercePresetLiteral converts an ast.Literal to the runtime Go type
// matching the declared VarType. Returns (value, true) on success and
// (nil, false) on a type mismatch. VarJSON and VarStringArray accept
// string literals (the runtime parses them on demand).
func coercePresetLiteral(lit *ast.Literal, vt VarType) (any, bool) {
	if lit == nil {
		return nil, false
	}
	switch vt {
	case VarString, VarJSON, VarStringArray:
		if lit.Kind == ast.LitString {
			return lit.StrVal, true
		}
	case VarInt:
		if lit.Kind == ast.LitInt {
			return lit.IntVal, true
		}
	case VarFloat:
		if lit.Kind == ast.LitFloat {
			return lit.FloatVal, true
		}
		if lit.Kind == ast.LitInt {
			return float64(lit.IntVal), true
		}
	case VarBool:
		if lit.Kind == ast.LitBool {
			return lit.BoolVal, true
		}
	}
	return nil, false
}

// literalNaturalValue returns the Go value a literal carries by its own kind,
// ignoring any declared target type. Used as the fallback when a value fails
// type coercion (C109) so downstream references still resolve to something.
func literalNaturalValue(lit *ast.Literal) any {
	if lit == nil {
		return nil
	}
	switch lit.Kind {
	case ast.LitString:
		return lit.StrVal
	case ast.LitInt:
		return lit.IntVal
	case ast.LitFloat:
		return lit.FloatVal
	case ast.LitBool:
		return lit.BoolVal
	}
	return nil
}

// ---------------------------------------------------------------------------
// Attachments
// ---------------------------------------------------------------------------

func (c *compiler) compileAttachments(topLevel, workflowLevel *ast.AttachmentsBlock, vars map[string]*Var) map[string]*Attachment {
	attachments := make(map[string]*Attachment)

	addAttachments := func(ab *ast.AttachmentsBlock) {
		if ab == nil {
			return
		}
		for _, f := range ab.Fields {
			// C051: name must not collide with a declared var.
			if _, conflict := vars[f.Name]; conflict {
				c.errorf(DiagAttachmentVarConflict,
					"attachment %q shares its name with a declared variable; rename one",
					f.Name)
				continue
			}
			// C050: duplicate within attachments block (workflow overrides top-level silently).
			if _, dup := attachments[f.Name]; dup {
				c.errorf(DiagDuplicateAttachment,
					"attachment %q declared more than once", f.Name)
				continue
			}
			a := &Attachment{
				Name:        f.Name,
				Type:        convertAttachmentType(f.Type),
				AcceptMIME:  f.AcceptMIME,
				Description: f.Description,
			}
			if f.Required != nil {
				a.Required = *f.Required
			}
			// C052: each accept_mime entry must look like type/subtype.
			for _, m := range a.AcceptMIME {
				if !isValidMIME(m) {
					c.errorf(DiagInvalidAttachmentMIME,
						"attachment %q has invalid accept_mime entry %q (expected type/subtype)",
						f.Name, m)
				}
			}
			attachments[f.Name] = a
		}
	}

	addAttachments(topLevel)
	addAttachments(workflowLevel)

	return attachments
}

// isValidMIME accepts a permissive MIME pattern: `type/subtype` with
// optional `*` glob (e.g. `image/*`). Empty type or subtype is rejected.
func isValidMIME(m string) bool {
	parts := strings.SplitN(m, "/", 2)
	if len(parts) != 2 {
		return false
	}
	if parts[0] == "" || parts[1] == "" {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Budget
// ---------------------------------------------------------------------------

func (c *compiler) compileBudget(b *ast.BudgetBlock) *Budget {
	cost := b.MaxCostUSD
	if cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		// Negative / NaN / Inf cost caps would silently disable the
		// budget enforcer (it compares against the running tally and
		// the comparison degenerates). Treat them as misconfiguration
		// and clamp to 0 (= "no cap") with an explicit diagnostic so
		// the operator notices.
		c.errorf(DiagBudgetCostInvalid,
			"workflow.budget.max_cost_usd %v is not a finite non-negative number; treating as unset", cost)
		cost = 0
	}
	return &Budget{
		MaxParallelBranches: b.MaxParallelBranches,
		MaxDuration:         b.MaxDuration,
		MaxCostUSD:          cost,
		MaxTokens:           b.MaxTokens,
		WarnTokens:          b.WarnTokens,
		MaxIterations:       b.MaxIterations,
	}
}

// compileResources converts the AST resources block to the IR map
// (name → capacity). Capacities ≤ 0 are dropped with a diagnostic — a
// zero/negative semaphore would block its `needs:` nodes forever.
func (c *compiler) compileResources(rb *ast.ResourcesBlock) (map[string]int, map[string][]string) {
	if rb == nil || len(rb.Capacities) == 0 {
		return nil, nil
	}
	out := make(map[string]int, len(rb.Capacities))
	var members map[string][]string
	for name, capacity := range rb.Capacities {
		if capacity <= 0 {
			c.errorf(DiagResourceCapInvalid,
				"workflow.resources.%s capacity %d must be > 0", name, capacity)
			continue
		}
		out[name] = capacity
		// Lease form: a resource declared as an ident-list carries its member
		// ids; each acquire leases one distinct id (capacity = len(members)).
		if m := rb.Members[name]; len(m) > 0 {
			if members == nil {
				members = make(map[string][]string, len(rb.Capacities))
			}
			members[name] = m
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, members
}

// compileCompaction converts an AST CompactionBlock to its IR form. Returns
// nil when the AST block is nil or carries no overrides — keeping `nil` as
// the canonical "inherit" marker.
func compileCompaction(b *ast.CompactionBlock) *Compaction {
	if b == nil {
		return nil
	}
	out := &Compaction{}
	if b.Threshold != nil {
		out.Threshold = *b.Threshold
	}
	if b.PreserveRecent != nil {
		out.PreserveRecent = *b.PreserveRecent
	}
	if out.Threshold == 0 && out.PreserveRecent == 0 {
		return nil
	}
	return out
}

// compileMemory converts an AST MemoryBlock to its IR form. Returns
// nil when disabled (canonical "off" marker). Defaults:
// read=true, write=true, pre_compact_inject=false, autoload=nil
// (pkg/memory.Autoload falls back to INDEX.md).
func compileMemory(b *ast.MemoryBlock) *Memory {
	if b == nil {
		return nil
	}
	enabled := false
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	if !enabled {
		return nil
	}
	out := &Memory{Enabled: true, Read: true, Write: true}
	if b.Scope != nil {
		out.Scope = *b.Scope
	}
	if len(b.Autoload) > 0 {
		out.Autoload = append([]string(nil), b.Autoload...)
	}
	if b.Read != nil {
		out.Read = *b.Read
	}
	if b.Write != nil {
		out.Write = *b.Write
	}
	if b.PreCompactInject != nil {
		out.PreCompactInject = *b.PreCompactInject
	}
	if b.ProjectRoot != nil {
		out.ProjectRoot = *b.ProjectRoot
	}
	if b.Visibility != nil {
		out.Visibility = strings.TrimSpace(*b.Visibility)
	}
	return out
}

// knownMemoryVisibilities is the closed enum accepted by `visibility:`.
var knownMemoryVisibilities = map[string]bool{
	"bot": true, "project": true, "cross_project": true,
	"user": true, "org": true, "global": true,
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

func (c *compiler) validateSchemaRef(node, prop, ref string) {
	if ref == "" {
		return
	}
	if _, ok := c.schemas[ref]; !ok {
		c.errorf(DiagUnknownSchema, "node %q property %q references unknown schema %q", node, prop, ref)
	}
}

func (c *compiler) validatePromptRef(node, prop, ref string) {
	if ref == "" {
		return
	}
	if _, ok := c.prompts[ref]; !ok {
		c.errorf(DiagUnknownPrompt, "node %q property %q references unknown prompt %q", node, prop, ref)
	}
}

// ---------------------------------------------------------------------------
// Type converters (AST → IR)
// ---------------------------------------------------------------------------

func convertAttachmentType(at ast.AttachmentTypeExpr) AttachmentType {
	switch at {
	case ast.AttachmentTypeImage:
		return AttachmentImage
	default:
		return AttachmentFile
	}
}

func convertVarType(te ast.TypeExpr) VarType {
	switch te {
	case ast.TypeBool:
		return VarBool
	case ast.TypeInt:
		return VarInt
	case ast.TypeFloat:
		return VarFloat
	case ast.TypeJSON:
		return VarJSON
	case ast.TypeStringArray:
		return VarStringArray
	default:
		return VarString
	}
}

// computeLoopBodies populates Loop.Body for each loop in the workflow.
// A loop's body is the set of nodes on a non-loop-edge path from one of
// the loop's edge targets back to one of its edge sources, plus those
// endpoints. The runtime uses this body to detect "re-entry from
// outside" (a non-loop edge whose target is in the body and whose source
// is not) and reset the counter for a fresh iteration budget.
//
// Why non-loop edges only? With nested loops (e.g. a fix_loop inside a
// package_loop), traversing loop edges during the BFS pulls nodes from
// the OUTER cycle into the INNER loop's body — fix_loop would absorb
// select_candidate and commit_changes through the package_loop edge.
// Restricting to non-loop edges gives each loop its natural minimal
// scope, which is what the reset rule needs (resetting on entry to a
// non-shared portion of the inner loop's body).
//
// Algorithm per loop:
//  1. Collect all (from, to) pairs of edges that carry this loop's name.
//     Seed the body with their endpoints.
//  2. From every loop-edge target, do a forward BFS over non-loop edges:
//     these are the nodes reachable while staying inside the loop's
//     iteration without crossing any loop boundary.
//  3. From every loop-edge source, do a reverse BFS over non-loop edges.
//  4. Body = endpoints ∪ (forward ∩ reverse).
//
// Workflows without loops keep Loop.Body == nil. Loops whose endpoints
// share a single source/target node still compute correctly (Body is at
// minimum the {from, to} pair).
func computeLoopBodies(w *Workflow) {
	if len(w.Loops) == 0 || len(w.Edges) == 0 {
		return
	}
	// Build forward / reverse adjacency lists from NON-LOOP edges only.
	// Loop edges are explicitly skipped so the BFS cannot cross loop
	// boundaries and absorb nodes that belong to an enclosing or
	// neighbouring loop's cycle.
	forwardAdj := make(map[string][]string, len(w.Nodes))
	reverseAdj := make(map[string][]string, len(w.Nodes))
	for _, edge := range w.Edges {
		if edge == nil || edge.LoopName != "" {
			continue
		}
		forwardAdj[edge.From] = append(forwardAdj[edge.From], edge.To)
		reverseAdj[edge.To] = append(reverseAdj[edge.To], edge.From)
	}

	bfs := func(seeds []string, adj map[string][]string) map[string]bool {
		visited := make(map[string]bool, len(seeds))
		queue := make([]string, 0, len(seeds))
		for _, s := range seeds {
			if !visited[s] {
				visited[s] = true
				queue = append(queue, s)
			}
		}
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			for _, next := range adj[n] {
				if !visited[next] {
					visited[next] = true
					queue = append(queue, next)
				}
			}
		}
		return visited
	}

	for name, loop := range w.Loops {
		if loop == nil {
			continue
		}
		var sources, targets []string
		seen := make(map[string]bool)
		entries := make(map[string]bool)
		for _, edge := range w.Edges {
			if edge == nil || edge.LoopName != name {
				continue
			}
			sources = append(sources, edge.From)
			targets = append(targets, edge.To)
			seen[edge.From] = true
			seen[edge.To] = true
			entries[edge.To] = true
		}
		if len(sources) == 0 {
			continue
		}
		forward := bfs(targets, forwardAdj)
		reverse := bfs(sources, reverseAdj)
		body := make(map[string]bool, len(seen))
		for n := range seen {
			body[n] = true
		}
		for n := range forward {
			if reverse[n] {
				body[n] = true
			}
		}
		loop.Body = body
		loop.Entries = entries
	}
}
