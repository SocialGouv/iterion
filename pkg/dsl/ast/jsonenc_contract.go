package ast

import (
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// jsonContractDecl mirrors ContractDecl (contract.go). Ports, criteria and
// effects travel as ordered lists under the AST's own field names, so the
// provenance walk pairs them and a duplicate name reaches the compiler.
type jsonContractDecl struct {
	// File is the declaration's file of origin, set by
	// MarshalFileWithProvenance alone: the transport never carries it.
	File           string               `json:"file,omitempty"`
	Name           string               `json:"name,omitempty"`
	DisplayName    string               `json:"display_name,omitempty"`
	Responsibility string               `json:"responsibility,omitempty"`
	Version        *int                 `json:"version,omitempty"`
	Inputs         []*jsonPortDecl      `json:"inputs,omitempty"`
	Outputs        []*jsonPortDecl      `json:"outputs,omitempty"`
	Criteria       []*jsonCriterionDecl `json:"criteria,omitempty"`
	Effects        []*jsonPublicEffect  `json:"effects,omitempty"`
}

// jsonPortDecl mirrors PortDecl. Required is a pointer so "unset" (true by
// default) and an explicit false both travel; Default is raw JSON so an
// explicit null stays distinct from an absent default — omitempty drops the
// absent one alone, the bytes `null` have a length. The `file:` block is
// keyed file_spec: `file` is the provenance key.
type jsonPortDecl struct {
	Name        string            `json:"name,omitempty"`
	Type        string            `json:"type,omitempty"`
	Description string            `json:"description,omitempty"`
	Required    *bool             `json:"required,omitempty"`
	Nullable    bool              `json:"nullable,omitempty"`
	Default     json.RawMessage   `json:"default,omitempty"`
	MinItems    *int              `json:"min_items,omitempty"`
	MaxItems    *int              `json:"max_items,omitempty"`
	From        string            `json:"from,omitempty"`
	FileSpec    *jsonPortFileDecl `json:"file_spec,omitempty"`
}

type jsonPortFileDecl struct {
	MediaType string `json:"media_type,omitempty"`
	MinBytes  int64  `json:"min_bytes,omitempty"`
	Schema    string `json:"schema,omitempty"`
}

type jsonCriterionDecl struct {
	Name   string          `json:"name,omitempty"`
	Kind   string          `json:"kind,omitempty"`
	Port   string          `json:"port,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type jsonPublicEffect struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Paid        bool   `json:"paid,omitempty"`
}

// contractToJSON builds the mirror. A JSON value that is not one public
// JSON value (a duplicate member, trailing bytes) is refused here, naming
// the port or criterion — in this direction too, so an AST built in Go (a
// tool, a test) never leaves as a document the reader would refuse; what
// leaves is canonical, the bytes the parser and the reader hold.
func contractToJSON(c *ContractDecl) (*jsonContractDecl, error) {
	jc := &jsonContractDecl{
		Name:           c.Name,
		DisplayName:    c.DisplayName,
		Responsibility: c.Responsibility,
		Version:        c.Version,
	}
	for i, p := range c.Inputs {
		if p == nil {
			return nil, fmt.Errorf("astjson: contract %q inputs[%d] is nil — every element of that list is an object", c.Name, i)
		}
		jp, err := portToJSON(c.Name, "input", p)
		if err != nil {
			return nil, err
		}
		jc.Inputs = append(jc.Inputs, jp)
	}
	for i, p := range c.Outputs {
		if p == nil {
			return nil, fmt.Errorf("astjson: contract %q outputs[%d] is nil — every element of that list is an object", c.Name, i)
		}
		jp, err := portToJSON(c.Name, "output", p)
		if err != nil {
			return nil, err
		}
		jc.Outputs = append(jc.Outputs, jp)
	}
	for i, k := range c.Criteria {
		if k == nil {
			return nil, fmt.Errorf("astjson: contract %q criteria[%d] is nil — every element of that list is an object", c.Name, i)
		}
		params, err := canonicalJSON(k.Params)
		if err != nil {
			return nil, fmt.Errorf("astjson: contract %q criterion %q params: %w", c.Name, k.Name, err)
		}
		jc.Criteria = append(jc.Criteria, &jsonCriterionDecl{Name: k.Name, Kind: k.Kind, Port: k.Port, Params: params})
	}
	for i, e := range c.Effects {
		if e == nil {
			return nil, fmt.Errorf("astjson: contract %q effects[%d] is nil — every element of that list is an object", c.Name, i)
		}
		jc.Effects = append(jc.Effects, &jsonPublicEffect{Name: e.Name, Description: e.Description, Paid: e.Paid})
	}
	return jc, nil
}

func portToJSON(contract, side string, p *PortDecl) (*jsonPortDecl, error) {
	def, err := canonicalJSON(p.Default)
	if err != nil {
		return nil, fmt.Errorf("astjson: contract %q %s %q default: %w", contract, side, p.Name, err)
	}
	jp := &jsonPortDecl{
		Name:        p.Name,
		Type:        p.Type,
		Description: p.Description,
		Required:    p.Required,
		Nullable:    p.Nullable,
		Default:     def,
		MinItems:    p.MinItems,
		MaxItems:    p.MaxItems,
		From:        p.From,
	}
	if p.FileSpec != nil {
		jp.FileSpec = &jsonPortFileDecl{MediaType: p.FileSpec.MediaType, MinBytes: p.FileSpec.MinBytes, Schema: p.FileSpec.Schema}
	}
	return jp, nil
}

func contractFromJSON(jc *jsonContractDecl) (*ContractDecl, error) {
	c := &ContractDecl{
		Name:           jc.Name,
		DisplayName:    jc.DisplayName,
		Responsibility: jc.Responsibility,
		Version:        jc.Version,
	}
	for _, jp := range jc.Inputs {
		p, err := portFromJSON(jc.Name, "input", jp)
		if err != nil {
			return nil, err
		}
		c.Inputs = append(c.Inputs, p)
	}
	for _, jp := range jc.Outputs {
		p, err := portFromJSON(jc.Name, "output", jp)
		if err != nil {
			return nil, err
		}
		c.Outputs = append(c.Outputs, p)
	}
	for _, jk := range jc.Criteria {
		params, err := canonicalJSON(jk.Params)
		if err != nil {
			return nil, fmt.Errorf("astjson: contract %q criterion %q params: %w", jc.Name, jk.Name, err)
		}
		c.Criteria = append(c.Criteria, &CriterionDecl{Name: jk.Name, Kind: jk.Kind, Port: jk.Port, Params: params})
	}
	for _, je := range jc.Effects {
		c.Effects = append(c.Effects, &PublicEffect{Name: je.Name, Description: je.Description, Paid: je.Paid})
	}
	return c, nil
}

func portFromJSON(contract, side string, jp *jsonPortDecl) (*PortDecl, error) {
	def, err := canonicalJSON(jp.Default)
	if err != nil {
		return nil, fmt.Errorf("astjson: contract %q %s %q default: %w", contract, side, jp.Name, err)
	}
	p := &PortDecl{
		Name:        jp.Name,
		Type:        jp.Type,
		Description: jp.Description,
		Required:    jp.Required,
		Nullable:    jp.Nullable,
		Default:     def,
		MinItems:    jp.MinItems,
		MaxItems:    jp.MaxItems,
		From:        jp.From,
	}
	if jp.FileSpec != nil {
		p.FileSpec = &PortFileDecl{MediaType: jp.FileSpec.MediaType, MinBytes: jp.FileSpec.MinBytes, Schema: jp.FileSpec.Schema}
	}
	return p, nil
}

// canonicalJSON is the one form a JSON value has in the AST: compact,
// members in key order, numbers as written — the form the parser reads a
// value into, so a document from the transport and the text it is saved
// as compare equal. An absent value stays absent; a document whose value
// is not one public JSON value (a duplicate member, trailing bytes) is
// refused here, where it is named, never carried on for a writer or a
// compiler to meet.
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	value, err := spec.DecodePublicJSON(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
