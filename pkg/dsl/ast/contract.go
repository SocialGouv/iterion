package ast

import "encoding/json"

// ContractDecl is the public interface shared by an executable node and a
// reusable workflow. It deliberately contains no prompts, providers or tools.
// Ports are ordered declarations, rather than maps: duplicate names must reach
// the compiler so they can be diagnosed instead of silently overwritten.
type ContractDecl struct {
	Name           string           `json:"name"`
	DisplayName    string           `json:"display_name"`
	Responsibility string           `json:"responsibility"`
	Version        int              `json:"version,omitempty"`
	Inputs         []*PortDecl      `json:"inputs,omitempty"`
	Outputs        []*PortDecl      `json:"outputs,omitempty"`
	Criteria       []*CriterionDecl `json:"criteria,omitempty"`
	Effects        []*PublicEffect  `json:"effects,omitempty"`
	Span           Span             `json:"-"`
}

// PortDecl describes a named value. Type is a builtin or a resolved schema
// reference, optionally followed by []. Required defaults to true. A nil
// Default means absence; the bytes "null" mean an explicitly supplied null.
// Neither is an empty collection. These distinctions survive the editor wire.
type PortDecl struct {
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Description string          `json:"description,omitempty"`
	Required    *bool           `json:"required,omitempty"`
	Nullable    bool            `json:"nullable,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	MinItems    *int            `json:"min_items,omitempty"`
	MaxItems    *int            `json:"max_items,omitempty"`
	File        *PortFileDecl   `json:"file,omitempty"`
	Span        Span            `json:"-"`
}

func (p *PortDecl) IsRequired() bool { return p.Required == nil || *p.Required }

// PortFileDecl adds verifiable requirements to a file-valued port. File
// existence, invocation ownership and immutable publication are mandatory
// runtime checks, not opt-in assertions an author can accidentally omit.
type PortFileDecl struct {
	MediaType string `json:"media_type,omitempty"`
	MinBytes  int64  `json:"min_bytes,omitempty"`
	Schema    string `json:"schema,omitempty"`
	Span      Span   `json:"-"`
}

// CriterionDecl names a deterministic, registry-backed validation rule.
// Parameters are data; descriptive prose is never interpreted as a predicate.
type CriterionDecl struct {
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Port   string          `json:"port"`
	Params json.RawMessage `json:"params,omitempty"`
	Span   Span            `json:"-"`
}

// PublicEffect describes an externally visible operation. Its name binds to
// the implementation's technical effect policy; it is not an admission grant.
type PublicEffect struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Paid        bool   `json:"paid,omitempty"`
	Span        Span   `json:"-"`
}

// PortGraphDecl is orchestration only: instances, named bindings and exports.
// Product outputs are selected explicitly from the public workflow outputs.
// Exporting an output does not prevent that same output feeding another node.
type PortGraphDecl struct {
	Nodes    []*PortNodeDecl    `json:"nodes,omitempty"`
	Bindings []*PortBindingDecl `json:"bindings,omitempty"`
	Exports  []*PortExportDecl  `json:"exports,omitempty"`
	Products []string           `json:"products,omitempty"`
	Span     Span               `json:"-"`
}

type PortNodeDecl struct {
	Name           string `json:"name"`
	Implementation string `json:"implementation"`
	Contract       string `json:"contract"`
	Policy         string `json:"policy,omitempty"`
	Span           Span   `json:"-"`
}

// Endpoints are explicit node.port references; input.port names a workflow
// input. Keeping each binding's span allows duplicate-supplier errors to point
// at the conflicting line, even when the two bindings have identical text.
type PortBindingDecl struct {
	From string `json:"from"`
	To   string `json:"to"`
	Span Span   `json:"-"`
}

type PortExportDecl struct {
	Name string `json:"name"`
	From string `json:"from"`
	Span Span   `json:"-"`
}

// PortPolicyDecl belongs to the technical configuration. Limits and effect
// recovery policies stay out of the graph and the reusable public contract.
type PortPolicyDecl struct {
	Name        string              `json:"name"`
	MaxMapItems int                 `json:"max_map_items,omitempty"`
	Effects     []*EffectPolicyDecl `json:"effects,omitempty"`
	Span        Span                `json:"-"`
}

type EffectPolicyDecl struct {
	Name     string `json:"name"`
	Resource string `json:"resource,omitempty"`
	Recovery string `json:"recovery"`
	Verifier string `json:"verifier,omitempty"`
	Span     Span   `json:"-"`
}
