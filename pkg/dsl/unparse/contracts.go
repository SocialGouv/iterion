package unparse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

func (w *fileWriter) contractProp(indent, name, value string, quoted bool) {
	if quoted {
		value = w.b.str(value)
	}
	fmt.Fprintf(&w.b, "%s%s: %s\n", indent, name, value)
}

func (w *fileWriter) writeContracts(contracts []*ast.ContractDecl) {
	for _, c := range contracts {
		w.blankLine()
		fmt.Fprintf(&w.b, "contract %s:\n", c.Name)
		mark := w.b.Len()
		if c.DisplayName != "" {
			w.contractProp("  ", "display_name", c.DisplayName, true)
		}
		if c.Responsibility != "" {
			w.contractProp("  ", "responsibility", c.Responsibility, true)
		}
		if c.Version != 0 {
			fmt.Fprintf(&w.b, "  version: %d\n", c.Version)
		}
		w.writeContractPorts("inputs", c.Inputs)
		w.writeContractPorts("outputs", c.Outputs)
		if len(c.Criteria) > 0 {
			w.b.WriteString("  criteria:\n")
			for _, rule := range c.Criteria {
				fmt.Fprintf(&w.b, "    %s:\n", rule.Name)
				mark := w.b.Len()
				if rule.Kind != "" {
					w.contractProp("      ", "kind", rule.Kind, false)
				}
				if rule.Port != "" {
					w.contractProp("      ", "port", rule.Port, false)
				}
				if rule.Params != nil {
					w.contractProp("      ", "params", w.contractJSON(rule.Params), false)
				}
				if w.b.Len() == mark {
					w.b.WriteString("\n")
				}
			}
		}
		if len(c.Effects) > 0 {
			w.b.WriteString("  effects:\n")
			for _, effect := range c.Effects {
				fmt.Fprintf(&w.b, "    %s:\n", effect.Name)
				w.contractProp("      ", "description", effect.Description, true)
				fmt.Fprintf(&w.b, "      paid: %t\n", effect.Paid)
			}
		}
		w.ensureBody(mark)
	}
}

func (w *fileWriter) writeContractPorts(name string, ports []*ast.PortDecl) {
	if len(ports) == 0 {
		return
	}
	fmt.Fprintf(&w.b, "  %s:\n", name)
	for _, p := range ports {
		fmt.Fprintf(&w.b, "    %s: %s\n", p.Name, p.Type)
		if p.Description != "" {
			w.contractProp("      ", "description", p.Description, true)
		}
		if p.Required != nil {
			fmt.Fprintf(&w.b, "      required: %t\n", *p.Required)
		}
		if p.Nullable {
			w.b.WriteString("      nullable: true\n")
		}
		if p.Default != nil {
			w.contractProp("      ", "default", w.contractJSON(p.Default), false)
		}
		if p.MinItems != nil {
			fmt.Fprintf(&w.b, "      min_items: %d\n", *p.MinItems)
		}
		if p.MaxItems != nil {
			fmt.Fprintf(&w.b, "      max_items: %d\n", *p.MaxItems)
		}
		if f := p.File; f != nil {
			w.b.WriteString("      file:\n")
			mark := w.b.Len()
			if f.MediaType != "" {
				w.contractProp("        ", "media_type", f.MediaType, true)
			}
			if f.MinBytes != 0 {
				fmt.Fprintf(&w.b, "        min_bytes: %d\n", f.MinBytes)
			}
			if f.Schema != "" {
				w.contractProp("        ", "schema", f.Schema, false)
			}
			if w.b.Len() == mark {
				w.b.WriteString("\n")
			}
		}
	}
}

func (w *fileWriter) writePortPolicies(policies []*ast.PortPolicyDecl) {
	for _, p := range policies {
		w.blankLine()
		fmt.Fprintf(&w.b, "port_policy %s:\n", p.Name)
		mark := w.b.Len()
		if p.MaxMapItems != 0 {
			fmt.Fprintf(&w.b, "  max_map_items: %d\n", p.MaxMapItems)
		}
		if len(p.Effects) > 0 {
			w.b.WriteString("  effects:\n")
			for _, e := range p.Effects {
				fmt.Fprintf(&w.b, "    %s:\n", e.Name)
				mark := w.b.Len()
				if e.Resource != "" {
					w.contractProp("      ", "resource", e.Resource, false)
				}
				if e.Recovery != "" {
					w.contractProp("      ", "recovery", e.Recovery, false)
				}
				if e.Verifier != "" {
					w.contractProp("      ", "verifier", e.Verifier, true)
				}
				if w.b.Len() == mark {
					w.b.WriteString("\n")
				}
			}
		}
		w.ensureBody(mark)
	}
}

func (w *fileWriter) writePortGraph(g *ast.PortGraphDecl) {
	w.b.WriteString("  graph:\n")
	mark := w.b.Len()
	if len(g.Nodes) > 0 {
		w.b.WriteString("    nodes:\n")
		for _, n := range g.Nodes {
			fmt.Fprintf(&w.b, "      %s:\n", n.Name)
			mark := w.b.Len()
			if n.Implementation != "" {
				w.contractProp("        ", "implementation", n.Implementation, false)
			}
			if n.Contract != "" {
				w.contractProp("        ", "contract", n.Contract, false)
			}
			if n.Policy != "" {
				w.contractProp("        ", "policy", n.Policy, false)
			}
			if w.b.Len() == mark {
				w.b.WriteString("\n")
			}
		}
	}
	if len(g.Bindings) > 0 {
		w.b.WriteString("    bindings:\n")
		for _, b := range g.Bindings {
			fmt.Fprintf(&w.b, "      %s -> %s\n", b.From, b.To)
		}
	}
	if len(g.Exports) > 0 {
		w.b.WriteString("    exports:\n")
		for _, e := range g.Exports {
			fmt.Fprintf(&w.b, "      %s: %s\n", e.Name, e.From)
		}
	}
	if len(g.Products) > 0 {
		fmt.Fprintf(&w.b, "    products: [%s]\n", quoteList(&w.b, g.Products))
	}
	if w.b.Len() == mark {
		w.b.WriteString("\n")
	}
}

// JSON payloads use the same string writer as the rest of the document,
// including strict-escape fallback for profile 1. Printing raw JSON would
// reinterpret backslashes when the file is read under that profile.
func (w *fileWriter) contractJSON(raw json.RawMessage) string {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		// Invalid AST data must not silently turn into a valid default.
		// Preserve its spelling so parsing/validation reports the error.
		return string(raw)
	}
	return w.contractJSONValue(value)
}

func (w *fileWriter) contractJSONValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return w.b.str(v)
	case json.Number:
		return v.String()
	case bool:
		return fmt.Sprint(v)
	case []any:
		parts := make([]string, len(v))
		for i, element := range v {
			parts[i] = w.contractJSONValue(element)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, w.b.str(key)+": "+w.contractJSONValue(v[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		panic(fmt.Sprintf("unexpected decoded contract JSON value %T", value))
	}
}
