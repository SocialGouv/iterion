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
// `mcp__github__get_*`). Because the same Policy drives both backends,
// backend-native tool names reach identical decisions — see
// [canonicalToolName].
package permission

import (
	"cmp"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
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

// Policy is a resolved permission policy: a mode plus three ordered
// rule lists. The zero value (ModeOff, no rules) is a disabled gate.
type Policy struct {
	Mode   Mode
	allow  []rule
	ask    []rule
	deny   []rule
	scoped bool // any rule has an (arg) pattern → Evaluate must summarize
	// exempt is the set of tool names the runtime explicitly marked as its
	// own infrastructure (registration-linked, in addition to the reserved
	// MCP namespace IsInfrastructureTool recognises). Keyed by canonical
	// name. See MarkExempt.
	exempt map[string]bool
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
	Exempt []string `json:"exempt,omitempty"`
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
		Mode:   p.Mode.String(),
		Allow:  raw(p.allow),
		Ask:    raw(p.ask),
		Deny:   raw(p.deny),
		Exempt: exempt,
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
	p.MarkExempt(cfg.Exempt...)
	return p, nil
}

// CanAsk reports whether evaluating this policy can produce an Ask decision.
// ModeDeny is not sufficient by itself: an explicit ask rule has precedence
// over the mode default.
func (p *Policy) CanAsk() bool {
	return p != nil && (p.Mode == ModeAsk || len(p.ask) > 0)
}

// NewPolicy parses the rule strings into a Policy. A malformed rule is
// an error (surfaced as a compile diagnostic upstream). Rule order
// within each list is preserved; cross-list precedence is deny → ask →
// allow (see [Policy.Evaluate]).
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

// AddAllowRule appends an allow rule (used by "allow always" on
// resume — the operator's grant persists for the rest of the run).
// A malformed rule is ignored (it came from a structured decision, not
// user free-text, so this is defensive only).
func (p *Policy) AddAllowRule(raw string) {
	if r, err := parseRule(raw); err == nil {
		if r.argRe != nil {
			p.scoped = true
		}
		p.allow = append(p.allow, r)
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

// isExempt reports whether a tool call must bypass the gate — either the
// runtime explicitly marked it (MarkExempt) or it lives in iterion's
// reserved MCP namespace (IsInfrastructureTool).
func (p *Policy) isExempt(toolName string) bool {
	if p.exempt[canonicalToolName(toolName)] {
		return true
	}
	return IsInfrastructureTool(toolName)
}

// Evaluate decides what to do with a tool call. Precedence mirrors
// Claude Code's evaluation order (deny rules → ask rules → allow rules
// → permission mode):
//
//  1. a matching deny rule → Deny (wins in every mode);
//  2. a matching ask rule  → Ask;
//  3. a matching allow rule → Allow;
//  4. otherwise the mode default — ModeAsk → Ask, ModeDeny → Deny,
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
	canon := canonicalToolName(toolName)
	// summarize is only needed by scoped (arg-pattern) rules; skip the
	// work entirely when every rule is a bare tool name (the common
	// `deny: [Bash]` / `allow: [*]` shapes).
	summary := ""
	if p.scoped {
		summary = summarize(canon, input)
	}
	if r, ok := matchAny(p.deny, toolName, canon, summary); ok {
		return Deny, r.raw
	}
	if r, ok := matchAny(p.ask, toolName, canon, summary); ok {
		return Ask, r.raw
	}
	if r, ok := matchAny(p.allow, toolName, canon, summary); ok {
		return Allow, r.raw
	}
	switch p.Mode {
	case ModeDeny:
		return Deny, ""
	default: // ModeAsk
		return Ask, ""
	}
}

// rule is a parsed permission entry: a tool matcher plus an optional
// argument matcher. A nil argRe is a bare tool rule (matches any
// invocation of the tool).
type rule struct {
	raw    string         // original text, for messages + forwarding
	tool   string         // canonical tool key, or "*" (any) — literal case
	toolRe *regexp.Regexp // non-nil when the tool name itself is a glob
	argRe  *regexp.Regexp // non-nil when the rule scoped an (arg) pattern
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
		argRe, err := compileArg(s[open+1 : len(s)-1])
		if err != nil {
			return rule{}, fmt.Errorf("bad arg pattern: %w", err)
		}
		r.argRe = argRe
	}
	toolPart = strings.TrimSpace(toolPart)
	if toolPart == "" {
		return rule{}, fmt.Errorf("missing tool name")
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
// name + canonical key) and, for scoped rules, the input summary.
func matchAny(rs []rule, rawName, canon, summary string) (rule, bool) {
	for _, r := range rs {
		if !r.matchesTool(rawName, canon) {
			continue
		}
		if r.argRe == nil {
			return r, true // bare tool rule matches any invocation
		}
		// summarize may emit several candidate strings (one per line) —
		// e.g. WebFetch yields "domain:<host>", "<host>" and the full URL
		// — and a scoped rule matches if it matches ANY candidate.
		for _, cand := range strings.Split(summary, "\n") {
			if r.argRe.MatchString(cand) {
				return r, true
			}
		}
	}
	return rule{}, false
}

func (r rule) matchesTool(rawName, canon string) bool {
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

// summarize renders a tool's input into the string(s) that scoped rule
// patterns match against, mirroring Claude Code's per-tool summaries
// (Bash → command, Read/Edit/Write → path, WebFetch → domain + url).
// Multiple candidates are joined with '\n' so a rule pattern matches if
// it matches ANY candidate (e.g. WebFetch by domain OR full url). canon
// is the already-canonicalised tool name. The per-tool argument keys
// mirror tooldisplay's header-detail extraction; keep the two in sync
// when adding a tool.
func summarize(canon string, input map[string]any) string {
	switch canon {
	case "bash":
		return str(input, "command")
	case "read", "write", "edit", "notebookedit":
		return cmp.Or(str(input, "file_path"), str(input, "path"), str(input, "notebook_path"))
	case "glob", "grep":
		return cmp.Or(str(input, "pattern"), str(input, "path"))
	case "ls":
		return str(input, "path")
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
		return strings.Join(cands, "\n")
	case "websearch":
		return str(input, "query")
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
		return strings.Join(parts, " ")
	}
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
	n := strings.ToLower(strings.TrimSpace(name))
	if alias, ok := toolAliases[n]; ok {
		return alias
	}
	// MCP FQNs (mcp__server__tool / mcp_server_tool) are kept verbatim
	// (lower-cased) so server-scoped globs match.
	if strings.HasPrefix(n, "mcp__") || strings.HasPrefix(n, "mcp_") {
		return n
	}
	return n
}

// toolAliases collapses cross-backend synonyms onto a canonical key.
var toolAliases = map[string]string{
	// shell
	"bash": "bash", "shell": "bash", "sh": "bash", "run_terminal_command": "bash",
	// read
	"read": "read", "read_file": "read", "readfile": "read", "cat": "read",
	// write
	"write": "write", "write_file": "write", "writefile": "write",
	// edit
	"edit": "edit", "edit_file": "edit", "file_edit": "edit",
	"multiedit": "edit", "edit_mode": "edit", "str_replace": "edit", "search_replace": "edit",
	// notebook
	"notebookedit": "notebookedit", "notebook_edit": "notebookedit",
	// search. pi names its glob tool `find` (it takes a `pattern`, not a
	// directory to walk), so without the alias a `Glob(**)` rule silently
	// fails to match it and the gate reaches a different verdict on pi than
	// on the other backends.
	"glob": "glob", "find": "glob", "grep": "grep",
	// web
	"webfetch": "webfetch", "web_fetch": "webfetch", "fetchurl": "webfetch", "web": "webfetch",
	"websearch": "websearch", "web_search": "websearch",
	// misc
	"ls": "ls", "list_dir": "ls", "todowrite": "todowrite", "todo_write": "todowrite",
	"agent": "agent", "task": "agent", "spawn_subagent": "agent",
	"use_tool": "use_tool",
}
