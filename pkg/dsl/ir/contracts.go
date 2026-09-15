package ir

import "encoding/json"

// PublicContract is a compiled `contract` declaration, bound to the program
// that keeps it (ADR-099): every input is a declared var, every output names
// the node and field that produce it, every criterion a port and an
// evaluator. It carries no evaluator function — reflect.DeepEqual holds two
// compiled programs equal (SameProgram), and a func is never equal to
// another — so the runtime compiles a criterion's evaluator when it runs.
type PublicContract struct {
	Name           string
	DisplayName    string
	Responsibility string
	Version        int // 1 when unset
	Inputs         []*PublicPort
	Outputs        []*PublicPort
	Criteria       []*PublicCriterion
	Effects        []*PublicEffect
}

// PublicPort is one input or output of a contract.
type PublicPort struct {
	Name        string
	Type        string // as declared: a builtin (`string`, `int`, …, `string[]`) or a schema name, with `[]` suffixes
	Description string
	Required    bool
	Nullable    bool
	Default     json.RawMessage // canonical (compact, keys in order); nil = absent, `null` = an explicit null
	MinItems    *int
	MaxItems    *int
	// FromNode and FromField bind an output to its producer: the node, and
	// for a value the field of its output schema; a file port, or a port
	// typed with the node's output schema, names the node alone.
	FromNode  string
	FromField string
	File      *PublicFile
}

// PublicFile is the verifiable shape of a file-valued port.
type PublicFile struct {
	MediaType string
	MinBytes  int64
	Schema    string
}

// PublicCriterion is a deterministic check on a port. Registered reports
// whether an evaluator ships for Kind: a criterion of an unregistered kind
// is kept and rendered, not evaluated (C303).
type PublicCriterion struct {
	Name       string
	Kind       string
	Port       string // `input.<name>` or `output.<name>`
	Params     json.RawMessage
	Registered bool
}

// PublicEffect is a visible operation the contract documents.
type PublicEffect struct {
	Name        string
	Description string
	Paid        bool
}
