package spec

// The public contract of a bot: what it takes, what it produces, the files
// it delivers, the deterministic checks that condition what follows, its
// visible effects — the face of the bot the catalogue, the snapshot and a
// parent's `subbot` read, declared once at top level and named by the
// workflow (`contract: <name>`). It carries no prompt, tool or provider
// setting. A contract is BOUND to the program it describes: an input is a
// declared var, an output names the node and field that produce it, a file
// names its producing node — what the compiler checks (C300–C304) so that a
// contract the program does not keep is refused rather than displayed.
// Concept harvested from #1216 (ADR-099); the graph that would compose
// contracts across nodes waits for the execution semantics that runs it.
var contractKinds = []Kind{
	{Name: "contract", Role: Declaration, Doc: "The public interface of a bot, named by the workflow's `contract:`; no prompts, tools or provider configuration. A bare header followed by a blank line or the end of the file declares an empty contract.",
		Properties: []Property{
			prop("display_name", String, "Explicit human-readable name"),
			prop("responsibility", String, "The single responsibility this bot fulfils"),
			prop("version", Int, "Public contract version, 1 or more (C300); defaults to 1"),
			block("inputs", "contract.ports", "Named typed values and files the bot takes; each one is a declared var (C300)"),
			block("outputs", "contract.ports", "Named typed values and files the bot produces on success; each one names the node and field that produce it (C301)"),
			block("criteria", "contract.criteria", "Deterministic registered checks on a port; prose is not executable"),
			block("effects", "contract.effects", "Visible effects, including paid operations"),
		}},
	{Name: "contract.ports", Role: BlockRole, Opener: "inputs / outputs", Hosts: []string{"contract"}, Doc: "Public input or output ports, in declaration order.",
		Entries: &Entries{Key: Ident, Body: "contract.port", Doc: "One port per line, its properties in the optional indented sub-block",
			Fields: []Field{field("type", TypeRef, "<type>", "A builtin type or a declared schema's name, optionally followed by []")}}},
	{Name: "contract.port", Role: Entry, Opener: "inputs / outputs", Hosts: []string{"contract.ports"}, Doc: "One named typed port. Missing, null and [] remain different values.",
		Properties: []Property{
			prop("description", String, "Meaning of the value"),
			prop("required", Bool, "Mandatory port (default true). An input mirrors its var — required exactly when the var has no default — and a written value that disagrees is refused (C300)"),
			prop("nullable", Bool, "Permit an explicit null value (default false); on an input whose var has no default, the one way to be optional — omitted, the var is null (C300)"),
			prop("default", JSON, "Typed default of an optional input: the var's default, read as the launch reads a value of its type (a `string[]` or `json` var's text as a list or an object) — written, it must be the var's (C300); omitted, the var's is the port's. One JSON value on one line — `\"text\"`, `12`, `true`, `null`, `[...]`, `{key: value}` — with no signed number and no exponent, which the text cannot write and the compiler refuses from a document (C302)"),
			prop("min_items", Int, "Minimum array cardinality (C300 on an input, C301 on an output)"),
			prop("max_items", Int, "Maximum array cardinality (C300 on an input, C301 on an output)"),
			prop("from", DottedIdent, "Producer of an output: `node.field` for a value, `node` for a file (C301); refused on an input (C300)"),
			block("file", "contract.file", "Properties of a delivered or consumed file; existence and provenance are the runtime's checks"),
		}},
	{Name: "contract.file", Role: BlockRole, Opener: "file", Hosts: []string{"contract.port"}, Doc: "Verifiable properties of a produced or consumed file.",
		Properties: []Property{
			prop("media_type", String, "Expected media type"),
			prop("min_bytes", Int, "Minimum file size"),
			prop("schema", Ident, "Schema of structured file contents"),
		}},
	{Name: "contract.criteria", Role: BlockRole, Opener: "criteria", Hosts: []string{"contract"}, Doc: "Deterministic acceptance checks on the ports, each a registered evaluator with JSON parameters — never prose.",
		Entries: &Entries{Key: Ident, Body: "contract.criterion", Doc: "A named deterministic acceptance check, described in its indented sub-block"}},
	{Name: "contract.criterion", Role: Entry, Opener: "criteria", Hosts: []string{"contract.criteria"}, Doc: "One named check: an evaluator, the port it reads, its parameters. A bare header declares a check nothing evaluates yet.",
		Properties: []Property{
			prop("kind", DottedIdent, "Registered deterministic validator (see the criteria table; a plugin's may be dotted); an unregistered kind is declared but not evaluated (C303)"),
			prop("port", DottedIdent, "Checked port, singular: `input.<name>` or `output.<name>` (C302)"),
			prop("params", JSON, "Parameters validated against the criterion's parameter declaration (C302): one JSON object on one line, e.g. `{min: 2}`"),
		}},
	{Name: "contract.effects", Role: BlockRole, Opener: "effects", Hosts: []string{"contract"}, Doc: "The bot's externally visible operations, documented; an effect grants nothing — the sandbox, the allow-lists and the verified actions keep the admission.",
		Entries: &Entries{Key: Ident, Body: "contract.effect", Doc: "A visible effect of the bot, described in its indented sub-block"}},
	{Name: "contract.effect", Role: Entry, Opener: "effects", Hosts: []string{"contract.effects"}, Doc: "One named effect and whether it may cost money.",
		Properties: []Property{
			prop("description", String, "Externally visible operation"),
			prop("paid", Bool, "The operation may incur a charge; unknown cost remains unknown"),
		}},
}
