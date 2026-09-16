package unparse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// writeContracts renders the top-level `contract NAME:` declarations. A
// contract with no property, and an entry with none, is a bare header
// followed by a blank line — the form the parser reads as empty.
func (w *fileWriter) writeContracts(contracts []*ast.ContractDecl) {
	for _, c := range contracts {
		w.blankLine()
		fmt.Fprintf(&w.b, "contract %s:\n", declName(&w.b, c.Name))
		mark := w.b.Len()
		if c.DisplayName != "" {
			writeQuotedProp(&w.b, "display_name", c.DisplayName)
		}
		if c.Responsibility != "" {
			writeQuotedProp(&w.b, "responsibility", c.Responsibility)
		}
		if c.Version != nil {
			writeProp(&w.b, "version", strconv.Itoa(*c.Version))
		}
		w.writeContractPorts("inputs", c.Inputs)
		w.writeContractPorts("outputs", c.Outputs)
		if len(c.Criteria) > 0 {
			w.b.WriteString("  criteria:\n")
			for _, k := range c.Criteria {
				fmt.Fprintf(&w.b, "    %s:\n", declName(&w.b, k.Name))
				entry := w.b.Len()
				if k.Kind != "" {
					writeDottedAt(&w.b, "      ", "kind", k.Kind)
				}
				if k.Port != "" {
					writeDottedAt(&w.b, "      ", "port", k.Port)
				}
				if len(k.Params) > 0 {
					fmt.Fprintf(&w.b, "      params: %s\n", w.jsonValue(k.Params))
				}
				endBlock(&w.b, entry)
			}
		}
		if len(c.Effects) > 0 {
			w.b.WriteString("  effects:\n")
			for _, e := range c.Effects {
				fmt.Fprintf(&w.b, "    %s:\n", declName(&w.b, e.Name))
				entry := w.b.Len()
				if e.Description != "" {
					fmt.Fprintf(&w.b, "      description: %s\n", w.b.str(e.Description))
				}
				if e.Paid {
					w.b.WriteString("      paid: true\n")
				}
				endBlock(&w.b, entry)
			}
		}
		endBlock(&w.b, mark)
	}
}

// writeContractPorts renders an `inputs:` or `outputs:` block: one
// `name: type` entry per port, its properties indented under it. A port
// list with no port is not written: nothing distinguishes it from an
// absent one, in the text or in the transport.
func (w *fileWriter) writeContractPorts(key string, ports []*ast.PortDecl) {
	if len(ports) == 0 {
		return
	}
	fmt.Fprintf(&w.b, "  %s:\n", key)
	for _, p := range ports {
		fmt.Fprintf(&w.b, "    %s: %s\n", declName(&w.b, p.Name), portTypeText(&w.b, p.Type))
		if p.Description != "" {
			fmt.Fprintf(&w.b, "      description: %s\n", w.b.str(p.Description))
		}
		if p.Required != nil {
			fmt.Fprintf(&w.b, "      required: %t\n", *p.Required)
		}
		if p.Nullable {
			w.b.WriteString("      nullable: true\n")
		}
		if len(p.Default) > 0 {
			fmt.Fprintf(&w.b, "      default: %s\n", w.jsonValue(p.Default))
		}
		if p.MinItems != nil {
			fmt.Fprintf(&w.b, "      min_items: %d\n", *p.MinItems)
		}
		if p.MaxItems != nil {
			fmt.Fprintf(&w.b, "      max_items: %d\n", *p.MaxItems)
		}
		if p.From != "" {
			writeDottedAt(&w.b, "      ", "from", p.From)
		}
		if p.FileSpec != nil {
			w.b.WriteString("      file:\n")
			mark := w.b.Len()
			if p.FileSpec.MediaType != "" {
				fmt.Fprintf(&w.b, "        media_type: %s\n", w.b.str(p.FileSpec.MediaType))
			}
			if p.FileSpec.MinBytes != 0 {
				fmt.Fprintf(&w.b, "        min_bytes: %d\n", p.FileSpec.MinBytes)
			}
			if p.FileSpec.Schema != "" {
				writeIdentAt(&w.b, "        ", "schema", p.FileSpec.Schema)
			}
			endBlock(&w.b, mark)
		}
	}
}

// declName renders a declaration or entry name: bare when it is an
// identifier, quoted otherwise, so the parser refuses the text AT the name
// — a name holding a newline, written bare, reads as more declarations than
// the document has, and the compiled program (which does not carry the
// contract) cannot tell. Verify names the refusal first (checkContract).
func declName(b *buf, name string) string {
	if isBareIdent(name) {
		return name
	}
	return b.str(name)
}

// portTypeText renders a port's type: a builtin or schema name with its
// `[]` suffixes, bare when the name is an identifier — otherwise quoted,
// which the parser refuses precisely, at the port, instead of a lexer
// error far from the field.
func portTypeText(b *buf, typ string) string {
	base := typ
	for strings.HasSuffix(base, "[]") {
		base = strings.TrimSuffix(base, "[]")
	}
	if isBareIdent(base) {
		return typ
	}
	return b.str(typ)
}

// writeIdentAt is writeIdentProp at the given indentation: bare when the
// value is an identifier, quoted otherwise.
func writeIdentAt(b *buf, indent, key, value string) {
	if isBareIdent(value) {
		fmt.Fprintf(b, "%s%s: %s\n", indent, key, value)
		return
	}
	fmt.Fprintf(b, "%s%s: %s\n", indent, key, b.str(value))
}

// writeDottedAt emits an identifier-shaped property that may be dotted
// (`open_pr.url`, `input.goal`) at the given indentation, quoted when it
// is not one (see writeIdentProp).
func writeDottedAt(b *buf, indent, key, value string) {
	if dottedIdent(value) {
		fmt.Fprintf(b, "%s%s: %s\n", indent, key, value)
		return
	}
	fmt.Fprintf(b, "%s%s: %s\n", indent, key, b.str(value))
}

func dottedIdent(s string) bool {
	for _, seg := range strings.Split(s, ".") {
		if !isBareIdent(seg) {
			return false
		}
	}
	return true
}

// jsonValue renders a default or a criterion's parameters in the
// document's own syntax: strings through the writer's quoting (a v1 `"…"`
// cannot hold a quote or a newline), object keys bare when they are
// identifiers, members in key order — the order the parser gives them
// back. A number is written as it came: one the text has no form for (a
// sign, an exponent) is refused where it is declared (C302) and, before
// that, by the guard, since the text does not parse. Bytes that are not
// one JSON value are written as they came, for the guard to refuse.
func (w *fileWriter) jsonValue(raw json.RawMessage) string {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil || d.More() {
		return string(raw)
	}
	return w.renderJSON(v)
}

func (w *fileWriter) renderJSON(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return x.String()
	case string:
		return w.b.str(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = w.renderJSON(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			key := k
			if !isBareIdent(k) {
				key = w.b.str(k)
			}
			parts[i] = key + ": " + w.renderJSON(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return "null"
}
