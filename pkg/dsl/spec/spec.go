// Package spec is the declarative registry of the `.bot` DSL's property
// surface: for every declaration kind, node kind and anonymous block, the
// properties it accepts, each with the shape of its value and one line of
// doc — and, where a body is not a property table, the shape of its
// author-named entries and of its declaration header, part by part. It is
// a LEAF (no iterion import): the parser reads it to explain an unknown
// property (the closest accepted names, the block a name belongs to, the
// kind's full list), the renderers turn it into the grammar reference, the
// skill's property section and the author JSON Schema, and a conformance
// test holds it to the parser in BOTH directions — every property listed
// here is accepted by the parser, every property the parser accepts is
// listed here, every Form's witnesses read as the parser reads them — so a
// property added to one side without the other fails CI instead of
// drifting for months (the three canonical examples of the DSL quickref did
// not parse until #1010's lot 0 rewrote them).
//
// The registry DESCRIBES the parser; it does not drive it (a generic parser
// loop over it is lot 1b of #1010, an option). What parses is decided by
// the parser's switch arms; this is the copy that cannot drift because the
// test reads both.
package spec

import "sort"

// Form is the shape of a property's value as the parser reads it. Each Form
// is one language of the parser, held to it by the conformance sweep on
// accepted and refused witnesses (conformance_form_test.go): a property
// listed under a Form reads exactly what that Form says, no more, no less.
type Form string

const (
	String         Form = "string"        // a quoted string: "…", `…` or a `|` block scalar — or one plain bare word, which is the string it spells
	Ident          Form = "ident"         // a bare identifier (a name or a reference); a quoted string is refused
	DottedIdent    Form = "dotted ident"  // a bare identifier, dotted when it addresses a group instance's node, a node's field or a plugin's kind (`r1.look`, `node.field`); a quoted string is refused
	StringOrIdent  Form = "string|ident"  // a quoted string or a bare identifier, dotted (`github.com`) allowed
	Int            Form = "int"           // an unquoted non-negative integer
	Number         Form = "number"        // an unquoted non-negative integer or float
	Bool           Form = "bool"          // true or false, bare
	JSON           Form = "json value"    // one JSON value on one line ("text", 12, true, null, [...], {key: value}): the subset the text writes — no signed number, no exponent, keys unique; null explicit, a bare word is not a value; a trailing comma in a container is tolerated
	Enum           Form = "enum"          // one of Property.Values, bare or quoted
	EnumOrEnv      Form = "enum|env"      // one of Property.Values, bare or quoted — or any quoted string, kept as written for a ${VAR:-default} substitution at run time
	PromptRef      Form = "prompt ref"    // a declared prompt's name, bare — or the prompt's own text as a string (quoted, raw or a `|` block scalar): an inline prompt
	StringOrNumber Form = "string|number" // a quoted string, a bare word, or a number, its unit attached (`30s`, `3`)
	// Every list form is written inline (`[a, b]`) or as one `- item` per
	// line indented under the property; both read as the same list.
	IdentList    Form = "ident list"         // [a, b] — an element that is not a bare name is refused
	StringList   Form = "string list"        // ["a", "b"] — a bare word is the string it spells
	ToolList     Form = "tool list"          // [bash, mcp.server.*, "quoted-literal"] — the same grammar as a skill list (a dotted bare ref, or a quoted literal); the two names tell the reader what the names ARE
	SkillList    Form = "skill list"         // ["kebab-name", dotted.ident] — the same grammar as a tool list; the day one diverges (a wildcard refused here), the divergence needs its own witness
	MixedList    Form = "string|ident list"  // ["!**.evil.site", github.com]
	IdentOrList  Form = "ident | ident list" // godot or [godot, blender]
	Map          Form = "map"                // { KEY: "v" } inline, or an indented `KEY: v` block; a value is a string or a bare word
	WithMap      Form = "with { … }"         // with { key: "value", … }; a number or a bool is the string it spells
	Block        Form = "block"              // an indented block whose body is the kind named in Property.Body
	BlockOrIdent Form = "ident | block"      // a bare word on the header line (the compiler narrows it to Property.Values), or an indented block
	// Forms of entry parts (Field), not of properties.
	QuotedString    Form = "quoted string"           // a quoted string, a raw string or a `|` block scalar — never a bare word (an expression)
	Literal         Form = "literal"                 // a scalar literal: a quoted string, an integer, a float or a bool (a var's default, a preset's value)
	TypeRef         Form = "type"                    // a builtin type or a declared schema's name, `[]` suffixes allowed (a contract port)
	Setting         Form = "ident | number | string" // a cursor setting: a value name, a position in [0, 1], or a quoted string for ${VAR} substitution
	IntOrStringList Form = "int | string list"       // a resource: a count, or a pool of quoted member ids
)

// Property is one `name: value` line a kind accepts.
type Property struct {
	Name string
	Form Form
	// Values are the accepted values of an Enum or an EnumOrEnv. For an
	// Ident, a StringOrIdent, a String or a BlockOrIdent they are the values
	// a COMPILE check accepts (the parser takes any word; the compiler names
	// the code), listed for the reader.
	Values []string
	// Body names the kind whose properties fill a Block / BlockOrIdent.
	Body string
	Doc  string
	// Since is the first syntax profile that accepts the property (0: every
	// profile); Until the last (0: every later profile). Outside the window
	// the parser refuses the property by name (E043 past Until), the author
	// schema of that profile leaves it out, and the rendered documents say
	// so. No property carries a Since above 1 today: the first that does
	// makes TestProfileMarksMatchTheParser demand the parser's refusal.
	Since int
	Until int
	// Deprecated marks a property still accepted in every profile of its
	// window but no longer the way to write the thing: the rendered
	// documents and the author schema annotate it, nothing refuses it.
	Deprecated bool
}

// Field is one named part of a declaration header or of an entry line:
// what the `.bot` spells at a fixed position after the name, and what a
// structured twin of the document names by a key. A var's `type`, its
// `[enum: …]` constraint and its `= default`; a group's parameter list; a
// use's `as` prefix and `with` map.
type Field struct {
	Name string
	Form Form
	// Values are the accepted words of an Enum part.
	Values []string
	// Required marks a part written on every entry (a var's type); an
	// optional part may be absent.
	Required bool
	// Spelling is how the `.bot` line writes the part, for the rendered
	// shape: `[enum: "a", "b"]`, `= <default>`, `(<param>, …)`. Empty
	// spells the part as `<name>` (a string as `"<name>"`).
	Spelling string
	Doc      string
}

// Header is the declaration line of a kind that departs from `<kind>
// <name>:`: the parts written after the name, in order (a group's
// parameter list; a use's `as` prefix and `with` map — for a `use` the
// name after the keyword is the GROUP's). Syntax is the line as the
// reference shows it.
type Header struct {
	Syntax string
	Fields []Field
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
// when its body is not (only) a fixed property table: what names an
// entry, the parts written on its line, and what its indented sub-block
// carries.
type Entries struct {
	// Key is the form of an entry's name: Ident (the default, "" reads as
	// Ident) or String (a quoted key, as a cursor band's "lo..hi").
	Key Form
	// KeyName is the word the rendered shape uses for the name ("name" when
	// empty): `<field>: …`, `"<range>": …`.
	KeyName string
	// Fields are the parts of the entry line after the colon, in order; an
	// entry with one field is `name: value`. Empty when the line stops at
	// the name (a preset, a criterion) and the sub-block carries it all.
	Fields []Field
	// Body names the kind whose PROPERTIES an entry's indented sub-block
	// carries (attachment, secret, fallback, contract.port), if it has one.
	Body string
	// Entries, when an entry's indented sub-block is itself made of
	// author-named entries (a preset's `var: literal` lines).
	Entries *Entries
	// SequenceKey is set for entries whose order is meaning to the engine
	// (a fallback chain's try order): a structured twin writes them as a
	// sequence of objects carrying the entry's name under this key, rather
	// than as a mapping keyed by name.
	SequenceKey string
	Doc         string
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
	Header     *Header
	Doc        string
	Properties []Property
	Entries    *Entries
	// Holds lists the node kinds a declaration's body may contain (a group).
	Holds []string
	// Edges reports that the body carries edge lines (`src -> dst …`).
	Edges bool
	// Text reports that the body is free text (a prompt).
	Text bool
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

// field is a required part of an entry line or a header.
func field(name string, form Form, spelling, doc string) Field {
	return Field{Name: name, Form: form, Required: true, Spelling: spelling, Doc: doc}
}

// optional is a part an entry line or a header may leave out.
func optional(name string, form Form, spelling, doc string) Field {
	return Field{Name: name, Form: form, Spelling: spelling, Doc: doc}
}

// varTypes are the types a var declares; schemaTypes add `file`, the
// operator-supplied binary a human node's output schema may collect.
var (
	varTypes    = []string{"string", "bool", "int", "float", "json", "string[]"}
	schemaTypes = append(append([]string{}, varTypes...), "file")
)

// Properties shared by every node kind that produces or consumes data.
var (
	pInput           = prop("input", Ident, "Schema the node's input is validated against")
	pOutput          = prop("output", Ident, "Schema the node's structured output must match")
	pPublish         = prop("publish", Ident, "Artifact name the output is published under (read back as {{artifacts.<name>}})")
	pArtifactLabels  = prop("artifact_labels", ToolList, "Labels stamped on the published artifact; a quoted element is the literal label")
	pAwait           = enum("await", "Convergence rule when several incoming branches reach the node", "wait_all", "best_effort")
	pDescription     = prop("description", String, "Free-text description shown by the studio and the reports")
	pNeeds           = prop("needs", IdentOrList, "Resource(s) leased from the workflow's resources: block for the node's duration")
	pModel           = prop("model", String, "Model id the backend serves, e.g. \"anthropic/claude-opus-5\"; empty takes the backend's default; a {{vars.x}} reference resolves (vars only), then ${VAR:-default}")
	pBackend         = prop("backend", String, "Execution backend: claw, claude_code, codex, pi, kimi, grok or opencode; a {{vars.x}} reference resolves (vars only), then ${VAR:-default}")
	pProvider        = prop("provider", String, "Provider hint for credential resolution, e.g. \"anthropic\"; a {{vars.x}} reference resolves (vars only), then ${VAR:-default}")
	pSupervisorModel = prop("model", String, "Model id the supervisor evaluates with, e.g. \"anthropic/claude-opus-5\"; empty follows the watched nodes' provider family; an environment form ${VAR:-default} expands, a {{…}} template is not rendered and warned (C148): a supervisor is spawned without the run's vars")
	pSystem          = prop("system", PromptRef, "The system prompt: a declared prompt's name, or the text itself as a string (an inline prompt, named after its body)")
	pUser            = prop("user", PromptRef, "The user message: a declared prompt's name, or the text itself as a string (an inline prompt, named after its body)")
	pTimeout         = prop("timeout", String, "Duration the node may run, e.g. \"20m\"")
	pCompress        = checked("compress", "Command-output compression: on, ultra or off (C102)", "on", "ultra", "off")
	pPermission      = checked("permission", "Tool-permission gate: off, ask or deny (C110–C112)", "off", "ask", "deny")
	pAutoMemory      = checked("auto_memory", "The backend's own auto-memory: on or off (C131/C132)", "on", "off")
	pInteraction     = enum("interaction", "How the node asks the operator (ADR-081); human_or_host lets the host application answer in the operator's place, whichever comes first (docs/assistant-dock.md, C212)", "none", "human", "llm", "llm_or_human", "review", "async", "human_or_host")
	pInteractionP    = prop("interaction_prompt", Ident, "Prompt the llm interaction mode answers with in the operator's place")
	pInteractionM    = prop("interaction_model", String, "Model the llm interaction mode uses; a {{vars.x}} reference resolves (vars only), then ${VAR:-default}")
	pReasoning       = Property{Name: "reasoning_effort", Form: EnumOrEnv, Values: []string{"low", "medium", "high", "xhigh", "max", "ultracode"},
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
	prop("session_slot", Ident, "Named durable session slot; requires session: persist"),
	prop("tools", ToolList, "Tools the node may call; `tools: []` declares NO tools, no line at all leaves it undeclared. Restricts claw (C135 on a name it lacks); on claude_code it becomes --disallowedTools over that CLI's native roster, on codex a sandbox mode; pi/kimi/grok never receive it (C270)"),
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
	prop("allow", StringList, "Permission rules always allowed on this node, Tool(pattern) syntax; a non-empty list REPLACES the workflow's allow: (C154 refuses an unreadable rule, C111 warns when nothing gated reads the list)"),
	prop("ask", StringList, "Permission rules that pause for approval on this node; a non-empty list REPLACES the workflow's ask: (C154/C111; C136 and C176 screen the node's routes against it)"),
	prop("deny", StringList, "Permission rules always blocked on this node; a non-empty list REPLACES the workflow's deny: (C154/C111)"),
	pNeeds,
	block("fallbacks", "fallbacks", "Ordered, NAMED alternative routes taken when the primary fails (ADR-087); a chain with no route is refused"),
	block("mcp", "mcp", "MCP servers active for the node"),
	block("compaction", "compaction", "Context-compaction thresholds of the node's session"),
	block("memory", "memory", "iterion's shared-memory tools and scopes for the node"),
	pSandbox,
	block("cursors", "cursors", "Prompt-engineering dials activated on the node (docs/cursors.md)"),
}

// Kinds is the registry, in the order the reference renders them.
var Kinds = append([]Kind{
	// ---- top-level declarations ----
	{Name: "prompt", Role: Declaration, Text: true, Doc: "A named text block, referenced by `system:` / `user:` / `instructions:`; its body is free text with {{…}} references and {{include \"file\"}} directives, the first line's indentation stripped from every line. Blank lines in the body are dropped by the lexer in profile 1 and kept as paragraph breaks in profile 2; a bare header declares an empty prompt."},
	{Name: "schema", Role: Declaration, Doc: "A structured-output shape; a bare header declares an empty schema.",
		Entries: &Entries{Key: Ident, KeyName: "field", Doc: "One field per line; `file` is valid only on the output schema of a human node whose interaction collects operator bytes (C129); the enum constraint applies to strings",
			Fields: []Field{
				Field{Name: "type", Form: Enum, Values: schemaTypes, Required: true, Doc: "The field's type"},
				optional("enum", StringList, `[enum: "a", "b"]`, "The values a string field may take, quoted"),
			}}},
	{Name: "cursor", Role: Declaration, Doc: "A prompt-engineering dial: an enum (values:) or a numeric band map (bands:) over [0, 1], each entry carrying a prompt fragment (C083–C086).",
		Properties: []Property{
			pDescription,
			block("values", "cursor.values", "Enum form: one `name: \"fragment\"` per line, order preserved"),
			block("bands", "cursor.bands", "Numeric form: one `\"lo..hi\": \"fragment\"` per line"),
		}},
	{Name: "cursor.values", Role: BlockRole, Opener: "values", Hosts: []string{"cursor"}, Doc: "The enum values of a cursor.",
		Entries: &Entries{Key: Ident, Doc: "Order is the position a numeric invocation snaps to",
			Fields: []Field{field("prompt", String, `"prompt fragment"`, "The fragment appended to the system prompt when the value is selected")}}},
	{Name: "cursor.bands", Role: BlockRole, Opener: "bands", Hosts: []string{"cursor"}, Doc: "The numeric bands of a cursor.",
		Entries: &Entries{Key: String, KeyName: "lo..hi", Doc: "The range is parsed by the compiler (C085 when malformed)",
			Fields: []Field{field("prompt", String, `"prompt fragment"`, "The fragment appended to the system prompt when the position falls in the band")}}},
	{Name: "supervisor", Role: Declaration, Doc: "A concurrent LLM watcher of agent nodes that enqueues steering messages the watched node reads at its next turn (docs/supervisors.md); run metadata, not a graph node.",
		Properties: []Property{
			prop("watches", IdentList, "Agent nodes the supervisor is armed for"),
			pSupervisorModel,
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
	{Name: "group", Role: Declaration, Doc: "A reusable node cluster with parameters, whose body holds agent/judge/router/human/tool/compute declarations and edges; instantiated by `use`, expanded at compile time (C141 warns on a use of an empty group). Prompts read `{{params.name}}`; nodes are addressed as <prefix>.<node> once instantiated.",
		Header: &Header{Syntax: "group <name>(<param>, …):", Fields: []Field{
			optional("params", IdentList, "(<param>, …)", "The parameters a use binds with its with map; prompts read {{params.<name>}}"),
		}},
		Holds: []string{"agent", "judge", "router", "human", "tool", "compute"}, Edges: true},
	{Name: "use", Role: Declaration, Doc: "One instance of a group, on a single line with no body: the name after `use` is the group's, `as` gives the instance its prefix, the with map binds the group's parameters.",
		Header: &Header{Syntax: "use <group> as <prefix> [with { <param>: \"value\", … }]", Fields: []Field{
			field("as", Ident, "as <prefix>", "The prefix the instance's nodes are addressed by (<prefix>.<node>)"),
			optional("with", WithMap, `with { <param>: "value", … }`, "The parameter bindings; a number or a bool is the string it spells"),
		}}},

	// ---- top-level blocks ----
	{Name: "vars", Role: BlockRole, Opener: "vars", Hosts: []string{"file", "workflow"}, Doc: "Typed run parameters, overridable with --var and presets.",
		Entries: &Entries{Key: Ident, Doc: "One var per line; the enum and matching constraints apply to strings; a json/string[] default is a quoted JSON text",
			Fields: []Field{
				Field{Name: "type", Form: Enum, Values: varTypes, Required: true, Doc: "The var's type"},
				optional("enum", StringList, `[enum: "a", "b"]`, "The values a string var may take, quoted"),
				optional("matching", QuotedString, `[matching: "<re>"]`, "An RE2 pattern a string var's value must match, checked at launch against --var and payload values; beside enum in either order, at most one of each"),
				optional("default", Literal, "= <default>", "The value the run starts with when no --var or preset sets one; a json or string[] default is written as a quoted JSON text"),
			}}},
	{Name: "presets", Role: BlockRole, Opener: "presets", Hosts: []string{"file"}, Doc: "Named bundles of var values selected with --recipe / --preset.",
		Entries: &Entries{Key: Ident, Doc: "Each entry is a preset name whose indented lines set one var each",
			Entries: &Entries{Key: Ident, KeyName: "var", Doc: "A var of the file and the literal it takes under this preset",
				Fields: []Field{field("value", Literal, "<literal>", "The value the var takes; its type is the var's")}}}},
	{Name: "attachments", Role: BlockRole, Opener: "attachments", Hosts: []string{"file", "workflow"}, Doc: "Operator-supplied files and images the run receives.",
		Entries: &Entries{Key: Ident, Body: "attachment", Doc: "An entry may open an indented sub-block",
			Fields: []Field{Field{Name: "type", Form: Enum, Values: []string{"file", "image"}, Required: true, Doc: "What the operator supplies"}}}},
	{Name: "attachment", Role: Entry, Opener: "attachments", Hosts: []string{"attachments"}, Doc: "The optional sub-block of one attachment.",
		Properties: []Property{
			pDescription,
			prop("accept_mime", StringList, "MIME types accepted"),
			prop("required", Bool, "The run cannot start without it"),
		}},
	{Name: "secrets", Role: BlockRole, Opener: "secrets", Hosts: []string{"file"}, Doc: "Secrets the run resolves by name from the team's or the local store (docs/secrets.md).",
		Entries: &Entries{Key: Ident, Body: "secret", Doc: "The value on the header line is optional: a bare `name:` (with or without a sub-block) resolves the stored secret by name",
			Fields: []Field{optional("value", String, `"value"`, "Inline value — the same field the sub-block's `value:` sets; prefer the stored secret, resolved by name")}}},
	{Name: "secret", Role: Entry, Opener: "secrets", Hosts: []string{"secrets"}, Doc: "The optional sub-block of one secret.",
		Properties: []Property{
			prop("value", String, "Inline value — prefer the stored secret, resolved by name"),
			Property{Name: "as", Form: StringOrIdent, Values: []string{"value", "file"}, Doc: "How the secret is materialised: value (env/template) or file"},
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
	{Name: "human", Role: Node, Doc: "A pause point the operator answers (interaction human, the default), an LLM answers (llm / llm_or_human), the host application may answer (human_or_host), or a review gate (review).",
		Properties: []Property{
			pDescription,
			pInput, pOutput, pPublish, pArtifactLabels,
			prop("instructions", PromptRef, "Prompt shown to the operator as the question: a declared prompt's name, or the text itself as a string"),
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
	{Name: "tool", Role: Node, Doc: "The deterministic node, no LLM. One of three recipes: `command:` runs through bash -c, `script:` through the interpreter `language:` names, `action:` calls a connector operation (ADR-098). With `output:` a command prints schema-shaped JSON on stdout. A Verified Action adds goal + postcondition + policy + recovery (ADR-044), which an `action:` refuses (C262/C263).",
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
			prop("action", StringOrIdent, "Connector operation to call, `connector.resource.verb`, bare or quoted — exclusive with command:/script: (ADR-098, C260)"),
			prop("connection", StringOrIdent, "The connection binding that authenticates the action, bare or quoted (an alias may carry a dash) (C261)"),
			block("params", "params", "The action's arguments, by the operation's own parameter keys"),
			prop("retry", StringOrNumber, "Action: how many EXTRA attempts, e.g. `3`; a duration is refused and empty means none (C265). Inert without `action:` (C266)"),
			prop("timeout", StringOrNumber, "Action: bound on one call, e.g. `30s` (C265). Inert without `action:` (C266)"),
		}},
	{Name: "params", Role: BlockRole, Opener: "params", Hosts: []string{"tool"}, Doc: "The arguments of a connector action, keyed by the operation's own parameter names.",
		Entries: &Entries{Key: StringOrIdent, KeyName: "key", Doc: "The key is the operation's own parameter name, quoted when it is not an identifier (`\"user-id\"`); a {{…}} template is rendered and then coerced to the type the operation declares, so an integer field receives a number",
			Fields: []Field{field("value", StringOrNumber, `"value"`, "The argument's value: a string, a bare word or a number, coerced by the operation's parameter type")}}},
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
		Entries: &Entries{Key: Ident, KeyName: "field", Doc: "An expression over vars, input, outputs, artifacts, loop and run — not a {{template}}",
			Fields: []Field{field("expression", QuotedString, `"expression"`, "The expression whose value fills the output field; always quoted, a bare word is not an expression")}}},
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
	{Name: "workflow", Role: Declaration, Edges: true, Doc: "The graph: entry, edges (`src -> dst [when …|else] [as loop(N)] [with {…}]`), and the run-wide settings; a bare header declares an empty workflow (C008).",
		Properties: []Property{
			prop("entry", DottedIdent, "Node the run starts at; a dotted name addresses a group instance's node"),
			prop("contract", Ident, "The bot's public contract (a top-level `contract` declaration), bound to the program (C300–C304)"),
			block("vars", "vars", "Workflow-scoped vars (merged with the file's)"),
			block("attachments", "attachments", "Workflow-scoped attachments"),
			block("budget", "budget", "Run caps, each overridable by the matching run flag"),
			block("resources", "resources", "Named semaphores and pools nodes lease with needs:"),
			block("mcp", "mcp", "MCP servers active for the run"),
			block("compaction", "compaction", "Default compaction thresholds"),
			pSandbox,
			checked("worktree", "auto runs the workflow in a fresh git worktree, finalised into a branch; none runs in place", "auto", "none"),
			prop("default_backend", String, "Backend for nodes that name none; a {{vars.x}} reference resolves (vars only), then ${VAR:-default}"),
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
			enum("interaction", "Default interaction mode for the run's nodes that set none", "none", "human", "llm", "llm_or_human", "review", "async", "human_or_host"),
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
		Entries: &Entries{Key: Ident, Doc: "A count is a semaphore; a quoted list is a pool whose members are leased one at a time",
			Fields: []Field{field("capacity", IntOrStringList, `<int> | ["member-a", "member-b"]`, "The slot count, or the pool's member ids (its capacity is their number)")}}},
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
			Property{Name: "project_root", Form: Bool, Until: 1,
				Doc: "Key the space on the repository root rather than the working directory (legacy; exclusive with visibility)"},
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
		Entries: &Entries{Key: Ident, KeyName: "cursor_name", Doc: "A value name, a position in [0, 1], or a quoted string for ${VAR} substitution",
			Fields: []Field{field("value", Setting, `ident | number | "string"`, "The setting: a declared value's name, a numeric position, or a quoted string resolved at run time")}}},
	{Name: "fallbacks", Role: BlockRole, Opener: "fallbacks", Hosts: []string{"agent", "judge"}, Doc: "The named alternative routes of a node, tried in declaration order when the primary fails (ADR-087/ADR-091); the one block that may not stand bare — a chain with no route is refused.",
		Entries: &Entries{Key: Ident, KeyName: "route", Body: "fallback", SequenceKey: "route", Doc: "One named route per entry, its settings in the indented sub-block; order is the try order"}},
	{Name: "fallback", Role: Entry, Opener: "fallbacks", Hosts: []string{"fallbacks"}, Doc: "One named route of a fallbacks: chain (ADR-087/ADR-091), tried in declaration order.",
		Properties: []Property{
			pBackend, pModel, pProvider,
			Property{Name: "on", Form: IdentList, Values: []string{"usage_window", "auth", "unavailable", "transient_exhausted", "any"},
				Doc: "Failure classes that take this route (default usage_window, unavailable; never any or auth by default)"},
			prop("metered", Bool, "The route spends a metered API key (credential hint)"),
			Property{Name: "action", Form: StringOrIdent, Values: []string{"skip"}, Doc: "skip: complete the node with a zero-value output stamped _skipped instead of failing"},
			prop("when", String, "Expression over vars that gates the route"),
		}},
}, contractKinds...)
