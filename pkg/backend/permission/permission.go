// Package permission implements iterion's tool-permission gate — the
// anti-hypnosis / anti-prompt-injection boundary shared by every backend
// with an enforcement seam.
//
// The operator declares a Policy (a mode plus allow/ask/deny rule
// lists) in the .bot DSL or via CLI/env. Every tool call an agent
// attempts is evaluated by [Policy.Evaluate] — deterministic code
// OUTSIDE the model's controllable surface — so a prompt-injected or
// "hypnotized" agent that tries an off-policy action (exfiltrate a
// secret, curl an attacker, rm -rf, push to a rogue remote) is denied
// or surfaced to the human instead of silently proceeding.
//
// Rule syntax mirrors Claude Code: a rule is either a bare tool name
// (`Bash`, matches any use) or a scoped pattern (`Bash(go test:*)`,
// `Read(./.env)`, `Edit(pkg/**)`, `WebFetch(domain:example.com)`,
// `mcp__github__get_*`). A rule may also name a tool CLASS —
// `class:shell` — which matches every registered built-in the registry
// tags with the matching capability, present and future: `deny:` bounds
// the whole class, `ask:` gates it, and `allow:` grants it (widening to
// future members is the author's choice, warned at authoring). Because
// the same Policy drives both backends,
// backend-native tool names reach identical decisions — see
// [canonicalToolName].
package permission

import (
	"cmp"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// Mode is the top-level permission posture, mirroring Claude Code's
// permission modes (the iterion-facing names are off|ask|deny).
type Mode int

const (
	// ModeOff disables the gate entirely — today's bypassPermissions
	// behaviour. The default when nothing is declared.
	ModeOff Mode = iota
	// ModeAsk auto-approves allow-rule matches, hard-blocks deny-rule
	// matches, and PAUSES the run to ask the human for everything else
	// (Claude Code's `default` mode). Resumable.
	ModeAsk
	// ModeDeny auto-approves allow-rule matches and hard-denies
	// everything else with no pause (Claude Code's `dontAsk` mode) —
	// the policy boundary for headless/cloud/cron runs with no human.
	ModeDeny
)

// String returns the DSL/CLI spelling of the mode.
func (m Mode) String() string {
	switch m {
	case ModeAsk:
		return "ask"
	case ModeDeny:
		return "deny"
	default:
		return "off"
	}
}

// ParseMode converts a DSL/CLI/env string to a Mode. The empty string
// and "off" both map to ModeOff. Unknown values are an error so a
// typo'd `permission: askk` is caught at compile time, not silently
// treated as off.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "bypass", "bypasspermissions":
		return ModeOff, nil
	case "ask", "default", "prompt":
		return ModeAsk, nil
	case "deny", "dontask", "dont_ask":
		return ModeDeny, nil
	default:
		return ModeOff, fmt.Errorf("unknown permission mode %q (want off|ask|deny)", s)
	}
}

// ResolveModeSourced picks the effective gate mode from the standard
// precedence chain (run override > node > workflow > env > off) and
// names the winning level ("run_override" | "node" | "workflow" |
// "env" | "default") for the studio's settings-provenance caption.
// The first NON-EMPTY level wins even when it fails to parse — the
// error surfaces with the level that caused it, instead of silently
// falling through to a lower level the operator didn't pick.
func ResolveModeSourced(override, node, workflow, envDefault string) (Mode, string, error) {
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
			m, err := ParseMode(l.value)
			return m, l.source, err
		}
	}
	return ModeOff, "default", nil
}

// Decision is the outcome of evaluating a tool call against a Policy.
type Decision int

const (
	// Allow: execute the tool.
	Allow Decision = iota
	// Ask: pause the run and surface the call to the human.
	Ask
	// Deny: refuse the call (a synthetic isError tool_result is
	// returned to the model so it can adapt).
	Deny
)

// String renders the decision for logs/events.
func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case Ask:
		return "ask"
	case Deny:
		return "deny"
	default:
		return "unknown"
	}
}

// Policy is a resolved permission policy: a mode, the workflow-declared rule
// lists, and engine-owned operator grants. The zero value (ModeOff, no rules)
// is a disabled gate.
type Policy struct {
	Mode  Mode
	allow []rule
	ask   []rule
	deny  []rule
	// engineDeny holds the rules the ENGINE arms on the policy (the
	// per-node secret trees, DenyPathTrees), kept beside — never inside —
	// the author's deny list: the serialised config carries them on their
	// own field, so a reader of PolicyConfig.Deny sees exactly the rules
	// the workflow and the run declared. They judge with the same rank as
	// an author deny (they win in every mode, no grant reopens them).
	engineDeny []rule
	grants     []rule // operator-approved, engine-owned rules; never declared in a workflow
	scoped     bool   // any rule has an (arg) pattern → Evaluate must summarize
	// exempt is the set of tool names the runtime explicitly marked as its
	// own infrastructure (registration-linked, in addition to the reserved
	// MCP namespace IsInfrastructureTool recognises). Keyed by canonical
	// name. See MarkExempt.
	exempt map[string]bool
	// pathRoot is the directory a model-supplied RELATIVE path resolves
	// against — the working directory the gated agent's tools run in. Empty
	// (or non-absolute) means "no root captured": a relative path is then
	// matched on its own spelling only. See SetPathRoot.
	pathRoot string
}

// PolicyConfig is the serialisable form of a Policy. It deliberately carries
// the original rule strings rather than the compiled regular expressions so a
// permission hook running in a child process reconstructs the policy through
// NewPolicy and therefore uses the exact same parser and evaluator.
type PolicyConfig struct {
	Mode   string   `json:"mode"`
	Allow  []string `json:"allow,omitempty"`
	Ask    []string `json:"ask,omitempty"`
	Deny   []string `json:"deny,omitempty"`
	Grants []string `json:"grants,omitempty"`
	Exempt []string `json:"exempt,omitempty"`
	// EngineDeny carries the engine-armed deny rules (DenyPathTrees)
	// separately from the author-declared Deny list, so the boundary the
	// engine set survives the process boundary without impersonating a
	// rule the workflow wrote.
	EngineDeny []string `json:"engine_deny,omitempty"`
	// PathRoot travels with the rules because path normalisation is part of
	// matching: a gate reconstructed without it would resolve a relative
	// path differently from the engine, and the same call would reach two
	// verdicts on the two sides of the process boundary.
	PathRoot string `json:"path_root,omitempty"`
}

// Config returns a detached, serialisable snapshot of the policy.
func (p *Policy) Config() PolicyConfig {
	if p == nil {
		return PolicyConfig{Mode: ModeOff.String()}
	}
	raw := func(rules []rule) []string {
		out := make([]string, 0, len(rules))
		for _, r := range rules {
			out = append(out, r.raw)
		}
		return out
	}
	exempt := make([]string, 0, len(p.exempt))
	for name := range p.exempt {
		exempt = append(exempt, name)
	}
	sort.Strings(exempt)
	return PolicyConfig{
		Mode:       p.Mode.String(),
		Allow:      raw(p.allow),
		Ask:        raw(p.ask),
		Deny:       raw(p.deny),
		Grants:     raw(p.grants),
		EngineDeny: raw(p.engineDeny),
		Exempt:     exempt,
		PathRoot:   p.pathRoot,
	}
}

// NewPolicyFromConfig reconstructs a serialised policy through NewPolicy.
func NewPolicyFromConfig(cfg PolicyConfig) (*Policy, error) {
	mode, err := ParseMode(cfg.Mode)
	if err != nil {
		return nil, err
	}
	p, err := NewPolicy(mode, cfg.Allow, cfg.Ask, cfg.Deny)
	if err != nil {
		return nil, err
	}
	for _, rule := range cfg.Grants {
		p.AddGrantRule(rule)
	}
	for _, raw := range cfg.EngineDeny {
		r, err := parseRule(raw)
		if err != nil {
			// The engine wrote these; one that does not parse is a version
			// skew across the process boundary, and silently dropping a
			// SECURITY rule would reopen the tree it bounded. Fail loud.
			return nil, fmt.Errorf("engine deny rule %q: %w", raw, err)
		}
		p.addEngineDeny(r)
	}
	p.MarkExempt(cfg.Exempt...)
	p.SetPathRoot(cfg.PathRoot)
	return p, nil
}

// CanAsk reports whether evaluating this policy can produce an Ask decision.
// ModeDeny is not sufficient by itself: an explicit ask rule has precedence
// over the mode default.
func (p *Policy) CanAsk() bool {
	return p != nil && (p.Mode == ModeAsk || len(p.ask) > 0)
}

// NewPolicy parses the workflow-declared rule strings into a Policy. A
// malformed rule is an error (surfaced as a compile diagnostic upstream).
// Trusted operator grants are added separately. A literal, argument-scoped
// allow may supersede a bare ask for the same tool; all other asks retain
// precedence over ordinary allows (see [Policy.Evaluate]).
func NewPolicy(mode Mode, allow, ask, deny []string) (*Policy, error) {
	p := &Policy{Mode: mode}
	for _, group := range []struct {
		name string
		raw  []string
		dst  *[]rule
	}{
		{"allow", allow, &p.allow},
		{"ask", ask, &p.ask},
		{"deny", deny, &p.deny},
	} {
		for _, raw := range group.raw {
			r, err := parseRule(raw)
			if err != nil {
				return nil, fmt.Errorf("%s rule %q: %w", group.name, raw, err)
			}
			if r.argRe != nil {
				p.scoped = true
			}
			*group.dst = append(*group.dst, r)
		}
	}
	return p, nil
}

// Enabled reports whether the gate is active (mode != off).
func (p *Policy) Enabled() bool { return p != nil && p.Mode != ModeOff }

// AddAllowRule appends an ordinary allow rule. Operator approvals must use
// AddGrantRule so they can override an explicit ask rule without changing the
// precedence of workflow-declared allow rules.
func (p *Policy) AddAllowRule(raw string) {
	if r, err := parseRule(raw); err == nil {
		if r.argRe != nil {
			p.scoped = true
		}
		p.allow = append(p.allow, r)
	}
}

// AddGrantRule appends an engine-owned operator approval. It outranks an ask
// rule for the approved scope, but never an explicit deny rule. A malformed
// rule is ignored defensively: callers derive it from a structured tool call,
// never workflow source or arbitrary model text.
func (p *Policy) AddGrantRule(raw string) {
	if r, err := parseRule(raw); err == nil {
		if r.argRe != nil {
			p.scoped = true
		}
		p.grants = append(p.grants, r)
	}
}

// MarkExempt records tool names the runtime opened for its own
// interaction/capability plumbing, so the gate never blocks them. This is
// the registration-linked half of the exemption: the executor calls it
// with the exact infra tools it wired for a node (ask_user, board.*),
// rather than the permission package re-deriving the set from naming
// conventions it doesn't own. The reserved-namespace check in
// IsInfrastructureTool remains as a backend-agnostic backstop.
func (p *Policy) MarkExempt(names ...string) {
	if p == nil {
		return
	}
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			continue
		}
		if p.exempt == nil {
			p.exempt = make(map[string]bool)
		}
		p.exempt[canonicalToolName(n)] = true
	}
}

// SetPathRoot records the directory the gated agent's tools run in, so a
// rule written as an absolute path bounds a call that names the same file
// relatively. The executor passes the node's working directory; the root
// then travels to a gate rebuilt in another process through PolicyConfig.
//
// A root that is not absolute is ignored rather than guessed at: joining a
// relative root onto a relative path would invent a third path, matched
// against rules the operator wrote for neither.
func (p *Policy) SetPathRoot(dir string) {
	if p == nil {
		return
	}
	p.pathRoot = strings.TrimSpace(dir)
}

// pathTreeTools are the canonical tool spellings whose argument is a
// filesystem path the gate can bound — the read family, the write family,
// the listing, and BOTH search families (workspace_grep is deliberately
// not a `grep` alias in the catalogue, so a `Grep(...)` row does not reach
// it and it needs its own). The catalogue keeps no enumeration of
// path-taking tools, so this table is one of THREE a new path-carrying
// tool must join: summarize (what the call's candidates are),
// pathInputKeys (which input keys hold the paths), and this one (what an
// engine-armed tree deny covers). A tool added to fewer than the three is
// a tree an agent can still read through it.
var pathTreeTools = []string{"Read", "Write", "Edit", "NotebookEdit", "LS", "Grep", "Glob", "workspace_grep"}

// DenyPathTrees arms the policy with engine-owned deny rules bounding each
// directory for every path-anchored tool family — the shape that keeps a
// run's mounted secrets and seeded credential directories off a gated agent
// (#1710's per-node secrets bound: resolvePermissionPolicy arms it on every
// gated node). The rules judge with the author's deny list at the same
// rank — they win in every mode, no grant or allow can reopen them — but
// stay distinguishable from it: PolicyConfig carries them on its own
// field, so a gate rebuilt in another process refuses the same directories
// without the engine's bound impersonating a rule the workflow wrote. Bash
// is deliberately NOT bound: the shell is the legitimate sink channel (a
// command may pass the mounted path to a program that spends the secret),
// only the agent READING the plaintext is refused.
func (p *Policy) DenyPathTrees(dirs ...string) error {
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		clean := filepath.Clean(dir)
		for _, tool := range pathTreeTools {
			raw := tool + "(" + clean + "/**)"
			r, err := parseRule(raw)
			if err != nil {
				// The directories are engine constants; a rule that does
				// not parse is a bug here, not operator input to skip.
				return fmt.Errorf("path-tree deny %q: %w", raw, err)
			}
			p.addEngineDeny(r)
		}
	}
	return nil
}

// addEngineDeny appends one engine-armed deny rule, keeping the scoped
// flag true to what the rule carries.
func (p *Policy) addEngineDeny(r rule) {
	if r.argRe != nil {
		p.scoped = true
	}
	p.engineDeny = append(p.engineDeny, r)
}

// isExempt reports whether a tool call must bypass the gate — either the
// runtime explicitly marked it (MarkExempt) or it lives in iterion's
// reserved MCP namespace (IsInfrastructureTool).
func (p *Policy) isExempt(toolName string) bool {
	if p.exempt[canonicalToolName(toolName)] {
		return true
	}
	return IsInfrastructureTool(toolName)
}

// Evaluate decides what to do with a tool call. Workflow rules preserve
// Claude Code's evaluation order; an engine-owned grant from an explicit
// operator approval is considered after deny and before ask:
//
//  1. a matching deny rule → Deny (wins in every mode);
//  2. a matching trusted grant → Allow;
//  3. a matching scoped ask rule → Ask;
//  4. a matching bare exact-tool ask rule → Ask, except a matching literal
//     scoped allow for that same tool → Allow;
//  5. another matching ask rule → Ask;
//  6. a matching allow rule → Allow;
//  7. otherwise the mode default — ModeAsk → Ask, ModeDeny → Deny,
//     ModeOff → Allow (the gate is disabled).
//
// The returned string is the matched rule (or "" for the mode default)
// — used for the human prompt / deny reason.
//
// The CLOSING verdicts (deny, and the ask that outranks an allow) read the
// folded candidate set — a path's case/slash-folded spellings included — so
// a spelling trick cannot slip a refused directory past a `Read(**)` allow
// the ask was written to re-gate. The OPENING verdicts (grant, allow) stay
// on the spellings of the same file: a fold may name a different file on a
// case-sensitive file system, and a grant must never widen to it. Among
// those, a PATTERN scope reads only the spellings with no `..` segment left
// (see pathCandidates): `/work/../etc/passwd` carries the text of
// `Read(/work/**)` and names a file outside it.
func (p *Policy) Evaluate(toolName string, input map[string]any) (Decision, string) {
	if !p.Enabled() {
		return Allow, ""
	}
	// iterion's own interaction/capability plumbing (ask_user, board.*,
	// control, watch) is infrastructure, not an agent action against the
	// environment — never gate it, or `ask` mode would pause on the very
	// tool used to ask the human. Same exemption on both backends.
	if p.isExempt(toolName) {
		return Allow, ""
	}
	canon, open, closed := p.candidateSets(toolName, input)
	if r, ok := matchAny(p.deny, toolName, canon, closed); ok {
		return Deny, r.raw
	}
	// The engine-armed trees judge with the author's denies and below them,
	// so a matched author rule is the one the refusal message quotes.
	if r, ok := matchAny(p.engineDeny, toolName, canon, closed); ok {
		return Deny, r.raw
	}
	if r, ok := matchAny(p.grants, toolName, canon, open); ok {
		return Allow, r.raw
	}
	// A scoped ask is deliberately stronger than every allow. This lets an
	// operator re-gate a single otherwise auto-allowed command.
	if r, ok := matchScoped(p.ask, toolName, canon, closed, false); ok {
		return Ask, r.raw
	}
	// A bare exact-tool ask is a generic fallback, so an authored literal
	// allow for one complete argument can safely make that one invocation
	// automatic. Do not extend this to tool globs, wildcard/prefix arguments,
	// or catch-all asks: those remain approval gates.
	if r, ok := matchBareExactTool(p.ask, toolName, canon); ok {
		if allow, ok := matchScoped(p.allow, toolName, canon, open, true); ok {
			return Allow, allow.raw
		}
		return Ask, r.raw
	}
	if r, ok := matchAny(p.ask, toolName, canon, closed); ok {
		return Ask, r.raw
	}
	if r, ok := matchAny(p.allow, toolName, canon, open); ok {
		return Allow, r.raw
	}
	switch p.Mode {
	case ModeDeny:
		return Deny, ""
	default: // ModeAsk
		return Ask, ""
	}
}

// HasExplicitDeny reports whether a declared deny rule (rather than the
// permission mode's default) matches this call. Backend bridges use it before
// changing a native tool spelling: an explicit author denial always wins.
func (p *Policy) HasExplicitDeny(toolName string, input map[string]any) bool {
	if p == nil || !p.Enabled() || p.isExempt(toolName) {
		return false
	}
	_, ok := p.matchingDeny(toolName, input)
	return ok
}

// LeavesTool reports whether this gate lets AT LEAST ONE call of a tool run.
//
// It is the existential question [Policy.Evaluate] cannot answer on its own:
// Evaluate judges ONE call, so a reader asking "can this node reach a writer
// at all" would have to invent the call's arguments, and a scoped rule
// (`Write(docs/**)`) would then answer about the path the reader guessed
// rather than about the tool. This is THE answer to that question in this
// package — a lint, a pre-run admission or a corpus guard reads it instead of
// re-deriving the mode's meaning from the rule lists, which is how a reader
// and the gate come to disagree about a rule form (iterion#2832).
//
// spellings are the names the BACKEND may send for one tool concept: a
// declaration reaches the gate under the roster's spelling (`write_file` is
// `Write` on claude_code), and a GLOB row is matched against the raw name, so
// the concept survives when any spelling of it does. The caller owns that
// projection — the gate has no roster of its own — and the reading mirrors
// evaluateForwardedCall's, the one other site that gates a call it knows
// under several names. No spelling at all is no capability: the empty call
// leaves nothing.
//
// The mode decides which list answers, exactly as Evaluate's precedence does:
//
//   - off, or a nil policy: the gate is disabled and the lists are inert;
//   - any mode: a deny row bounding EVERY call removes the tool (deny wins
//     in every mode). A row bounding a subset does not — the other calls run;
//   - `ask`: everything no deny row bounds pauses for the human, who may
//     approve, so the capability exists;
//   - `deny`: the mode hard-denies everything no row matches, so the tool is
//     left only where an allow row, an ask row (a deny mode still pauses on
//     one) or an engine-owned grant NAMES it at all. A scoped allow is
//     enough: some calls pass.
//
// An exempt tool bypasses the gate entirely (Evaluate's own first answer), so
// it is always left.
func (p *Policy) LeavesTool(spellings ...string) bool {
	if !p.Enabled() {
		return true
	}
	for _, name := range spellings {
		if p.leavesSpelling(name) {
			return true
		}
	}
	return false
}

// leavesSpelling is LeavesTool for ONE name the gate may see.
func (p *Policy) leavesSpelling(name string) bool {
	if p.isExempt(name) {
		return true
	}
	canon := canonicalToolName(name)
	for _, r := range p.deny {
		if r.boundsEveryCall(name, canon) {
			return false
		}
	}
	if p.Mode != ModeDeny {
		return true
	}
	for _, rules := range [][]rule{p.grants, p.ask, p.allow} {
		for _, r := range rules {
			if r.matchesTool(name, canon) {
				return true
			}
		}
	}
	return false
}

func (p *Policy) matchingDeny(toolName string, input map[string]any) (rule, bool) {
	canon, _, closed := p.candidateSets(toolName, input)
	if r, ok := matchAny(p.deny, toolName, canon, closed); ok {
		return r, true
	}
	return matchAny(p.engineDeny, toolName, canon, closed)
}

// candidateSets renders the two candidate sets a call is judged against:
// the OPEN set (summarize — spellings of the same file, read by the
// verdicts that open: grants and allows; a pattern scope reads its
// non-climbing half, see candidates) and the CLOSED set (denyCandidates —
// every path the call names, folded spellings included, read by the
// verdicts that close: denies and asks). Both are only needed by scoped
// (arg-pattern) rules; skip the work entirely when every rule is bare (the
// common deny: [Bash] shape).
//
// A call that carries no argument at all offers one EMPTY candidate on
// both sides, which a scoped wildcard rule (`Bash(*)`, `Read(*)`) matches
// and a deny spelled that way must keep matching. A call whose opening set
// is empty because the gate refused every spelling of it is NOT such a
// call: the closed set still holds what it carried, and the opening rules
// it reaches are the bare and unbounded ones (matchesSummary).
func (p *Policy) candidateSets(toolName string, input map[string]any) (canon string, open, closed candidates) {
	canon = canonicalToolName(toolName)
	if !p.scoped {
		return canon, candidates{}, candidates{}
	}
	open = summarize(canon, input, p.pathRoot)
	shut := denyCandidates(canon, input, p.pathRoot, open.exact)
	if len(shut) == 0 {
		empty := []string{""}
		return canon, both(empty), both(empty)
	}
	return canon, open, both(shut)
}

// candidates is the set one call offers a rule list, read per rule.
//
// A PATTERN scope — a wildcard (`Read(/work/**)`) or a prefix
// (`Bash(git diff:*)`) — reads a spelling's text as containment: "under
// /work". It reads `pattern`, from which pathCandidates keeps every path
// spelling that still carries a `..` segment out:
// `/run/iterion/secrets/../../../etc/passwd` holds the text of
// `Read(/run/iterion/secrets/**)` and is /etc/passwd (#2918). An EXACT scope
// names one spelling whole, and a `..` segment cannot make the string it
// equals name another file, so it reads `exact`, every spelling the call
// has — which is what keeps an `allow once` quoting what the model wrote
// matching its own retry.
//
// The closing verdicts read one list both ways (both): over-refusing costs a
// refusal, and a deny written against the climb itself (`Read(**/../**)`)
// keeps biting.
type candidates struct {
	pattern []string
	exact   []string
}

// both is the candidate set that reads the same list for every rule shape.
func both(list []string) candidates { return candidates{pattern: list, exact: list} }

// of returns the list a rule reads.
func (c candidates) of(r rule) []string {
	if r.literalArg {
		return c.exact
	}
	return c.pattern
}

// add records one spelling: every rule shape may name it exactly, and a
// pattern reads it only when it has no `..` segment left.
func (c *candidates) add(s string) {
	c.exact = appendUnique(c.exact, s)
	if !pathClimbs(s) {
		c.pattern = appendUnique(c.pattern, s)
	}
}

// pathClimbs reports whether a path spelling carries a `..` segment — the
// one lexical shape that makes a spelling's TEXT name a directory the path
// is not under. filepath.Clean folds every such segment of an absolute
// path and leaves a relative one only at its head (`../x`), whose prefix
// is just as false against a rule that starts with dots (`Read(.*)`,
// `Read(../**)` meeting `../../x`). Repeated separators and `.` segments
// change no ancestry and do not count; a segment that merely contains dots
// (`..foo`) is a file name. The gate decodes nothing, so `%2e%2e` is a name
// too — the file system reads it as one.
func pathClimbs(s string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(s), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// rule is a parsed permission entry: a tool matcher plus an optional
// argument matcher. A nil argRe is a bare tool rule (matches any
// invocation of the tool).
type rule struct {
	raw    string         // original text, for messages + forwarding
	tool   string         // canonical tool key, or "*" (any) — literal case
	toolRe *regexp.Regexp // non-nil when the tool name itself is a glob
	// class is non-empty when the rule names a tool class (`class:shell`)
	// instead of a tool spelling. Membership is read from the registry's
	// capability metadata at match time, so the class covers the members
	// registered after the rule was written.
	class string
	argRe *regexp.Regexp // non-nil when the rule scoped an (arg) pattern
	// literalArg is true only for a bare, exact argument scope. It is kept
	// separately from argRe so permission precedence never has to infer
	// specificity from the rule spelling or regex shape.
	literalArg bool
	// anyArg is true when the argument scope leaves no call out — `Write(**)`
	// bounds every path, so the rule removes the tool as a bare one does.
	// Kept here, beside the parse that reads the pattern, so no caller has to
	// re-read a rule's spelling to learn how wide it is; which spellings are
	// that wide is argScopeIsUnbounded's.
	anyArg bool
}

// parseRule parses `Tool` or `Tool(content)` into a rule.
func parseRule(raw string) (rule, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return rule{}, fmt.Errorf("empty rule")
	}
	r := rule{raw: s}

	open := strings.IndexByte(s, '(')
	toolPart := s
	if open >= 0 {
		if !strings.HasSuffix(s, ")") {
			return rule{}, fmt.Errorf("missing closing ')'")
		}
		toolPart = s[:open]
		argContent := s[open+1 : len(s)-1]
		argRe, err := compileArg(argContent)
		if err != nil {
			return rule{}, fmt.Errorf("bad arg pattern: %w", err)
		}
		r.argRe = argRe
		trimmedArg := strings.TrimSpace(argContent)
		r.literalArg = !strings.HasSuffix(trimmedArg, ":*") && !strings.Contains(trimmedArg, "*")
		r.anyArg = argScopeIsUnbounded(trimmedArg)
	}
	toolPart = strings.TrimSpace(toolPart)
	if toolPart == "" {
		return rule{}, fmt.Errorf("missing tool name")
	}

	// A tool class: `class:shell` matches every registered built-in that
	// executes model-supplied code — the members present AND the ones a
	// later registration adds. The class table lives in the registry
	// (toolcatalog), so no edit here and no workflow change follows a new
	// member. An unknown class is refused here, naming the known ones: a
	// typo'd class would otherwise be a rule that matches nothing, read by
	// its author as a bound.
	if strings.HasPrefix(strings.ToLower(toolPart), "class:") {
		name := strings.TrimSpace(toolPart[len("class:"):])
		if !toolcatalog.IsKnownClass(name) {
			return rule{}, fmt.Errorf("unknown tool class %q (known classes: %s)", name, strings.Join(toolcatalog.KnownClasses(), ", "))
		}
		r.class = strings.ToLower(name)
		return r, nil
	}

	// Tool-name globs: `*` (any tool) and a trailing `*` after an
	// `mcp__server__` prefix (and, defensively, any literal prefix).
	if strings.Contains(toolPart, "*") {
		re, err := globToRegexp(toolPart)
		if err != nil {
			return rule{}, fmt.Errorf("bad tool glob: %w", err)
		}
		r.tool = toolPart
		r.toolRe = re
		return r, nil
	}
	r.tool = canonicalToolName(toolPart)
	return r, nil
}

// ValidateRule reports why a rule string is unparseable, or nil when the
// gate accepts it.
//
// It runs the REAL parser — the one Evaluate matches with — so it has no
// spelling of its own to keep in step. A malformed rule is otherwise
// discovered at dispatch, by which time the run has paid for a workspace
// and a model call to learn that a quote was missing.
func ValidateRule(raw string) error {
	_, err := parseRule(raw)
	return err
}

// RuleNamesClass reports the tool class a rule names, and whether it names
// one at all. The compiler's warnings read it to tell a class row (whose
// widening covers future members) from a spelling row.
func RuleNamesClass(rawRule string) (string, bool) {
	r, err := parseRule(rawRule)
	if err != nil || r.class == "" {
		return "", false
	}
	return r.class, true
}

// RuleCovers reports whether one rule string bounds the named tool — the
// question the compiler's class-coverage warning asks for every deny rule
// against every shell-capable built-in a node can call.
//
// Coverage is at the TOOL level: a rule matches the tool's canonical key
// (spelling row), its class membership (class row) or its name (glob row),
// whatever (arg) pattern the rule carries. An argument scope is the
// author's finer contract over that tool — `Bash(git push:*)` bounds bash
// for one command family — and the warning exists for the coarser hole, a
// tool NO row names at all. Read as "does any row bound this tool", never
// "does this row bound every use of the tool".
//
// It is the matcher [Policy.LeavesTool] reads for the deny-mode half of its
// own question, so the two cannot disagree about which rows name a tool.
func RuleCovers(rawRule, toolName string) bool {
	r, err := parseRule(rawRule)
	if err != nil {
		return false
	}
	canon := canonicalToolName(toolName)
	return r.matchesTool(toolName, canon)
}

// RuleRemovesTool reports whether one rule bounds EVERY call of a tool — the
// question a reader asking "is this tool gone" must ask, as opposed to
// RuleCovers' "does any row bound this tool".
//
// The two differ exactly on an argument scope: `Write(docs/**)` denies a
// subset, so a node holding it still writes everywhere else, while
// `Write(**)` leaves no call out and removes the tool as a bare `Write` does.
// A class row (`class:shell`) with no scope removes every member.
//
// It parses with the REAL grammar Evaluate matches with, and answers through
// the SAME predicate [Policy.LeavesTool] reads, so it has no spelling of its
// own to keep in step.
func RuleRemovesTool(rawRule, toolName string) bool {
	r, err := parseRule(rawRule)
	if err != nil {
		return false
	}
	return r.boundsEveryCall(toolName, canonicalToolName(toolName))
}

// boundsEveryCall reports whether this rule leaves no call of the tool out —
// THE reading of "is this tool gone" in this package, read by RuleRemovesTool
// and by Policy.LeavesTool so a rule form cannot mean one thing to a lint and
// another to the gate.
//
// A bare row bounds every call; an argument scope bounds a subset unless the
// scope itself leaves nothing out (anyArg, decided at parse).
func (r rule) boundsEveryCall(rawName, canon string) bool {
	if !r.matchesTool(rawName, canon) {
		return false
	}
	return r.argRe == nil || r.anyArg
}

// compileArg compiles the `(content)` part of a rule into one anchored
// matcher: a trailing `:*` is the Bash prefix idiom (`git diff:*` matches
// `git diff` and any longer command), `*`/`**` are greedy wildcards, and
// a bare literal is an exact match.
func compileArg(content string) (*regexp.Regexp, error) {
	c := strings.TrimSpace(content)
	switch {
	case strings.HasSuffix(c, ":*"):
		return regexp.Compile("^" + regexp.QuoteMeta(strings.TrimSuffix(c, ":*")))
	case strings.Contains(c, "*"):
		return globToRegexp(c)
	default:
		return regexp.Compile("^" + regexp.QuoteMeta(c) + "$")
	}
}

// argScopeIsUnbounded reports whether an argument scope leaves no call out —
// read from the SHAPE compileArg gives the scope, never from a list of
// spellings:
//
//   - `:*` is the prefix idiom with an EMPTY prefix, which compiles to `^`
//     and therefore matches every command — `deny: ["Bash(:*)"]` bounds the
//     shell as a bare `Bash` row does;
//   - a scope made of greedy wildcards and nothing else goes to globToRegexp,
//     which joins the pieces around each `*` with `.*`: `*` and `**` compile
//     to `^.*$`, `(***)` to `^.*.*.*$`, and every one of them matches every
//     candidate. An allowlist of three spellings read `Write(***)` as
//     bounding a subset while the gate denied 4/4 probe calls.
//
// A scope that bounds a subset (`docs/**`, `git diff:*`, `*:*`) is not one,
// and neither is `()`, whose matcher is `^$`: it bounds the empty argument
// alone. The reading is the scope's shape and not "the matcher accepts the
// empty string", which `^$` would satisfy too.
func argScopeIsUnbounded(trimmedArg string) bool {
	if trimmedArg == ":*" {
		return true
	}
	return trimmedArg != "" && strings.Trim(trimmedArg, "*") == ""
}

// matchAny returns the first rule in rs that matches the tool (by its raw
// name + canonical key) and, for scoped rules, one of the input candidates.
//
// A few calls deliberately expose multiple safe candidates (WebFetch's
// domain and full URL, a path's spellings, a search's pattern and root —
// see summarize). Shell commands are different: a newline is part of one
// command payload, not a second candidate, which is why summarize returns
// a shell command as a single candidate instead of a joined string a
// matcher would have to remember not to split. Each rule reads the half of
// the set its scope may read (candidates.of).
func matchAny(rs []rule, rawName, canon string, cands candidates) (rule, bool) {
	for _, r := range rs {
		if !r.matchesTool(rawName, canon) {
			continue
		}
		if r.matchesSummary(cands.of(r)) {
			return r, true
		}
	}
	return rule{}, false
}

// matchScoped returns a matching argument-scoped rule. When literalOnly is
// true, it accepts neither wildcard/prefix argument scopes nor tool globs.
func matchScoped(rs []rule, rawName, canon string, cands candidates, literalOnly bool) (rule, bool) {
	for _, r := range rs {
		if r.argRe == nil || (literalOnly && (!r.literalArg || r.toolRe != nil)) {
			continue
		}
		if r.matchesTool(rawName, canon) && r.matchesSummary(cands.of(r)) {
			return r, true
		}
	}
	return rule{}, false
}

// matchBareExactTool recognizes a generic ask only when it names this exact
// canonical tool. A wildcard ask remains a catch-all approval rule, and so
// does a class ask: a class spans tools, so its asks are approval gates, never
// the exact-tool shape a literal allow may supersede.
func matchBareExactTool(rs []rule, rawName, canon string) (rule, bool) {
	for _, r := range rs {
		if r.argRe == nil && r.toolRe == nil && r.class == "" && r.tool == canon && r.matchesTool(rawName, canon) {
			return r, true
		}
	}
	return rule{}, false
}

func (r rule) matchesSummary(cands []string) bool {
	if r.argRe == nil {
		return true // bare tool rule matches any invocation
	}
	if r.anyArg {
		// An unbounded scope leaves no call out — `Read(**)` reads every
		// path, the one whose every spelling a pattern may not read
		// included, which reaches here with no candidate at all.
		return true
	}
	// summarize may emit several candidates — e.g. WebFetch yields
	// "domain:<host>", "<host>" and the full URL — and a scoped rule
	// matches if it matches ANY of them. The candidate set is the whole
	// contract: a matcher cannot re-split one here without re-deciding,
	// per tool, what a separator means (see matchAny).
	for _, cand := range cands {
		if r.argRe.MatchString(cand) {
			return true
		}
	}
	return false
}

func (r rule) matchesTool(rawName, canon string) bool {
	if r.class != "" {
		return toolcatalog.ClassContains(r.class, canon)
	}
	if r.toolRe != nil {
		// Glob is matched against the raw name (so `mcp__github__get_*`
		// works on the FQN form) and the canonical key.
		return r.toolRe.MatchString(rawName) || r.toolRe.MatchString(canon)
	}
	if r.tool == "*" {
		return true
	}
	return r.tool == canon
}

// globToRegexp converts a `*`/`**` glob to an anchored regexp. Both `*`
// and `**` translate to `.*` (greedy) — a faithful, predictable
// superset of Claude Code's segment-aware globbing that covers the
// common rule shapes (`pkg/**`, `*.go`, `mcp__srv__get_*`). Documented
// in docs/permissions.md.
func globToRegexp(glob string) (*regexp.Regexp, error) {
	parts := strings.Split(glob, "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	return regexp.Compile("^" + strings.Join(parts, ".*") + "$")
}

// summarize renders a tool's input into the candidates that scoped rule
// patterns match against, mirroring Claude Code's per-tool summaries
// (Bash → command, Read/Edit/Write → path, WebFetch → domain + url). A
// rule matches if it matches ANY candidate, so a candidate is only ever
// offered for something the call genuinely does: another SPELLING of the
// same path (pathCandidates), or another DIMENSION of the same call
// (WebFetch's host beside its URL, a search's root beside its pattern).
// A second path the call merely carries is NOT one of them — an allow for
// the harmless one would authorise the other; denyCandidates is where
// every path a call names is read, because deny wins.
//
// canon is the already-canonicalised tool name. The per-tool argument keys
// mirror tooldisplay's header-detail extraction; keep the two in sync
// when adding a tool. The FIRST exact candidate is the one an operator
// approval is scoped to and the one messages quote (messages.go), so a new
// candidate is appended, never prepended.
//
// A new path-carrying tool joins THREE tables together, no enumeration
// derives them: this switch (what the call's candidates are),
// pathInputKeys (which input keys hold the paths), and pathTreeTools (what
// an engine-armed tree deny covers).
func summarize(canon string, input map[string]any, root string) candidates {
	switch canon {
	case "bash", "diagnostic_shell":
		// One opaque candidate whatever it contains: a newline is part of
		// the command payload, and splitting it would let an exact allow
		// for the first line approve arbitrary trailing shell text.
		return both([]string{str(input, "command")})
	case "read", "write", "edit", "notebookedit":
		return pathDimension(canon, primaryPath(canon, input), root)
	case "ls":
		return pathDimension(canon, primaryPath(canon, input), root)
	case "glob", "grep", "workspace_grep":
		// Two dimensions: WHAT is searched for and WHERE. Without the
		// second, a rule naming a directory is compared to the search
		// pattern only — so a `deny: ["Grep(/run/iterion/secrets/**)"]`
		// read as a bound was inert, and a bare Grep allow let an agent
		// read any file's content (#1710).
		pattern := str(input, "pattern")
		if globPatternClimbs(canon, pattern) {
			// A glob pattern is a path under the search root, and one that
			// climbs (`../../etc/*`) leaves it: the root no longer bounds
			// what the search reads, and the pattern's text names no place
			// without it. No opening rule but a bare or unbounded one can
			// vouch for such a search; the closing side still reads all of
			// it (denyCandidates).
			return candidates{}
		}
		dim := pathDimension(canon, primaryPath(canon, input), root)
		if pattern == "" {
			return dim
		}
		return candidates{
			pattern: append([]string{pattern}, dim.pattern...),
			exact:   append([]string{pattern}, dim.exact...),
		}
	case "webfetch":
		return fetchCandidates(str(input, "url"))
	case "websearch":
		return both([]string{str(input, "query")})
	default:
		// Generic fallback: a sorted "k=v" join so a pattern can still
		// scope MCP/custom tools by an argument value if needed. The gate
		// does not know which value is a path, so a value that carries a
		// `..` segment (`path=/work/../etc/passwd` against
		// `mcp__fs__read(path=/work/**)`) keeps the join from patterns, as
		// a climbing path spelling does.
		keys := make([]string, 0, len(input))
		for k := range input {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		climbs := false
		for _, k := range keys {
			v := fmt.Sprint(input[k])
			climbs = climbs || pathClimbs(v)
			parts = append(parts, k+"="+v)
		}
		joined := []string{strings.Join(parts, " ")}
		if climbs {
			return candidates{exact: joined}
		}
		return both(joined)
	}
}

// fetchCandidates renders a fetched URL's candidates: its host, as
// `domain:<host>` and bare, and the URL itself.
//
// A URL's path is a path too. A fetcher resolves its dot segments before it
// asks the server (RFC 3986 §5.2.4; a WHATWG parser also reads `%2e%2e` as
// `..` and, for http(s), `\` as `/`), so `https://e.com/docs/%2e%2e/admin`
// holds the text of `WebFetch(https://e.com/docs/**)` and reaches /admin —
// the class pathCandidates closes for files. A URL whose decoded path climbs,
// and one the gate cannot parse and so cannot read, is offered to exact rules
// only; its host still stands for it, a dot segment moving no host.
func fetchCandidates(raw string) candidates {
	var out candidates
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Host != "" {
		for _, host := range []string{"domain:" + parsed.Host, parsed.Host} {
			out.exact = append(out.exact, host)
			out.pattern = append(out.pattern, host)
		}
	}
	if raw == "" {
		return out
	}
	out.exact = append(out.exact, raw)
	if err == nil && !urlPathClimbs(parsed) {
		out.pattern = append(out.pattern, raw)
	}
	return out
}

// urlPathClimbs reports whether a parsed URL's path carries a `..` segment
// once decoded — url.Parse decodes the path, so `%2e%2e` (and `%2f`) are
// read the way a server reads them — with `\` read as the separator a WHATWG
// parser makes of it. An opaque URL (`https:e.com/../x`) is read the same
// way: such a parser takes its host from it.
func urlPathClimbs(u *url.URL) bool {
	return pathClimbs(strings.ReplaceAll(u.Path, `\`, "/")) ||
		pathClimbs(strings.ReplaceAll(u.Opaque, `\`, "/"))
}

// normalizedFetchURL renders the location a fetched URL reaches once its
// path is decoded and its dot segments and repeated separators resolved —
// the spelling a closing rule names (`deny: ["WebFetch(https://e.com/admin/**)"]`)
// that `https://e.com/./%61dmin/x` reaches without carrying. "" when the gate
// cannot read the URL or it is already in that form. The lexical cleanup is
// one reading among the fetchers' and servers'; the closing side reads it
// BESIDE the URL as sent, so a reading that differs costs a refusal at most.
func normalizedFetchURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" || u.Path == "" {
		return ""
	}
	p := strings.ReplaceAll(u.Path, `\`, "/")
	clean := path.Clean("/" + p)
	last := p[strings.LastIndex(p, "/")+1:]
	if clean != "/" && (strings.HasSuffix(p, "/") || last == "." || last == "..") {
		clean += "/"
	}
	n := *u
	n.Path, n.RawPath = clean, ""
	n.Fragment, n.RawFragment = "", ""
	if s := n.String(); s != raw {
		return s
	}
	return ""
}

// globPatternClimbs reports whether a call is a Glob whose pattern carries
// a `..` segment. Only the glob family reads its pattern as a PATH under
// the root; Grep's pattern is a regular expression over file contents, and
// `..` there means two characters.
func globPatternClimbs(canon, pattern string) bool {
	return canon == "glob" && pathClimbs(pattern)
}

// denyCandidates renders the candidates a DENY rule is matched against:
// everything summarize offers (opening, its exact list), plus every OTHER
// filesystem path the call carries. A tool call is free to name two —
// `{"file_path": "/work/ok", "path": "/run/iterion/secrets/x"}` — and which
// one the tool honours depends on the backend: claw's read_file reads
// `path`, claude_code's Read reads `file_path`. Reading all of them keeps
// "a deny wins in every mode" true whichever key the tool picks, while an
// allow stays on the single path summarize chose, or naming an allowed path
// beside a forbidden one would authorise the forbidden one.
//
// The path spellings are also offered FOLDED (foldedPathDimension): a deny
// is a closing verdict, so the fold can only cost a refusal, never a leak
// — the asymmetry the ask side shares and the allow side does not.
func denyCandidates(canon string, input map[string]any, root string, opening []string) []string {
	cands := slices.Clone(opening)
	if canon == "webfetch" {
		cands = appendUnique(cands, normalizedFetchURL(str(input, "url")))
	}
	if searchesByPattern(canon) {
		// summarize withholds a climbing glob pattern from the opening
		// side; a closing rule still reads it as sent.
		cands = appendUnique(cands, str(input, "pattern"))
	}
	// The path summarize read is already expanded; expanding a repeated
	// value again would pay its filesystem walk twice for the same
	// spellings. A climbing glob had none read there, so its root is
	// expanded below with the rest.
	var expanded []string
	if !globPatternClimbs(canon, str(input, "pattern")) {
		read := primaryPath(canon, input)
		expanded = append(expanded, read)
		for _, c := range foldedPathDimension(canon, read, root) {
			cands = appendUnique(cands, c)
		}
	}
	for _, raw := range closingPaths(canon, input) {
		if slices.Contains(expanded, raw) {
			continue
		}
		expanded = append(expanded, raw)
		for _, c := range pathDimension(canon, raw, root).exact {
			cands = appendUnique(cands, c)
		}
		for _, c := range foldedPathDimension(canon, raw, root) {
			cands = appendUnique(cands, c)
		}
	}
	return cands
}

// closingPaths lists every filesystem path a closing verdict reads for a
// call: the value of each path key the tool family carries, and — for a
// glob whose pattern climbs out of its root — where the climb lands: the
// pattern joined onto the root (`/work/a` + `../../run/x/*` reads
// `/run/x/*`, claw's placement), and an absolute pattern as written (the
// placement of a backend that honours it). The lexical join reads each glob
// segment as one directory; a globstar that a backend lets match zero would
// climb one level higher, which is why the opening side places no climbing
// glob at all.
func closingPaths(canon string, input map[string]any) []string {
	paths := []string{}
	for _, key := range pathInputKeys(canon) {
		paths = appendUnique(paths, str(input, key))
	}
	if pattern := str(input, "pattern"); globPatternClimbs(canon, pattern) {
		paths = appendUnique(paths, filepath.Join(cmp.Or(primaryPath(canon, input), "."), pattern))
		if filepath.IsAbs(pattern) {
			paths = appendUnique(paths, pattern)
		}
	}
	return paths
}

// foldedPathDimension renders the case/slash-folded twin of every spelling
// pathDimension offers, in the doctrine of the read-scope guard
// (CredentialReadGuard, iterion#2868): a file system that folds case makes
// `.ENV` and `.env` the same file, so a rule compared in one spelling only
// is a bypass of itself, and filepath.ToSlash folds the separator where
// the platform treats `\` as one. The fold is offered on the CLOSING side
// of the gate alone (deny, and the ask that outranks an allow): on a
// case-sensitive file system a folded spelling may name a DIFFERENT file,
// so a deny over-refusing costs a refusal while an allow over-matching
// would hand over a path the operator never wrote — summarize's candidates
// stay spellings of the same file.
func foldedPathDimension(canon, raw, root string) []string {
	out := []string{}
	for _, s := range pathDimension(canon, raw, root).exact {
		out = appendUnique(out, foldPathSpelling(s))
	}
	return out
}

// foldPathSpelling folds one path spelling for comparison: slash-spelled
// and case-folded. The `path:` dimension prefix is preserved so a rule
// naming the search root explicitly still meets the folded candidate.
func foldPathSpelling(s string) string {
	if rest, ok := strings.CutPrefix(s, "path:"); ok {
		return "path:" + foldPathSpelling(rest)
	}
	return strings.ToLower(filepath.ToSlash(s))
}

// pathInputKeys names every input key that holds a filesystem path for a
// tool family, primaryPath the one summarize reads — the first present,
// in the order the gate has always read them. A deny is matched against
// all of them, so the two must not drift: one table, read twice.
//
// A new path-carrying tool joins THREE tables together, no enumeration
// derives them: this one (which input keys hold the paths), summarize
// (what the call's candidates are), and pathTreeTools (what an
// engine-armed tree deny covers).
func pathInputKeys(canon string) []string {
	switch canon {
	case "read", "write", "edit", "notebookedit":
		return []string{"file_path", "path", "notebook_path"}
	case "glob", "grep", "workspace_grep", "ls":
		return []string{"path"}
	}
	return nil
}

func primaryPath(canon string, input map[string]any) string {
	for _, key := range pathInputKeys(canon) {
		if v := str(input, key); v != "" {
			return v
		}
	}
	return ""
}

// pathDimension renders the candidates one filesystem path contributes
// for this tool family: every spelling of the path, the same spellings
// with a trailing separator when the tool reads the whole tree under the
// path, and — for the search tools, whose bare argument is otherwise the
// search PATTERN — all of them under a `path:` prefix, so a rule can name
// the dimension explicitly (`Grep(path:/run/iterion/secrets/**)`), the
// way WebFetch rules name a host with `domain:`. Each spelling keeps the
// half pathCandidates sorted it into: neither the separator nor the prefix
// changes whether it climbs.
func pathDimension(canon, raw, root string) candidates {
	spellings := pathCandidates(raw, root)
	return candidates{
		pattern: pathDimensionOf(canon, spellings.pattern),
		exact:   pathDimensionOf(canon, spellings.exact),
	}
}

// pathDimensionOf is pathDimension for one list of spellings.
func pathDimensionOf(canon string, spellings []string) []string {
	out := []string{}
	for _, s := range spellings {
		out = appendUnique(out, s)
		if readsADirectoryTree(canon) {
			// A call ROOTED at a directory reads what is under it, which
			// is exactly what a `dir/**` rule names. Without the trailing
			// separator, `deny: ["Grep(/run/iterion/secrets/**)"]` would
			// miss the one call that walks the whole directory.
			out = appendUnique(out, strings.TrimSuffix(s, "/")+"/")
		}
	}
	if !searchesByPattern(canon) {
		return out
	}
	named := make([]string, 0, 2*len(out))
	named = append(named, out...)
	for _, s := range out {
		named = appendUnique(named, "path:"+s)
	}
	return named
}

// searchesByPattern reports whether a tool's bare argument scope is read
// as a search pattern rather than a path, which is what makes the `path:`
// prefix necessary to name the other dimension.
func searchesByPattern(canon string) bool {
	switch canon {
	case "glob", "grep", "workspace_grep":
		return true
	}
	return false
}

// readsADirectoryTree reports whether a tool's path argument is a
// container it reads THROUGH (a search root, a listing) rather than the
// single file it opens.
func readsADirectoryTree(canon string) bool {
	return searchesByPattern(canon) || canon == "ls"
}

// pathCandidates is THE normalisation of one model-supplied filesystem
// path — every path the gate matches against a rule, on either side,
// passes through it. It renders the spellings a scoped rule may
// legitimately match: the string the model sent, its lexical cleanup
// (`/run//iterion/./secrets/x` → `/run/iterion/secrets/x`), its absolute
// form against the directory the agent runs in, and the form with the
// symlinks of its existing prefix resolved. Every spelling names the SAME
// file, so offering them can only make a rule that names that file match;
// none of them can make a rule that matched stop matching.
//
// It also sorts them by what may read them (candidates): a spelling that
// still carries a `..` segment — the string as sent
// (`/run/iterion/secrets/../../../etc/passwd`), or a relative cleanup that
// climbs above its anchor (`../etc/passwd`) — names the file, but its text
// names a directory the file is not under, so a PATTERN never reads it.
// The anchored spellings stand in for it: the cleanup of an absolute path,
// and the absolute form a captured root gives a relative one. With no root
// a climbing relative path has no anchored spelling at all, and only an
// exact or unbounded rule can vouch for it.
//
// The rule's own text is never rewritten — the gate matches what the
// operator wrote — so a rule naming a path FRAGMENT still wraps it in
// stars (`Read(**.env)`, docs/permissions.md).
func pathCandidates(raw, root string) candidates {
	var out candidates
	if strings.TrimSpace(raw) == "" {
		return out
	}
	out.add(raw)
	cleaned := filepath.Clean(raw)
	out.add(cleaned)
	abs := cleaned
	if !filepath.IsAbs(abs) && filepath.IsAbs(root) {
		abs = filepath.Join(root, cleaned)
		out.add(abs)
	}
	out.add(resolveExistingPrefix(abs))
	return out
}

// resolveExistingPrefix resolves the symlinks of the longest EXISTING
// prefix of an absolute path and re-joins the rest, returning "" when
// there is nothing to add. A plain EvalSymlinks answers nothing for a
// path that does not exist yet — which is every Write to a new file, the
// call that needs the check most: with `/tmp/s` a symlink to
// `/run/iterion/secrets`, a write to `/tmp/s/new` must still meet the
// deny that names the real directory.
//
// A relative path is left alone: EvalSymlinks would resolve it against
// THIS process's directory, which is not the directory the agent runs in
// (and in a sandboxed backend not even the same filesystem).
//
// The climb is bounded because the path is MODEL-supplied: each step is a
// symlink resolution of its own, so a path with thousands of segments
// would buy the agent a quadratic pile of lstats per gated call. Past the
// bound the lexical spellings still apply — the gate loses a candidate,
// never a rule.
func resolveExistingPrefix(p string) string {
	if !filepath.IsAbs(p) {
		return ""
	}
	rest := ""
	for cur, climbed := p, 0; climbed < maxPrefixClimb; climbed++ {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if rest != "" {
				resolved = filepath.Join(resolved, rest)
			}
			if resolved == p {
				return "" // nothing a symlink hid
			}
			return resolved
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "" // walked to the root without a readable prefix
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
	return "" // deeper than the bound
}

// maxPrefixClimb bounds resolveExistingPrefix's climb. A workspace path
// is a dozen segments deep; the bound is there for a path that was
// written to exhaust the gate, not for a real one.
const maxPrefixClimb = 64

// appendUnique appends s unless it is empty or already present. The
// candidate list is short (a handful of spellings), so a linear scan is
// cheaper than a map and keeps the order meaningful.
func appendUnique(dst []string, s string) []string {
	if s == "" {
		return dst
	}
	for _, existing := range dst {
		if existing == s {
			return dst
		}
	}
	return append(dst, s)
}

func str(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// IsInfrastructureTool reports whether a tool is iterion's own
// runtime plumbing (the interaction + capability surface) rather than
// an agent action against the environment. These are always allowed by
// the gate. Covers both backends' spellings: ask_user (claw) /
// mcp__iterion__ask_user (claude_code), the board / control / watch MCP
// families, and the iterion-internal __mcp-* servers. Exported so the
// resume path can tell a real ask_user pause from a permission pause.
func IsInfrastructureTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "ask_user", "askuser", "send_user_message":
		return true
	case "structuredoutput", "structured_output":
		// The synthetic tool iterion injects to collect a node's `output:`
		// schema (claude_code: StructuredOutput, claw: structured_output).
		// It is how the agent RETURNS its result, not an action against the
		// environment — gating it would break every schema'd node.
		return true
	}
	// Match iterion's reserved MCP namespace regardless of the separator
	// spelling a backend uses for the FQN — claude_code double-underscore
	// (mcp__iterion_board__create), claw single-underscore
	// (mcp_iterion_board_create), and dotted (mcp.iterion_board.create) all
	// normalise to the same `mcp_iterion…` form. Keying on the namespace
	// (a structural invariant every internal server upholds) rather than an
	// exact-name list is what makes the exemption robust across backends.
	n := normalizeMCPName(name)
	return strings.HasPrefix(n, "mcp_iterion") || strings.HasPrefix(n, "_mcp")
}

// IsReservedMCPServerName reports whether a user server could place tools in
// the infrastructure namespace. Use the gate's own FQN normalization so
// catalog admission and CLI forwarding cannot disagree with the exemption.
// The probe also catches empty/separator-only names: those would let a tool
// named iterion_* collapse into the reserved namespace on its own.
func IsReservedMCPServerName(name string) bool {
	return IsInfrastructureTool("mcp__" + strings.TrimSpace(name) + "__iterion_probe")
}

// normalizeMCPName lower-cases a tool name and collapses MCP separator
// variants (`__`, `.`) to single underscores so the iterion namespace can
// be recognised in any backend's spelling.
func normalizeMCPName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.ReplaceAll(n, ".", "_")
	for strings.Contains(n, "__") {
		n = strings.ReplaceAll(n, "__", "_")
	}
	return n
}

// canonicalToolName maps a backend-specific tool name to a stable key
// so a single rule (e.g. `Bash(...)`) gates the matching tool across
// claude_code's TitleCase names, claw/pi's lowercase names, Grok's native
// verbs, and Kimi's FetchURL spelling.
// Unknown names are lower-cased and stripped of separators so a custom
// rule still matches its own spelling.
func canonicalToolName(name string) string {
	return toolcatalog.CanonicalToolName(name)
}
