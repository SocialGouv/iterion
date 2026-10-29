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
	"fmt"
	"net/url"
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
	Mode   Mode
	allow  []rule
	ask    []rule
	deny   []rule
	grants []rule // operator-approved, engine-owned rules; never declared in a workflow
	scoped bool   // any rule has an (arg) pattern → Evaluate must summarize
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
		Mode:     p.Mode.String(),
		Allow:    raw(p.allow),
		Ask:      raw(p.ask),
		Deny:     raw(p.deny),
		Grants:   raw(p.grants),
		Exempt:   exempt,
		PathRoot: p.pathRoot,
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
	if r, ok := p.matchingDeny(toolName, input); ok {
		return Deny, r.raw
	}
	canon, cands := p.toolSummary(toolName, input)
	if r, ok := matchAny(p.grants, toolName, canon, cands); ok {
		return Allow, r.raw
	}
	// A scoped ask is deliberately stronger than every allow. This lets an
	// operator re-gate a single otherwise auto-allowed command.
	if r, ok := matchScoped(p.ask, toolName, canon, cands, false); ok {
		return Ask, r.raw
	}
	// A bare exact-tool ask is a generic fallback, so an authored literal
	// allow for one complete argument can safely make that one invocation
	// automatic. Do not extend this to tool globs, wildcard/prefix arguments,
	// or catch-all asks: those remain approval gates.
	if r, ok := matchBareExactTool(p.ask, toolName, canon); ok {
		if allow, ok := matchScoped(p.allow, toolName, canon, cands, true); ok {
			return Allow, allow.raw
		}
		return Ask, r.raw
	}
	if r, ok := matchAny(p.ask, toolName, canon, cands); ok {
		return Ask, r.raw
	}
	if r, ok := matchAny(p.allow, toolName, canon, cands); ok {
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

func (p *Policy) matchingDeny(toolName string, input map[string]any) (rule, bool) {
	canon := canonicalToolName(toolName)
	var cands []string
	if p.scoped {
		cands = atLeastOne(denyCandidates(canon, input, p.pathRoot))
	}
	return matchAny(p.deny, toolName, canon, cands)
}

func (p *Policy) toolSummary(toolName string, input map[string]any) (string, []string) {
	canon := canonicalToolName(toolName)
	// summarize is only needed by scoped (arg-pattern) rules; skip the work
	// entirely when every rule is bare (the common deny: [Bash] shape).
	var cands []string
	if p.scoped {
		cands = atLeastOne(summarize(canon, input, p.pathRoot))
	}
	return canon, cands
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
	// re-read a rule's spelling to learn how wide it is.
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
		r.anyArg = trimmedArg == "*" || trimmedArg == "**"
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
// It parses with the REAL grammar Evaluate matches with, so it has no
// spelling of its own to keep in step.
func RuleRemovesTool(rawRule, toolName string) bool {
	r, err := parseRule(rawRule)
	if err != nil {
		return false
	}
	if !r.matchesTool(toolName, canonicalToolName(toolName)) {
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

// matchAny returns the first rule in rs that matches the tool (by its raw
// name + canonical key) and, for scoped rules, one of the input candidates.
//
// A few calls deliberately expose multiple safe candidates (WebFetch's
// domain and full URL, a path's spellings, a search's pattern and root —
// see summarize). Shell commands are different: a newline is part of one
// command payload, not a second candidate, which is why summarize returns
// a shell command as a single candidate instead of a joined string a
// matcher would have to remember not to split.
func matchAny(rs []rule, rawName, canon string, cands []string) (rule, bool) {
	for _, r := range rs {
		if !r.matchesTool(rawName, canon) {
			continue
		}
		if r.matchesSummary(cands) {
			return r, true
		}
	}
	return rule{}, false
}

// matchScoped returns a matching argument-scoped rule. When literalOnly is
// true, it accepts neither wildcard/prefix argument scopes nor tool globs.
func matchScoped(rs []rule, rawName, canon string, cands []string, literalOnly bool) (rule, bool) {
	for _, r := range rs {
		if r.argRe == nil || (literalOnly && (!r.literalArg || r.toolRe != nil)) {
			continue
		}
		if r.matchesTool(rawName, canon) && r.matchesSummary(cands) {
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
// when adding a tool. The FIRST candidate is the one an operator approval
// is scoped to and the one messages quote (messages.go), so a new
// candidate is appended, never prepended.
func summarize(canon string, input map[string]any, root string) []string {
	switch canon {
	case "bash", "diagnostic_shell":
		// One opaque candidate whatever it contains: a newline is part of
		// the command payload, and splitting it would let an exact allow
		// for the first line approve arbitrary trailing shell text.
		return []string{str(input, "command")}
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
		cands := []string{}
		if pattern := str(input, "pattern"); pattern != "" {
			cands = append(cands, pattern)
		}
		return append(cands, pathDimension(canon, primaryPath(canon, input), root)...)
	case "webfetch":
		u := str(input, "url")
		host := ""
		if parsed, err := url.Parse(u); err == nil {
			host = parsed.Host
		}
		cands := []string{}
		if host != "" {
			cands = append(cands, "domain:"+host, host)
		}
		if u != "" {
			cands = append(cands, u)
		}
		return cands
	case "websearch":
		return []string{str(input, "query")}
	default:
		// Generic fallback: a sorted "k=v" join so a pattern can still
		// scope MCP/custom tools by an argument value if needed.
		keys := make([]string, 0, len(input))
		for k := range input {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+fmt.Sprint(input[k]))
		}
		return []string{strings.Join(parts, " ")}
	}
}

// denyCandidates renders the candidates a DENY rule is matched against:
// everything summarize offers, plus every OTHER filesystem path the call
// carries. A tool call is free to name two — `{"file_path": "/work/ok",
// "path": "/run/iterion/secrets/x"}` — and which one the tool honours
// depends on the backend: claw's read_file reads `path`, claude_code's
// Read reads `file_path`. Reading all of them keeps "a deny wins in every
// mode" true whichever key the tool picks, while an allow stays on the
// single path summarize chose, or naming an allowed path beside a
// forbidden one would authorise the forbidden one.
func denyCandidates(canon string, input map[string]any, root string) []string {
	cands := summarize(canon, input, root)
	// The primary path is already expanded; expanding a repeated value
	// again would pay its filesystem walk twice for the same spellings.
	expanded := []string{primaryPath(canon, input)}
	for _, key := range pathInputKeys(canon) {
		raw := str(input, key)
		if raw == "" || slices.Contains(expanded, raw) {
			continue
		}
		expanded = append(expanded, raw)
		for _, c := range pathDimension(canon, raw, root) {
			cands = appendUnique(cands, c)
		}
	}
	return cands
}

// pathInputKeys names every input key that holds a filesystem path for a
// tool family, primaryPath the one summarize reads — the first present,
// in the order the gate has always read them. A deny is matched against
// all of them, so the two must not drift: one table, read twice.
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
// way WebFetch rules name a host with `domain:`.
func pathDimension(canon, raw, root string) []string {
	out := []string{}
	for _, s := range pathCandidates(raw, root) {
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

// pathCandidates renders the spellings of ONE filesystem path a scoped
// rule may legitimately match: the string the model sent, its lexical
// cleanup (`/run//iterion/./secrets/x` → `/run/iterion/secrets/x`), its
// absolute form against the directory the agent runs in, and the form
// with the symlinks of its existing prefix resolved. Every spelling names
// the SAME file, so offering them can only make a rule that names that
// file match; none of them can make a rule that matched stop matching.
//
// The rule's own text is never rewritten — the gate matches what the
// operator wrote — so a rule naming a path FRAGMENT still wraps it in
// stars (`Read(**.env)`, docs/permissions.md).
func pathCandidates(raw, root string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := []string{raw}
	cleaned := filepath.Clean(raw)
	out = appendUnique(out, cleaned)
	abs := cleaned
	if !filepath.IsAbs(abs) && filepath.IsAbs(root) {
		abs = filepath.Join(root, cleaned)
		out = appendUnique(out, abs)
	}
	return appendUnique(out, resolveExistingPrefix(abs))
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

// atLeastOne keeps the empty candidate that a call with no argument has
// always offered: a scoped wildcard rule (`Bash(*)`, `Read(*)`) matches
// it, and a deny spelled that way must keep matching it.
func atLeastOne(cands []string) []string {
	if len(cands) == 0 {
		return []string{""}
	}
	return cands
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
