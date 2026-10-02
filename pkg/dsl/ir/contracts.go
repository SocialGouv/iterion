package ir

import "encoding/json"

// PublicContract is a compiled `contract` declaration, bound to the program
// that keeps it (ADR-099): every input is a declared var, every output names
// the node and field that produce it, every criterion a port and an
// evaluator. It carries no evaluator function — reflect.DeepEqual holds two
// compiled programs equal (SameProgram), and a func is never equal to
// another — so the runtime compiles a criterion's evaluator when it runs.
// The JSON tags are the wire form `iterion validate --json` and the MCP
// local_validate tool return (`public_contract`): snake_case like every
// other key of that document.
type PublicContract struct {
	Name           string             `json:"name"`
	DisplayName    string             `json:"display_name,omitempty"`
	Responsibility string             `json:"responsibility,omitempty"`
	Version        int                `json:"version"` // 1 when unset
	Inputs         []*PublicPort      `json:"inputs,omitempty"`
	Outputs        []*PublicPort      `json:"outputs,omitempty"`
	Criteria       []*PublicCriterion `json:"criteria,omitempty"`
	Effects        []*PublicEffect    `json:"effects,omitempty"`
}

// PublicPort is one input or output of a contract. Default is nil when the
// port declares none and the bytes `null` for an explicit null — omitempty
// drops the absent one alone, so the wire keeps the two apart.
type PublicPort struct {
	Name        string          `json:"name"`
	Type        string          `json:"type"` // as declared: a builtin (`string`, `int`, …, `string[]`) or a schema name, with `[]` suffixes
	Description string          `json:"description,omitempty"`
	Required    bool            `json:"required"`
	Nullable    bool            `json:"nullable,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"` // canonical (compact, keys in order)
	MinItems    *int            `json:"min_items,omitempty"`
	MaxItems    *int            `json:"max_items,omitempty"`
	// EnumValues are the values an input's var admits (`[enum: …]`): the
	// domain the program accepts, carried so a reader of the contract is
	// never told a wider one.
	EnumValues []string `json:"enum,omitempty"`
	// Matching is the RE2 pattern an input's var admits
	// (`[matching: …]`), carried for the same reason as EnumValues: a
	// reader of the contract is never told a wider domain than the
	// program accepts.
	Matching string `json:"matching,omitempty"`
	// FromNode and FromField bind an output to its producer: the node, and
	// for a value the field of its output schema; a file port, or a port
	// typed with the node's output schema, names the node alone.
	FromNode  string      `json:"from_node,omitempty"`
	FromField string      `json:"from_field,omitempty"`
	File      *PublicFile `json:"file,omitempty"`
}

// PublicFile is the verifiable shape of a file-valued port.
type PublicFile struct {
	MediaType string `json:"media_type,omitempty"`
	MinBytes  int64  `json:"min_bytes,omitempty"`
	Schema    string `json:"schema,omitempty"`
}

// PublicCriterion is a deterministic check on a port. Registered reports
// whether an evaluator ships for Kind: a criterion of an unregistered kind
// is kept and rendered, not evaluated (C303).
type PublicCriterion struct {
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	Port       string          `json:"port"` // `input.<name>` or `output.<name>`
	Params     json.RawMessage `json:"params,omitempty"`
	Registered bool            `json:"registered"`
}

// PublicEffect is a visible operation the contract documents.
type PublicEffect struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Paid        bool   `json:"paid,omitempty"`
}
