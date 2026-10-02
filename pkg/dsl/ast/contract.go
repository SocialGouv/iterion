package ast

import "encoding/json"

// ContractDecl is the public interface of a bot: what it takes, what it
// produces, the files it delivers, the deterministic checks and the visible
// effects — declared once at top level, named by the workflow's `contract:`.
// It contains no prompt, provider or tool. Ports are ordered declarations,
// not maps: a duplicate name reaches the compiler to be diagnosed instead
// of silently overwritten. Every field of this file is mirrored by the JSON
// transport (jsonenc.go) and written by the unparser (unparse/contracts.go),
// both held by the sweeps of their packages.
type ContractDecl struct {
	Name           string
	DisplayName    string
	Responsibility string
	// Version is the public contract version as written; nil is unset (1).
	// A pointer so an explicit value — 0 included, which the compiler
	// refuses — survives every transport instead of vanishing on a save.
	Version  *int
	Inputs   []*PortDecl
	Outputs  []*PortDecl
	Criteria []*CriterionDecl
	Effects  []*PublicEffect
	// Comments are the `##` lines written around this declaration, each
	// carrying the address it was written at (Comment.Anchor, Comment.Place).
	Comments []*Comment
	Span     Span
}

// PortDecl describes one named value. Type is a builtin or a declared
// schema name, optionally followed by []. Required defaults to true. A nil
// Default means absence; the bytes "null" mean an explicitly supplied null;
// neither is an empty collection — the distinctions survive the transport.
// From binds an output to the program: `node.field` for a value, `node`
// for a file; an input carries none (the compiler refuses one, C300).
type PortDecl struct {
	Name        string
	Type        string
	Description string
	Required    *bool
	Nullable    bool
	Default     json.RawMessage
	MinItems    *int
	MaxItems    *int
	From        string
	// FileSpec is the port's `file:` block. Named FileSpec, not File, on
	// purpose: the JSON key `file` is the provenance key every mirror
	// carries (the file a declaration came from), and the provenance
	// writer pairs AST and mirror fields by name.
	FileSpec *PortFileDecl
	Span     Span
}

// IsRequired reports the port's requiredness, true when unset.
func (p *PortDecl) IsRequired() bool { return p.Required == nil || *p.Required }

// PortFileDecl adds verifiable properties to a file-valued port. File
// existence, ownership and immutable publication are the runtime's checks,
// never an assertion an author can omit.
type PortFileDecl struct {
	MediaType string
	MinBytes  int64
	Schema    string
	Span      Span
}

// CriterionDecl names a deterministic, registry-backed check on a port.
// Params are data; descriptive prose is never a predicate.
type CriterionDecl struct {
	Name   string
	Kind   string
	Port   string
	Params json.RawMessage
	Span   Span
}

// PublicEffect describes an externally visible operation of the bot. It
// documents; it is not an admission grant.
type PublicEffect struct {
	Name        string
	Description string
	Paid        bool
	Span        Span
}
