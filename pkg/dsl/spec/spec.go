// Package spec is the declarative registry of the `.bot` DSL's property
// surface: for every declaration kind, node kind and anonymous block, the
// properties it accepts, each with the shape of its value and one line of
// doc. It is a LEAF (no iterion import): the parser reads it to explain an
// unknown property (the closest accepted names, the block a name belongs
// to, the kind's full list), the renderers turn it into the grammar
// reference and the skill's property section, and a conformance test holds
// it to the parser in BOTH directions — every property listed here is
// accepted by the parser, every property the parser accepts is listed here
// — so a property added to one side without the other fails CI instead of
// drifting for months (the three canonical examples of the DSL quickref did
// not parse until #1010's lot 0 rewrote them).
//
// The registry DESCRIBES the parser; it does not drive it (a generic parser
// loop over it is lot 1b of #1010, an option). What parses is decided by
// the parser's switch arms; this is the copy that cannot drift because the
// test reads both.
package spec

import "sort"

// Form is the shape of a property's value as the parser reads it.
type Form string

const (
	String        Form = "string"             // a quoted string: "…", `…` or a `|` block scalar
	Ident         Form = "ident"              // a bare identifier (a name or a reference)
	StringOrIdent Form = "string|ident"       // a quoted string or a bare identifier
	Int           Form = "int"                // an unquoted integer
	Number        Form = "number"             // an unquoted integer or float
	Bool          Form = "bool"               // true or false
	Enum          Form = "enum"               // one of Property.Values, bare
	IdentList     Form = "ident list"         // [a, b]
	StringList    Form = "string list"        // ["a", "b"]
	ToolList      Form = "tool list"          // [bash, mcp.server.*, "quoted-literal"]
	SkillList     Form = "skill list"         // ["kebab-name", dotted.ident]
	MixedList     Form = "string|ident list"  // ["!**.evil.site", github.com]
	IdentOrList   Form = "ident | ident list" // godot or [godot, blender]
	Map           Form = "map"                // { KEY: "v" } inline, or an indented `KEY: v` block
	WithMap       Form = "with { … }"         // with { key: "value", … }
	Block         Form = "block"              // an indented block whose body is the kind named in Property.Body
	BlockOrIdent  Form = "ident | block"      // a bare mode on the header line, or an indented block
)

// Property is one `name: value` line a kind accepts.
type Property struct {
	Name string
	Form Form
	// Values are the accepted bare values of an Enum. For an Ident or a
	// StringOrIdent they are the values a COMPILE check accepts (the parser
	// takes any word; the compiler names the code), listed for the reader.
	Values []string
	// Body names the kind whose properties fill a Block / BlockOrIdent.
	Body string
	Doc  string
}

// Role says what a kind is in the grammar.
type Role string

const (
	Declaration Role = "declaration" // `kind name:` at the top level (prompt, schema, cursor, …)
	Node        Role = "node"        // a graph node declaration (agent, tool, …)
	BlockRole   Role = "block"       // an anonymous block opened by a keyword or a property (budget:, mcp:, …)
	Entry       Role = "entry"       // an author-named entry inside a block (an attachment, a secret, a fallback route)
)

// Entries describes the author-named entries a block's body is made of,
// when its body is not (only) a fixed property table.
type Entries struct {
	Shape string // the entry line, e.g. `name: type [enum: "a", "b"] [= default]`
	Doc   string
	Body  string // the kind describing an entry's own sub-block, if it has one
}

// Kind is one declaration kind, node kind or block of the grammar.
type Kind struct {
	Name string // the name the parser's diagnostics use: "agent", "sandbox.network", …
	Role Role
	// Opener is, for a block or an entry, the keyword or property that opens
	// it inside its host: "network" for sandbox.network, "fallbacks" for a
	// fallback route.
	Opener string
	// Hosts are the kinds whose body may contain this one; "file" is the top
	// level. Empty for a top-level declaration.
	Hosts []string
	// Header is the declaration line when it is not the default
	// `<kind> <name>:` (a group carries parameters, a use has no body).
	Header     string
	Doc        string
	Properties []Property
	Entries    *Entries
}

// Names returns the kind's property names in declaration order.
func (k Kind) Names() []string {
	out := make([]string, 0, len(k.Properties))
	for _, p := range k.Properties {
		out = append(out, p.Name)
	}
	return out
}

// Property returns the kind's property of that name.
func (k Kind) Property(name string) (Property, bool) {
	for _, p := range k.Properties {
		if p.Name == name {
			return p, true
		}
	}
	return Property{}, false
}

// Has reports whether the kind accepts a property of that name.
func (k Kind) Has(name string) bool {
	_, ok := k.Property(name)
	return ok
}

// Lookup returns the kind of that name.
func Lookup(name string) (Kind, bool) {
	for _, k := range Kinds {
		if k.Name == name {
			return k, true
		}
	}
	return Kind{}, false
}

// Owners returns, sorted, the names of the kinds that accept a property of
// that name.
func Owners(name string) []string {
	var out []string
	for _, k := range Kinds {
		if k.Has(name) {
			out = append(out, k.Name)
		}
	}
	sort.Strings(out)
	return out
}

// HostedBy returns the kinds whose Hosts include host, in registry order.
func HostedBy(host string) []Kind {
	var out []Kind
	for _, k := range Kinds {
		for _, h := range k.Hosts {
			if h == host {
				out = append(out, k)
				break
			}
		}
	}
	return out
}

func prop(name string, form Form, doc string) Property {
	return Property{Name: name, Form: form, Doc: doc}
}

func enum(name, doc string, values ...string) Property {
	return Property{Name: name, Form: Enum, Values: values, Doc: doc}
}

// checked is an Ident the parser takes as any word and a later check — the
// compiler's, or the runtime's — narrows to values (the doc names the
// diagnostic when there is one).
func checked(name, doc string, values ...string) Property {
	return Property{Name: name, Form: Ident, Values: values, Doc: doc}
}

func block(name, body, doc string) Property {
	return Property{Name: name, Form: Block, Body: body, Doc: doc}
}

// Properties shared by every node kind that produces or consumes data.
var (
	pInput          = prop("input", Ident, "Schema the node's input is validated against")
	pOutput         = prop("output", Ident, "Schema the node's structured output must match")
	pPublish        = prop("publish", Ident, "Artifact name the output is published under (read back as {{artifacts.<name>}})")
	pArtifactLabels = prop("artifact_labels", ToolList, "Labels stamped on the published artifact; a quoted element is the literal label")
	pAwait          = enum("await", "Convergence rule when several incoming branches reach the node", "wait_all", "best_effort")
	pDescription    = prop("description", String, "Free-text description shown by the studio and the reports")
	pNeeds          = prop("needs", IdentOrList, "Resource(s) leased from the workflow's resources: block for the node's duration")
	pModel          = prop("model", String, "Model id the backend serves, e.g. \"anthropic/claude-opus-5\"; empty takes the backend's default")
	pBackend        = prop("backend", String, "Execution backend: claw, claude_code, codex, pi, kimi or grok")
	pProvider       = prop("provider", String, "Provider hint for credential resolution, e.g. \"anthropic\"")
	pSystem         = prop("system", Ident, "Prompt declaration used as the system prompt")
	pUser           = prop("user", Ident, "Prompt declaration used as the user message")
	pTimeout        = prop("timeout", String, "Duration the node may run, e.g. \"20m\"")
	pCompress       = checked("compress", "Command-output compression: on, ultra or off (C102)", "on", "ultra", "off")
	pPermission     = checked("permission", "Tool-permission gate: off, ask or deny (C110–C112)", "off", "ask", "deny")
	pAutoMemory     = checked("auto_memory", "The backend's own auto-memory: on or off (C131/C132)", "on", "off")
	pInteraction    = enum("interaction", "How the node asks the operator (ADR-081)", "none", "human", "llm", "llm_or_human", "review", "async")
	pInteractionP   = prop("interaction_prompt", Ident, "Prompt the llm interaction mode answers with in the operator's place")
	pInteractionM   = prop("interaction_model", String, "Model the llm interaction mode uses")
	pReasoning      = Property{Name: "reasoning_effort", Form: Enum, Values: []string{"low", "medium", "high", "xhigh", "max", "ultracode"},
		Doc: "Reasoning effort; ultracode is xhigh plus multi-agent orchestration, reliable on Opus 4.8 and the Claude 5 family (Opus 5, Fable 5.1) only (C089 warns elsewhere); a quoted string is env-substituted at runtime"}
	pSandbox = Property{Name: "sandbox", Form: BlockOrIdent, Body: "sandbox", Values: []string{"none", "auto"},
		Doc: "Sandbox for this scope: a bare mode (none, auto) or an indented block — the inline form, which needs image: or build: (C044)"}
)

// llmOnly marks a router property C023 refuses outside mode: llm.
func llmOnly(p Property) Property {
	p.Doc = "llm mode only (C023 otherwise): " + p.Doc
	return p
}

// llmProperties is the surface agents and judges share (one parser arm
// serves both).
var llmProperties = []Property{
	pDescription,
	pModel, pBackend, pProvider,
	prop("command", String, "Executable that drives a CLI backend, overriding its default binary"),
	pInput, pOutput, pPublish, pArtifactLabels,
	pSystem, pUser,
	enum("session", "How the node's LLM session relates to the previous node's", "fresh", "inherit", "inherit_if_available", "fork", "artifacts_only", "persist"),
	prop("tools", ToolList, "Tools the node may call; restricts claw (C135 on a name it lacks), inert on a CLI backend"),
	prop("tool_policy", ToolList, "Tool-policy entries applied on top of tools"),
	prop("capabilities", ToolList, "Board capabilities opened to the node: board.create, board.move, board.read, … (C080/C081)"),
	prop("skills", SkillList, "Skill-library skills mirrored into the run's .claude/skills"),
	prop("tool_max_steps", Int, "Upper bound on tool-call rounds in one execution"),
	prop("max_tokens", Int, "Output-token cap per call"),
	pReasoning,
	pTimeout,
	prop("readonly", Bool, "Declares the node mutates no workspace file, so it may run beside another branch"),
	prop("full_access", Bool, "Grants the backend its full tool access"),
	prop("images", StringList, "Image paths sent with the prompt"),
	pInteraction, pInteractionP, pInteractionM,
	pAwait,
	pCompress, pAutoMemory, pPermission,
	pNeeds,
	block("fallbacks", "fallback", "Ordered, NAMED alternative routes taken when the primary fails (ADR-087); a chain with no route is refused"),
	block("mcp", "mcp", "MCP servers active for the node"),
	block("compaction", "compaction", "Context-compaction thresholds of the node's session"),
	block("memory", "memory", "iterion's shared-memory tools and scopes for the node"),
	pSandbox,
	block("cursors", "cursors", "Prompt-engineering dials activated on the node (docs/cursors.md)"),
}

// Kinds is the registry, in the order the reference renders them.
var Kinds = []Kind{
	// ---- top-level declarations ----
	{Name: "prompt", Role: Declaration, Doc: "A named text block, referenced by `system:` / `user:` / `instructions:`; its body is free text with {{…}} references and {{include \"file\"}} directives. Blank lines in the body are dropped by the lexer; a bare header declares an empty prompt.",
		Entries: &Entries{Shape: "indented text lines", Doc: "Free text; the first line's indentation is stripped from every line"}},
	{Name: "schema", Role: Declaration, Doc: "A structured-output shape; a bare header declares an empty schema.",
		Entries: &Entries{Shape: `field: string | bool | int | float | json | string[] | file [enum: "a", "b"]`, Doc: "One field per line; `file` is valid only on the output schema of a human node whose interaction collects operator bytes (C129); the enum constraint applies to strings"}},
	{Name: "cursor", Role: Declaration, Doc: "A prompt-engineering dial: an enum (values:) or a numeric band map (bands:) over [0, 1], each entry carrying a prompt fragment (C083–C086).",
		Properties: []Property{
			pDescription,
			block("values", "cursor.values", "Enum form: one `name: \"fragment\"` per line, order preserved"),
			block("bands", "cursor.bands", "Numeric form: one `\"lo..hi\": \"fragment\"` per line"),
		}},
	{Name: "cursor.values", Role: BlockRole, Opener: "values", Hosts: []string{"cursor"}, Doc: "The enum values of a cursor.",
		Entries: &Entries{Shape: `name: "prompt fragment"`, Doc: "Order is the position a numeric invocation snaps to"}},
	{Name: "cursor.bands", Role: BlockRole, Opener: "bands", Hosts: []string{"cursor"}, Doc: "The numeric bands of a cursor.",
		Entries: &Entries{Shape: `"lo..hi": "prompt fragment"`, Doc: "The range is parsed by the compiler (C085 when malformed)"}},
	{Name: "supervisor", Role: Declaration, Doc: "A concurrent LLM watcher of agent nodes that enqueues steering messages the watched node reads at its next turn (docs/supervisors.md); run metadata, not a graph node.",
		Properties: []Property{
			prop("watches", IdentList, "Agent nodes the supervisor is armed for"),
			pModel,
			pSystem,
			prop("cooldown", String, "Minimum delay between two evaluations, e.g. \"2m\""),
			prop("max_evals", Int, "Upper bound on evaluations per run"),
			prop("monitors", StringList, "Event patterns armed from the first event (the CLI --monitor grammar)"),
		}},
	{Name: "mcp_server", Role: Declaration, Doc: "An MCP server the workflow may activate: stdio (command/args) or http/sse (url), optionally OAuth2.",
		Properties: []Property{
			enum("transport", "How the server is reached", "stdio", "http", "sse"),
			prop("command", String, "stdio: the executable"),
			prop("args", StringList, "stdio: its arguments"),
			prop("url", String, "http / sse: the endpoint"),
			block("auth", "auth", "OAuth2 authorization-code/PKCE settings"),
		}},
	{Name: "auth", Role: BlockRole, Opener: "auth", Hosts: []string{"mcp_server"}, Doc: "OAuth2 settings of an MCP server (only authorization-code/PKCE is wired).",
		Properties: []Property{
			prop("type", String, "\"oauth2\""),
			prop("auth_url", String, "Authorization endpoint"),
			prop("token_url", String, "Token endpoint"),
			prop("revoke_url", String, "Revocation endpoint (optional)"),
			prop("client_id", String, "OAuth client id"),
			prop("scopes", StringList, "Scopes requested"),
		}},
	{Name: "group", Role: Declaration, Header: "group <name>(<param>, …):", Doc: "A reusable node cluster with parameters, whose body holds agent/judge/router/human/tool/compute declarations and edges; instantiated by `use`, expanded at compile time (C141 warns on a use of an empty group). Prompts read `{{params.name}}`.",
		Entries: &Entries{Shape: "node declarations and edges (src -> dst)", Doc: "Nodes are addressed as <prefix>.<node> once instantiated"}},
	{Name: "use", Role: Declaration, Header: "use <group> as <prefix> [with { <param>: \"value\", … }]", Doc: "One instance of a group; the with map binds its parameters. No body.",
		Entries: &Entries{Shape: `use g as p with { param: "value" }`, Doc: "A single line, no indented body"}},

	// ---- top-level blocks ----
	{Name: "vars", Role: BlockRole, Opener: "vars", Hosts: []string{"file", "workflow"}, Doc: "Typed run parameters, overridable with --var and presets.",
		Entries: &Entries{Shape: `name: type [enum: "a", "b"] [= default]`, Doc: "type is string, bool, int, float, json or string[]; the enum constraint applies to strings; a json/string[] default is a quoted JSON text"}},
	{Name: "presets", Role: BlockRole, Opener: "presets", Hosts: []string{"file"}, Doc: "Named bundles of var values selected with --recipe / --preset.",
		Entries: &Entries{Shape: "name: (indented) var: literal", Doc: "Each entry is a preset name with one `var: literal` line per value"}},
	{Name: "attachments", Role: BlockRole, Opener: "attachments", Hosts: []string{"file", "workflow"}, Doc: "Operator-supplied files and images the run receives.",
		Entries: &Entries{Shape: "name: file | image", Body: "attachment", Doc: "An entry may open an indented sub-block"}},
	{Name: "attachment", Role: Entry, Opener: "attachments", Hosts: []string{"attachments"}, Doc: "The optional sub-block of one attachment.",
		Properties: []Property{
			pDescription,
			prop("accept_mime", StringList, "MIME types accepted"),
			prop("required", Bool, "The run cannot start without it"),
		}},
	{Name: "secrets", Role: BlockRole, Opener: "secrets", Hosts: []string{"file"}, Doc: "Secrets the run resolves by name from the team's or the local store (docs/secrets.md).",
		Entries: &Entries{Shape: `name: "value"`, Body: "secret", Doc: "The value on the header line is optional: a bare `name:` (with or without a sub-block) resolves the stored secret by name"}},
	{Name: "secret", Role: Entry, Opener: "secrets", Hosts: []string{"secrets"}, Doc: "The optional sub-block of one secret.",
		Properties: []Property{
			prop("value", String, "Inline value — prefer the stored secret, resolved by name"),
			checked("as", "How the secret is materialised: value (env/template) or file", "value", "file"),
			prop("mount_path", String, "as: file — the path inside the sandbox"),
			prop("env", StringOrIdent, "Environment variable that receives the value"),
			prop("optional", Bool, "A missing secret does not fail the launch"),
			prop("hosts", StringList, "Hosts the secret may be sent to"),
			pDescription,
		}},

	// ---- nodes ----
	{Name: "agent", Role: Node, Doc: "An LLM node with tools, structured I/O and any backend.", Properties: llmProperties},
	{Name: "judge", Role: Node, Doc: "An LLM node producing verdicts; same surface as an agent, typically without tools.", Properties: llmProperties},
	{Name: "router", Role: Node, Doc: "A routing node: fan_out_all, fan_out_each, condition, round_robin or llm (docs/routers.md). Never takes await.",
		Properties: []Property{
			pDescription,
			enum("mode", "Routing mode", "fan_out_all", "fan_out_each", "condition", "round_robin", "llm"),
			llmOnly(pModel), llmOnly(pBackend), pProvider, llmOnly(pSystem), llmOnly(pUser),
			prop("multi", Bool, "llm mode only (C023 otherwise): the model may select several outgoing edges"),
			llmOnly(pReasoning),
			prop("over", String, "fan_out_each: expression naming the collection to iterate"),
			prop("as", Ident, "fan_out_each: alias each item is bound to ({{each.<as>}})"),
			prop("key", Ident, "fan_out_each: item field that names each branch"),
			prop("depends_on", Ident, "fan_out_each: item field naming the branch this one waits for (requires key)"),
			pNeeds,
		}},
	{Name: "human", Role: Node, Doc: "A pause point the operator answers (interaction human, the default), an LLM answers (llm / llm_or_human), or a review gate (review).",
		Properties: []Property{
			pDescription,
			pInput, pOutput, pPublish, pArtifactLabels,
			prop("instructions", Ident, "Prompt shown to the operator as the question"),
			pSystem, pModel,
			pInteraction, pInteractionP, pInteractionM,
			prop("min_answers", Int, "Answers required before the node resumes"),
			pAwait,
			prop("review_url", String, "review: the PR/MR the gate reviews (a {{…}} reference is accepted)"),
			Property{Name: "posture", Form: StringOrIdent, Values: []string{"human_required", "agent_verdict_ok"},
				Doc: "review: who may merge — human_required (default) or agent_verdict_ok; not validated at compile, another word reads as the default"},
			Property{Name: "merge_strategy", Form: StringOrIdent, Values: []string{"squash", "merge"},
				Doc: "review: squash (default) or merge; not validated at compile"},
			prop("merge_into", StringOrIdent, "review: current (default), none or a branch name"),
			prop("max_turns", Int, "review: conversation turns before the gate escalates"),
		}},
	{Name: "tool", Role: Node, Doc: "Direct shell execution, no LLM: `command:` runs through bash -c, `script:` through the interpreter `language:` names; with `output:` the command prints schema-shaped JSON on stdout. A Verified Action adds goal + postcondition + policy + recovery (ADR-044).",
		Properties: []Property{
			pDescription,
			prop("command", String, "Shell command, run through bash -c (exclusive with script)"),
			prop("script", String, "Inline script run by the interpreter language: names"),
			checked("language", "Interpreter for script:", "js", "node", "py", "python", "python3", "sh", "bash"),
			pInput, pOutput, pPublish, pArtifactLabels,
			pAwait,
			pSandbox,
			pCompress,
			prop("permission", Ident, "Parsed for symmetry but NOT enforced on a tool node (C112 warns): the command runs directly, the gate is an agent's"),
			pNeeds,
			prop("parallel_safe", Bool, "Declares the node safe to run beside a mutating branch"),
			prop("goal", String, "Verified action: what the command is for, in one line"),
			prop("postcondition", String, "Verified action: command whose exit code is the truth oracle at every rung"),
			checked("policy", "Verified action: required (default), recover or best_effort (C103–C106)", "required", "recover", "best_effort"),
			block("recovery", "recovery", "Verified action: the self-heal ladder's bounds"),
		}},
	{Name: "recovery", Role: BlockRole, Opener: "recovery", Hosts: []string{"tool"}, Doc: "Bounds of a Verified Action's recovery ladder (idempotent-skip → recipe → self-repair → agent → policy).",
		Properties: []Property{
			prop("max_repair_attempts", Int, "Self-repair rungs before the agent rung"),
			prop("max_agent_attempts", Int, "Agent rungs before the policy decides"),
			pModel,
			prop("agent_tools", ToolList, "Tools the recovery agent may call"),
		}},
	{Name: "compute", Role: Node, Doc: "A deterministic expression node: each expr entry is evaluated by the bounded expression language into an output field.",
		Properties: []Property{
			pDescription,
			pInput, pOutput, pPublish, pArtifactLabels,
			pAwait,
			block("expr", "expr", "One `field: \"expression\"` per output field"),
		}},
	{Name: "expr", Role: BlockRole, Opener: "expr", Hosts: []string{"compute"}, Doc: "The expressions of a compute node.",
		Entries: &Entries{Shape: `field: "expression"`, Doc: "An expression over vars, input, outputs, artifacts, loop and run — not a {{template}}"}},
	{Name: "subbot", Role: Node, Doc: "Runs another .bot as a nested child run; its outputs read back as {{outputs.<subbot>.<field>}} (C119).",
		Properties: []Property{
			pDescription,
			prop("source", String, "Path of the child .bot, relative to this file"),
			prop("with", WithMap, "Child vars; {{…}} references are allowed in the values"),
			pOutput,
			pNeeds,
			prop("isolated", Bool, "Asserts the child runs in its own workspace"),
		}},
	{Name: "emit", Role: Node, Doc: "Publishes a named run-scoped event with an immutable payload (ADR-051).",
		Properties: []Property{
			pDescription,
			prop("event", String, "Event name"),
			prop("with", WithMap, "Payload fields"),
		}},
	{Name: "wait", Role: Node, Doc: "Blocks its branch until the named event fires; the timeout is mandatory (ADR-051, C196–C198).",
		Properties: []Property{
			pDescription,
			prop("event", String, "Event name awaited"),
			prop("timeout", String, "Duration after which the wait fails, e.g. \"30s\" (mandatory)"),
			pOutput,
		}},
	{Name: "await_answers", Role: Node, Doc: "Parks its branch until every pending ask_user_async question of `from:` (or the whole run) is answered; output {answers: […]} (ADR-081, C241/C242).",
		Properties: []Property{
			pDescription,
			prop("from", StringOrIdent, "Node whose async questions are awaited; omit for the whole run"),
			prop("timeout", String, "Duration after which the node fails, e.g. \"30m\" (mandatory)"),
		}},
	{Name: "fail", Role: Node, Doc: "A named terminal failure with a typed code and a message (C247 checks the UPPER_SNAKE code, C248 refuses a reserved one).",
		Properties: []Property{
			pDescription,
			prop("code", StringOrIdent, "Error code, UPPER_SNAKE (bare or quoted)"),
			prop("message", String, "Message; {{…}} references are rendered"),
			prop("resumable", Bool, "Leaves the run failed_resumable instead of failed"),
		}},

	// ---- workflow and its blocks ----
	{Name: "workflow", Role: Declaration, Doc: "The graph: entry, edges (`src -> dst [when …|else] [as loop(N)] [with {…}]`), and the run-wide settings; a bare header declares an empty workflow (C008).",
		Properties: []Property{
			prop("entry", Ident, "Node the run starts at; a dotted name addresses a group instance's node"),
			block("vars", "vars", "Workflow-scoped vars (merged with the file's)"),
			block("attachments", "attachments", "Workflow-scoped attachments"),
			block("budget", "budget", "Run caps, each overridable by the matching run flag"),
			block("resources", "resources", "Named semaphores and pools nodes lease with needs:"),
			block("mcp", "mcp", "MCP servers active for the run"),
			block("compaction", "compaction", "Default compaction thresholds"),
			pSandbox,
			checked("worktree", "auto runs the workflow in a fresh git worktree, finalised into a branch; none runs in place", "auto", "none"),
			prop("default_backend", String, "Backend for nodes that name none"),
			pCompress, pAutoMemory,
			checked("loop_budget_guard", "Decline a loop's back-edge the budget cannot fund: on (default) or off (C133)", "on", "off"),
			checked("repo_devbox", "Load the target repo's devbox.json toolchain: on (default) or off (C134)", "on", "off"),
			checked("workspace_checkpoint", "Mid-run preservation of a copy-based sandbox's workspace as a checkpoint branch pushed to the run's own remote: on (default) or off (C139)", "on", "off"),
			pPermission,
			prop("allow", StringList, "Permission rules always allowed, Tool(pattern) syntax"),
			prop("ask", StringList, "Permission rules that pause for approval"),
			prop("deny", StringList, "Permission rules always blocked"),
			prop("tool_policy", ToolList, "Run-wide tool-policy entries"),
			prop("capabilities", ToolList, "Run-wide board capabilities"),
			prop("skills", SkillList, "Run-wide skill-library skills"),
			enum("interaction", "Default interaction mode for the run's nodes that set none", "none", "human", "llm", "llm_or_human", "review", "async"),
		}},
	{Name: "budget", Role: BlockRole, Opener: "budget", Hosts: []string{"workflow"}, Doc: "Run caps; a zero or absent cap is the engine default, and each is overridable per run without editing the .bot.",
		Properties: []Property{
			prop("max_parallel_branches", Int, "Concurrent branches (0 = engine default)"),
			prop("max_duration", String, "Wall-clock cap, e.g. \"4h\""),
			prop("max_cost_usd", Number, "Spend cap in USD"),
			prop("max_tokens", Int, "Total token cap"),
			prop("warn_tokens", Int, "Advisory: crossing it emits budget_warning"),
			prop("max_iterations", Int, "Total node executions; also the fuel of an unbounded loop (C097)"),
		}},
	{Name: "resources", Role: BlockRole, Opener: "resources", Hosts: []string{"workflow"}, Doc: "Named resources nodes lease with needs:.",
		Entries: &Entries{Shape: `name: <int> | ["member-a", "member-b"]`, Doc: "A count is a semaphore; a quoted list is a pool whose members are leased one at a time"}},
	{Name: "compaction", Role: BlockRole, Opener: "compaction", Hosts: []string{"workflow", "agent", "judge"}, Doc: "When and how a session's context is compacted.",
		Properties: []Property{
			prop("threshold", Number, "Context fraction (0–1) that triggers compaction"),
			prop("preserve_recent", Int, "Recent messages kept verbatim"),
		}},
	{Name: "memory", Role: BlockRole, Opener: "memory", Hosts: []string{"agent", "judge"}, Doc: "iterion's shared-memory tools for a node (docs/memory-and-knowledge.md); distinct from auto_memory.",
		Properties: []Property{
			prop("enabled", Bool, "Open the memory tools to the node"),
			prop("scope", String, "Memory scope the tools read and write"),
			prop("autoload", StringList, "Documents injected at node start"),
			prop("read", Bool, "Allow memory_read"),
			prop("write", Bool, "Allow memory_write"),
			prop("pre_compact_inject", Bool, "Re-inject memory before a compaction"),
			prop("project_root", Bool, "Key the space on the repository root rather than the working directory (legacy; exclusive with visibility)"),
			Property{Name: "visibility", Form: String, Values: []string{"bot", "project", "cross_project", "user", "org", "global"},
				Doc: "Who sees the space (C170); quoted"},
		}},
	{Name: "mcp", Role: BlockRole, Opener: "mcp", Hosts: []string{"workflow", "agent", "judge"}, Doc: "Which MCP servers are active; an empty block wires nothing (C135 stays an error).",
		Properties: []Property{
			prop("autoload_project", Bool, "Workflow scope: load the repository's .mcp.json servers (default true)"),
			prop("inherit", Bool, "Node scope: inherit the workflow's active servers (default true)"),
			prop("servers", IdentList, "mcp_server declarations activated"),
			prop("disable", IdentList, "Servers removed from the ambient set"),
		}},
	{Name: "sandbox", Role: BlockRole, Opener: "sandbox", Hosts: []string{"workflow", "agent", "judge", "tool"}, Doc: "Per-run container isolation (docs/sandbox.md): the short form names a mode, the block form is inline and needs image: or build: (C044).",
		Properties: []Property{
			checked("mode", "none, auto (devcontainer.json or the published slim image) or inline (C044 on another word)", "none", "auto", "inline"),
			prop("image", String, "Container image (exclusive with build)"),
			block("build", "sandbox.build", "Dockerfile build, local docker only (V2-6)"),
			prop("user", String, "Container user"),
			prop("workspace_folder", String, "Mount point of the workspace inside the container"),
			checked("host_state", "Mount ~/.iterion and ~/.claude into the container: auto or none", "auto", "none"),
			prop("post_create", String, "Command run once after the container starts"),
			prop("env", Map, "Environment variables"),
			prop("mounts", MixedList, "Extra bind mounts"),
			block("network", "sandbox.network", "Egress policy (open by default)"),
		}},
	{Name: "sandbox.build", Role: BlockRole, Opener: "build", Hosts: []string{"sandbox"}, Doc: "A Dockerfile build of the sandbox image (docker driver only).",
		Properties: []Property{
			prop("dockerfile", String, "Dockerfile path"),
			prop("context", String, "Build context"),
			prop("args", Map, "Build arguments"),
		}},
	{Name: "sandbox.network", Role: BlockRole, Opener: "network", Hosts: []string{"sandbox"}, Doc: "Network egress of the sandbox, enforced by a CONNECT proxy on the host.",
		Properties: []Property{
			checked("mode", "open (no proxy), allowlist or denylist (C044 on another word)", "open", "allowlist", "denylist"),
			prop("preset", StringOrIdent, "Rule preset, e.g. \"iterion-default\""),
			checked("inherit", "How a node's rules compose with the workflow's: omit to merge (the default), or replace / append (C044 on another word)", "replace", "append"),
			prop("rules", MixedList, "Hosts and globs; a leading ! negates"),
		}},
	{Name: "cursors", Role: BlockRole, Opener: "cursors", Hosts: []string{"agent", "judge"}, Doc: "Cursor activation on a node: the reserved enabled: key plus one setting per declared cursor.",
		Properties: []Property{
			prop("enabled", Bool, "Gate for the whole block (an explicit block opts in)"),
		},
		Entries: &Entries{Shape: `cursor_name: ident | number | "string"`, Doc: "A value name, a position in [0, 1], or a quoted string for ${VAR} substitution"}},
	{Name: "fallback", Role: Entry, Opener: "fallbacks", Hosts: []string{"agent", "judge"}, Doc: "One named route of a fallbacks: chain (ADR-087/ADR-091), tried in declaration order.",
		Properties: []Property{
			pBackend, pModel, pProvider,
			Property{Name: "on", Form: IdentList, Values: []string{"usage_window", "auth", "unavailable", "transient_exhausted", "any"},
				Doc: "Failure classes that take this route (default usage_window, unavailable; never any or auth by default)"},
			prop("metered", Bool, "The route spends a metered API key (credential hint)"),
			checked("action", "skip: complete the node with a zero-value output stamped _skipped instead of failing", "skip"),
			prop("when", String, "Expression over vars that gates the route"),
		}},
}
