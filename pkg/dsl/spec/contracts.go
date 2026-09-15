package spec

// The public contract of a bot: what it takes, what it produces, the files
// it delivers, the deterministic checks that condition what follows, its
// visible effects — the face of the bot the catalogue, the snapshot and a
// parent's `subbot` read, declared once at top level and named by the
// workflow (`contract: <name>`). It carries no prompt, tool or provider
// setting. A contract is BOUND to the program it describes: an input is a
// declared var, an output names the node and field that produce it, a file
// names its producing node — what the compiler checks (C300–C302) so that a
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
		Entries: &Entries{Shape: "name: type [optional indented properties]", Doc: "Builtin or declared schema type, optionally followed by []", Body: "contract.port"}},
	{Name: "contract.port", Role: Entry, Opener: "inputs / outputs", Hosts: []string{"contract.ports"}, Doc: "One named typed port. Missing, null and [] remain different values.",
		Properties: []Property{
			prop("description", String, "Meaning of the value"),
			prop("required", Bool, "Mandatory port (default true)"),
			prop("nullable", Bool, "Permit an explicit null value (default false)"),
			prop("default", JSON, "Typed default of an optional input (C300); omission means absence. One JSON value on one line — `\"text\"`, `12`, `true`, `null`, `[...]`, `{key: value}` — with no signed number and no exponent, which the text cannot write (C302)"),
			prop("min_items", Int, "Minimum array cardinality (C301)"),
			prop("max_items", Int, "Maximum array cardinality (C301)"),
			prop("from", Ident, "Producer of an output: `node.field` for a value, `node` for a file (C301); refused on an input (C300)"),
			block("file", "contract.file", "Properties of a delivered or consumed file; existence and provenance are the runtime's checks"),
		}},
	{Name: "contract.file", Role: BlockRole, Opener: "file", Hosts: []string{"contract.port"}, Doc: "Verifiable properties of a produced or consumed file.",
		Properties: []Property{
			prop("media_type", String, "Expected media type"),
			prop("min_bytes", Int, "Minimum file size"),
			prop("schema", Ident, "Schema of structured file contents"),
		}},
	{Name: "contract.criteria", Role: BlockRole, Opener: "criteria", Hosts: []string{"contract"}, Doc: "Deterministic acceptance checks on the ports, each a registered evaluator with JSON parameters — never prose.",
		Entries: &Entries{Shape: "name: [indented criterion properties]", Doc: "Named deterministic acceptance check", Body: "contract.criterion"}},
	{Name: "contract.criterion", Role: Entry, Opener: "criteria", Hosts: []string{"contract.criteria"}, Doc: "One named check: an evaluator, the port it reads, its parameters. A bare header declares a check nothing evaluates yet.",
		Properties: []Property{
			prop("kind", Ident, "Registered deterministic validator (see the criteria table; a plugin's may be dotted); an unregistered kind is declared but not evaluated (C303)"),
			prop("port", Ident, "Checked port, singular: `input.<name>` or `output.<name>` (C302)"),
			prop("params", JSON, "Parameters validated against the criterion's parameter declaration (C302): one JSON object on one line, e.g. `{min: 2}`"),
		}},
	{Name: "contract.effects", Role: BlockRole, Opener: "effects", Hosts: []string{"contract"}, Doc: "The bot's externally visible operations, documented; an effect grants nothing — the sandbox, the allow-lists and the verified actions keep the admission.",
		Entries: &Entries{Shape: "name: [indented effect properties]", Doc: "A visible effect of the bot", Body: "contract.effect"}},
	{Name: "contract.effect", Role: Entry, Opener: "effects", Hosts: []string{"contract.effects"}, Doc: "One named effect and whether it may cost money.",
		Properties: []Property{
			prop("description", String, "Externally visible operation"),
			prop("paid", Bool, "The operation may incur a charge; unknown cost remains unknown"),
		}},
}
