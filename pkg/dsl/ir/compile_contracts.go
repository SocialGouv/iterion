package ir

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// The public contract, bound to the program (ADR-099). A contract the
// program does not keep is refused here rather than displayed: the
// catalogue, the snapshot and a parent's `subbot` trust what a contract
// says.
const (
	DiagContractInput            DiagCode = "C300" // an input the program does not keep (not a declared var, another type, a `from:`, a `file:`, a `required:` or `default:` the var contradicts), a contract declared twice, a version below 1, a workflow naming no declared contract
	DiagContractOutput           DiagCode = "C301" // an output the program does not produce: no `from:`, an unknown node or field, another type, a default, a file port on a node that publishes none, an undeclared file schema
	DiagContractCriterion        DiagCode = "C302" // a criterion or a value the contract cannot hold: an unknown port, a type the evaluator does not take, invalid parameters, a default the text cannot write or of another type
	DiagContractUnknownKind      DiagCode = "C303" // warning: a criterion's kind has no registered evaluator — declared, not evaluated
	DiagContractOutputOffSuccess DiagCode = "C304" // warning: an output's producer is on no path to done — produced only when the bot fails
)

// compilePublicContracts binds every contract of the unit to the program —
// its vars, nodes, schemas and edges are compiled by now — and returns them
// by name with the one the workflow keeps. Every contract is held, named by
// the workflow or not: a contract describes THIS program.
func (c *compiler) compilePublicContracts(wf *ast.WorkflowDecl, vars map[string]*Var, edges []*Edge) (map[string]*PublicContract, *PublicContract) {
	contracts := map[string]*PublicContract{}
	succeeds := nodesOnAPathToDone(c.nodes, edges)
	for _, decl := range c.file.Contracts {
		if decl == nil {
			continue
		}
		if _, dup := contracts[decl.Name]; dup {
			c.errorfAtSpan(DiagContractInput, decl.Span, "contract %q is declared twice: a name declares one contract", decl.Name)
			continue
		}
		contracts[decl.Name] = c.compilePublicContract(decl, vars, succeeds)
	}
	if wf.Contract == "" {
		return contracts, nil
	}
	bound, ok := contracts[wf.Contract]
	if !ok {
		c.errorfAtSpan(DiagContractInput, wf.Span, "workflow %q names contract %q, which no `contract %s:` declares", wf.Name, wf.Contract, wf.Contract)
		return contracts, nil
	}
	return contracts, bound
}

// nodesOnAPathToDone is the set of nodes from which `done` is reachable
// along the edges, conditions aside — what the bot can produce on success.
// A node with no outgoing edge at all is counted in: the compiler's own
// edge checks say what such a node is, this one does not guess.
func nodesOnAPathToDone(nodes map[string]Node, edges []*Edge) map[string]bool {
	pred := map[string][]string{}
	outgoing := map[string]bool{}
	for _, e := range edges {
		pred[e.To] = append(pred[e.To], e.From)
		outgoing[e.From] = true
	}
	reach := map[string]bool{"done": true}
	stack := []string{"done"}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range pred[n] {
			if !reach[p] {
				reach[p] = true
				stack = append(stack, p)
			}
		}
	}
	for id := range nodes {
		if !outgoing[id] {
			reach[id] = true
		}
	}
	return reach
}

func (c *compiler) compilePublicContract(decl *ast.ContractDecl, vars map[string]*Var, succeeds map[string]bool) *PublicContract {
	pc := &PublicContract{Name: decl.Name, DisplayName: decl.DisplayName, Responsibility: decl.Responsibility, Version: 1}
	if decl.Version != nil {
		if *decl.Version < 1 {
			c.errorfAtSpan(DiagContractInput, decl.Span, "contract %q: version %d — a public contract version starts at 1", decl.Name, *decl.Version)
		}
		pc.Version = *decl.Version
	}
	ports := map[string]*PublicPort{}
	seen := map[string]bool{}
	for _, p := range decl.Inputs {
		if p == nil {
			continue
		}
		pp := c.compilePublicPort(decl.Name, "input", p, seen)
		c.bindInput(decl.Name, p, pp, vars)
		pc.Inputs = append(pc.Inputs, pp)
		ports["input."+pp.Name] = pp
	}
	seen = map[string]bool{}
	for _, p := range decl.Outputs {
		if p == nil {
			continue
		}
		pp := c.compilePublicPort(decl.Name, "output", p, seen)
		c.bindOutput(decl.Name, p, pp, succeeds)
		pc.Outputs = append(pc.Outputs, pp)
		ports["output."+pp.Name] = pp
	}
	seen = map[string]bool{}
	for _, k := range decl.Criteria {
		if k == nil {
			continue
		}
		if seen[k.Name] {
			c.errorfAtSpan(DiagContractCriterion, k.Span, "contract %q: criterion %q is declared twice", decl.Name, k.Name)
		}
		seen[k.Name] = true
		pc.Criteria = append(pc.Criteria, c.compilePublicCriterion(decl.Name, k, ports))
	}
	seen = map[string]bool{}
	for _, e := range decl.Effects {
		if e == nil {
			continue
		}
		if seen[e.Name] {
			c.errorfAtSpan(DiagContractInput, e.Span, "contract %q: effect %q is declared twice", decl.Name, e.Name)
		}
		seen[e.Name] = true
		pc.Effects = append(pc.Effects, &PublicEffect{Name: e.Name, Description: e.Description, Paid: e.Paid})
	}
	return pc
}

// sideCode is the code a port's own shape draws: an input's is C300, an
// output's C301 — the side the author fixes.
func sideCode(side string) DiagCode {
	if side == "input" {
		return DiagContractInput
	}
	return DiagContractOutput
}

// compilePublicPort reads what a port declares about itself — its name,
// type, requiredness, cardinality, default and file shape — and holds the
// declaration to what a port can be. The binding to the program is the
// caller's (bindInput, bindOutput).
func (c *compiler) compilePublicPort(contract, side string, p *ast.PortDecl, seen map[string]bool) *PublicPort {
	code := sideCode(side)
	if seen[p.Name] {
		c.errorfAtSpan(code, p.Span, "contract %q: %s %q is declared twice", contract, side, p.Name)
	}
	seen[p.Name] = true
	pp := &PublicPort{
		Name:        p.Name,
		Type:        p.Type,
		Description: p.Description,
		Required:    p.IsRequired(),
		Nullable:    p.Nullable,
		Default:     p.Default,
		MinItems:    p.MinItems,
		MaxItems:    p.MaxItems,
	}
	if p.FileSpec != nil {
		pp.File = &PublicFile{MediaType: p.FileSpec.MediaType, MinBytes: p.FileSpec.MinBytes, Schema: p.FileSpec.Schema}
		if p.FileSpec.Schema != "" {
			if _, ok := c.schemas[p.FileSpec.Schema]; !ok {
				c.errorfAtSpan(code, p.Span, "contract %q: %s %q: file schema %q is not a declared schema", contract, side, p.Name, p.FileSpec.Schema)
			}
		}
	}
	if p.Type == "" {
		c.errorfAtSpan(code, p.Span, "contract %q: %s %q has no type — a port is written `name: type`", contract, side, p.Name)
	}
	isArray := strings.HasSuffix(p.Type, "[]")
	if (p.MinItems != nil || p.MaxItems != nil) && !isArray {
		c.errorfAtSpan(code, p.Span, "contract %q: %s %q is %s, not an array — min_items and max_items bound an array (`%s[]`)", contract, side, p.Name, p.Type, p.Type)
	}
	if p.MinItems != nil && p.MaxItems != nil && *p.MinItems > *p.MaxItems {
		c.errorfAtSpan(code, p.Span, "contract %q: %s %q: min_items %d is above max_items %d", contract, side, p.Name, *p.MinItems, *p.MaxItems)
	}
	if p.Default != nil {
		if err := parser.WritableJSONValue(p.Default); err != nil {
			c.errorfAtSpan(DiagContractCriterion, p.Span, "contract %q: %s %q default: %v", contract, side, p.Name, err)
		} else if why := defaultFits(p.Default, p.Type, p.Nullable); why != "" {
			c.errorfAtSpan(DiagContractCriterion, p.Span, "contract %q: %s %q default %s", contract, side, p.Name, why)
		}
	}
	return pp
}

// defaultFits reports why a default does not fit the port's type — "" when
// it does. A builtin type takes its own JSON shape; `null` only on a
// nullable port; `json` takes anything. A schema name takes an object — a
// branch no declared port reaches today: an input is a var, and no var is
// schema-typed (C300); an output has no default (C301).
func defaultFits(raw json.RawMessage, portType string, nullable bool) string {
	value, err := spec.DecodePublicJSON(raw)
	if err != nil {
		return "is not one JSON value: " + err.Error()
	}
	if value == nil {
		if nullable {
			return ""
		}
		return "is null on a port that is not nullable: set `nullable: true`, or give a value"
	}
	base := strings.TrimSuffix(portType, "[]")
	if strings.HasSuffix(portType, "[]") {
		items, ok := value.([]any)
		if !ok {
			return fmt.Sprintf("%s is not a %s: write a list, `[...]`", string(raw), portType)
		}
		for i, item := range items {
			if why := scalarFits(item, base); why != "" {
				return fmt.Sprintf("item %d %s", i, why)
			}
		}
		return ""
	}
	return scalarFits(value, portType)
}

func scalarFits(value any, typ string) string {
	switch typ {
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Sprintf("%v is not a string: quote it", value)
		}
	case "int":
		n, ok := value.(json.Number)
		if !ok || strings.ContainsAny(string(n), ".eE") {
			return fmt.Sprintf("%v is not an int", value)
		}
	case "float":
		if _, ok := value.(json.Number); !ok {
			return fmt.Sprintf("%v is not a float", value)
		}
	case "bool":
		if _, ok := value.(bool); !ok {
			return fmt.Sprintf("%v is not a bool: write true or false", value)
		}
	case "json", "":
		return ""
	default: // a schema name: an object
		if _, ok := value.(map[string]any); !ok {
			return fmt.Sprintf("%v is not a %s: write an object, `{field: value}`", value, typ)
		}
	}
	return ""
}

// bindInput holds an input to the program: it is a declared var of the
// same type, its value comes from the launch (never a `from:`, never a
// file — a var carries none), and its requiredness and default are the
// var's: an input is required exactly when its var has no default, and
// defaults to what the var defaults to, read as the launch reads a value
// of that type (seededDefault). A port may repeat what the var says,
// never contradict it: a `required:` that disagrees, a `default:` the var
// does not carry or carries as another value, is refused. On a var
// without a default, a `nullable: true` port may be `required: false` —
// with no default, or `default: null` — because omitted, the run starts
// with the var unset, which is null. The var's enum is the domain the
// contract advertises.
func (c *compiler) bindInput(contract string, p *ast.PortDecl, pp *PublicPort, vars map[string]*Var) {
	if p.From != "" {
		c.errorfAtSpan(DiagContractInput, p.Span, "contract %q: input %q carries `from:` — an input is a declared var whose value comes from the launch; drop from:, or declare the port under outputs:", contract, p.Name)
	}
	if p.FileSpec != nil {
		c.errorfAtSpan(DiagContractInput, p.Span, "contract %q: input %q declares file: — an input is a var the launch carries, and a var carries no file; drop file: (a file the bot takes is an attachment)", contract, p.Name)
	}
	v, ok := vars[p.Name]
	if !ok {
		c.errorfAtSpan(DiagContractInput, p.Span, "contract %q: input %q is not a declared var — declare `%s: %s` under vars: (the contract describes this program)", contract, p.Name, p.Name, p.Type)
		return
	}
	if p.Type != "" && v.Type.String() != p.Type {
		c.errorfAtSpan(DiagContractInput, p.Span, "contract %q: input %q is %s, the var %s is %s — the port takes the var's type", contract, p.Name, p.Type, p.Name, v.Type.String())
	}
	pp.EnumValues = v.EnumValues
	switch {
	case v.HasDefault:
		pp.Required = false
		if p.Default == nil {
			pp.Default = varDefaultJSON(v)
		} else if !defaultAgrees(p.Default, v) {
			c.errorfAtSpan(DiagContractInput, p.Span, "contract %q: input %q defaults to %s, the var %s defaults to %s — the port mirrors the var's default", contract, p.Name, string(p.Default), p.Name, varDefaultJSON(v))
		}
		if p.Required != nil && *p.Required {
			c.errorfAtSpan(DiagContractInput, p.Span, "contract %q: input %q is required, the var %s has a default (%s) — the run proceeds without the input; drop `required:` (the port mirrors the var), or drop the var's default", contract, p.Name, p.Name, varDefaultJSON(v))
		}
	case p.Default != nil && !isJSONNull(p.Default):
		pp.Required = p.IsRequired()
		c.errorfAtSpan(DiagContractInput, p.Span, "contract %q: input %q declares a default the var %s does not carry — the run never seeds it; give the var the default (`%s: %s = …`), or drop default:", contract, p.Name, p.Name, p.Name, v.Type.String())
	case p.Default != nil:
		// A null default: on a nullable port (C302 otherwise), what an
		// omitted var is at the run — optional, by the default it declares.
		pp.Required = false
		if p.Required != nil && *p.Required {
			c.errorfAtSpan(DiagContractInput, p.Span, "contract %q: input %q is required and defaults to null — drop `required:` (a default makes it optional), or drop the default", contract, p.Name)
		}
	default:
		pp.Required = p.IsRequired()
		if !pp.Required && !p.Nullable {
			c.errorfAtSpan(DiagContractInput, p.Span, "contract %q: input %q is `required: false`, the var %s has no default and the port is not nullable — omitted, the run starts with the var unset, which is null; give the var a default (`%s: %s = …`), make the port `nullable: true`, or drop `required: false`", contract, p.Name, p.Name, p.Name, v.Type.String())
		}
	}
}

// isJSONNull reports a written default that is the one value null.
func isJSONNull(raw json.RawMessage) bool {
	value, err := spec.DecodePublicJSON(raw)
	return err == nil && value == nil
}

// seededDefault is a var's default as the launch reads a value of its
// type — a `string[]` or `json` var's text as a list or an object, the
// reading an override gets (CoerceVarValue); a scalar is typed already.
func seededDefault(v *Var) any {
	seeded, err := CoerceVarValue(v.Default, v.Type)
	if err != nil {
		return v.Default
	}
	return seeded
}

// varDefaultJSON is a var's default as the canonical JSON the public view
// carries: a number without an exponent, as the text writes one, a
// string, a bool, a list or an object.
func varDefaultJSON(v *Var) json.RawMessage {
	switch x := seededDefault(v).(type) {
	case float64:
		return json.RawMessage(strconv.FormatFloat(x, 'f', -1, 64))
	case int64:
		return json.RawMessage(strconv.FormatInt(x, 10))
	default:
		raw, err := json.Marshal(x)
		if err != nil {
			return nil
		}
		return raw
	}
}

// defaultAgrees reports whether a port's written default is the var's: the
// same string, integer, number or bool — a number the text writes two ways
// (`1.50`, `1.5`) is one value — or, for a list or an object, the same
// value under one reading of both sides.
func defaultAgrees(raw json.RawMessage, v *Var) bool {
	value, err := spec.DecodePublicJSON(raw)
	if err != nil {
		return false
	}
	switch want := seededDefault(v).(type) {
	case string:
		got, ok := value.(string)
		return ok && got == want
	case bool:
		got, ok := value.(bool)
		return ok && got == want
	case int64:
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		got, err := n.Int64()
		return err == nil && got == want
	case float64:
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		got, err := n.Float64()
		return err == nil && got == want
	}
	// A container: the port's value read as the var's is (seededDefault →
	// encoding/json, numbers as float64), so `{ratio: 1.0}` and the var's
	// `{"ratio": 1.0}` are one value whatever spelling each side used.
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return false
	}
	got, err := json.Marshal(normalized)
	return err == nil && string(got) == string(varDefaultJSON(v))
}

// producerOf resolves a `from:` against the program's nodes: the whole
// reference as a node id first (an instance of a group is `<prefix>.<node>`),
// then the longest node id before the last dot, the rest being the field.
func (c *compiler) producerOf(from string) (nodeID, field string, n Node, ok bool) {
	if n, ok := c.nodes[from]; ok {
		return from, "", n, true
	}
	if i := strings.LastIndex(from, "."); i > 0 {
		if n, ok := c.nodes[from[:i]]; ok {
			return from[:i], from[i+1:], n, true
		}
	}
	nodeID, field, _ = strings.Cut(from, ".")
	return nodeID, field, nil, false
}

// bindOutput holds an output to its producer: `from: <node>.<field>` names
// a declared node and a field of its output schema, of the port's type;
// `from: <node>` binds a file port to a node that publishes, or a port
// typed with the node's whole output schema. An output is produced, never
// defaulted; one whose producer is on no path to done is produced only
// when the bot fails (C304).
func (c *compiler) bindOutput(contract string, p *ast.PortDecl, pp *PublicPort, succeeds map[string]bool) {
	if p.Default != nil {
		c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q has a default — an output is produced by the program, never defaulted", contract, p.Name)
	}
	if p.From == "" {
		c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q names no producer — write `from: <node>.<field>` (a file port, or a port typed with a node's output schema: `from: <node>`)", contract, p.Name)
		return
	}
	nodeID, field, n, ok := c.producerOf(p.From)
	if !ok {
		c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q: from: names node %q, which the program does not declare (an instance of a group is named `<prefix>.<node>`)", contract, p.Name, nodeID)
		return
	}
	pp.FromNode, pp.FromField = nodeID, field
	if !succeeds[nodeID] {
		c.warnfAtSpan(DiagContractOutputOffSuccess, p.Span, "contract %q: output %q: node %q is on no path to done — the bot produces it only when it fails", contract, p.Name, nodeID)
	}
	if p.FileSpec != nil {
		if field != "" {
			c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q is a file port: it names its node alone, `from: %s`", contract, p.Name, nodeID)
		}
		if NodePublish(n) == "" {
			c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q: node %q publishes no file — a file port names an agent, judge, human, tool or compute node with `publish:`", contract, p.Name, nodeID)
		}
		return
	}
	schemaName := NodeOutputSchema(n)
	if schemaName == "" {
		if implicit := NodeImplicitOutputFields(n); len(implicit) > 0 {
			switch {
			case field == "":
				c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q: node %q has no output schema; name its field (`from: %s.%s`)", contract, p.Name, nodeID, nodeID, implicit[0])
			case !slices.Contains(implicit, field):
				c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q: node %q has no field %q (it has %s)", contract, p.Name, nodeID, field, strings.Join(implicit, ", "))
			case p.Type != "json":
				c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q is %s, %s.%s is json — the port takes json", contract, p.Name, p.Type, nodeID, field)
			}
			return
		}
		c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q: node %q has no output schema to produce it", contract, p.Name, nodeID)
		return
	}
	if field == "" {
		if p.Type != schemaName {
			c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q is %s but `from: %s` binds the node's whole output, a %s — name the field (`from: %s.<field>`), or type the port %s", contract, p.Name, p.Type, nodeID, schemaName, nodeID, schemaName)
		}
		return
	}
	schema, ok := c.schemas[schemaName]
	if !ok {
		return // the node's own unknown-schema diagnostic names it
	}
	var found *SchemaField
	for _, f := range schema.Fields {
		if f.Name == field {
			found = f
			break
		}
	}
	if found == nil {
		c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q: node %q's output %s has no field %q", contract, p.Name, nodeID, schemaName, field)
		return
	}
	if p.Type != "" && found.Type.String() != p.Type {
		c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q is %s, %s.%s is %s — the port takes the field's type", contract, p.Name, p.Type, nodeID, field, found.Type.String())
	}
}

// compilePublicCriterion holds a criterion to a port of the contract and
// to its evaluator: a registered kind takes the port's type — a file port is
// a file, whatever its declared type — and its parameters compile; an
// unregistered kind is declared, not evaluated.
func (c *compiler) compilePublicCriterion(contract string, k *ast.CriterionDecl, ports map[string]*PublicPort) *PublicCriterion {
	pk := &PublicCriterion{Name: k.Name, Kind: k.Kind, Port: k.Port, Params: k.Params}
	if k.Kind == "" {
		c.errorfAtSpan(DiagContractCriterion, k.Span, "contract %q: criterion %q names no kind — `kind: min_length`, `kind: pattern`", contract, k.Name)
	}
	var port *PublicPort
	if side, name, ok := strings.Cut(k.Port, "."); !ok || (side != "input" && side != "output") || name == "" {
		c.errorfAtSpan(DiagContractCriterion, k.Span, "contract %q: criterion %q: port %q — write `input.<name>` or `output.<name>`, singular", contract, k.Name, k.Port)
	} else if port = ports[k.Port]; port == nil {
		c.errorfAtSpan(DiagContractCriterion, k.Span, "contract %q: criterion %q: no port %s is declared", contract, k.Name, k.Port)
	}
	if err := parser.WritableJSONValue(k.Params); err != nil {
		c.errorfAtSpan(DiagContractCriterion, k.Span, "contract %q: criterion %q params: %v", contract, k.Name, err)
		return pk
	}
	if k.Kind == "" {
		return pk
	}
	ev, ok := spec.LookupPublicCriterion(k.Kind)
	if !ok {
		c.warnfAtSpan(DiagContractUnknownKind, k.Span, "contract %q: criterion %q: kind %q has no registered evaluator (this build ships %s) — declared, not evaluated", contract, k.Name, k.Kind, registeredKinds())
		return pk
	}
	pk.Registered = true
	if port != nil {
		class, is := "string", port.Type
		switch {
		case port.File != nil:
			class, is = "file", "a file"
		case strings.HasSuffix(port.Type, "[]"):
			class = "array"
		case port.Type != "string":
			class = port.Type
		}
		if !slices.Contains(ev.Types, class) {
			c.errorfAtSpan(DiagContractCriterion, k.Span, "contract %q: criterion %q: %s checks a %s, port %s is %s", contract, k.Name, k.Kind, strings.Join(ev.Types, " or "), k.Port, is)
		}
	}
	if _, err := spec.CompilePublicCriterion(k.Kind, k.Params); err != nil {
		c.errorfAtSpan(DiagContractCriterion, k.Span, "contract %q: criterion %q: %v", contract, k.Name, err)
	}
	return pk
}

func registeredKinds() string {
	names := make([]string, 0, len(spec.PublicCriteria))
	for _, ev := range spec.PublicCriteria {
		names = append(names, ev.Name)
	}
	return strings.Join(names, ", ")
}
