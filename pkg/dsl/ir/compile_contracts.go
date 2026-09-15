package ir

import (
	"encoding/json"
	"fmt"
	"slices"
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
	DiagContractInput       DiagCode = "C300" // an input the program does not keep (not a declared var, another type, a `from:`, a default while required), a contract declared twice, a version below 1, a workflow naming no declared contract
	DiagContractOutput      DiagCode = "C301" // an output the program does not produce: no `from:`, an unknown node or field, another type, a default
	DiagContractCriterion   DiagCode = "C302" // a criterion or a value the contract cannot hold: an unknown port, a type the evaluator does not take, invalid parameters, a default the text cannot write or of another type
	DiagContractUnknownKind DiagCode = "C303" // warning: a criterion's kind has no registered evaluator — declared, not evaluated
)

// compilePublicContracts binds every contract of the unit to the program —
// its vars, nodes and schemas are compiled by now — and returns them by
// name with the one the workflow keeps. Every contract is held, named by
// the workflow or not: a contract describes THIS program.
func (c *compiler) compilePublicContracts(wf *ast.WorkflowDecl, vars map[string]*Var) (map[string]*PublicContract, *PublicContract) {
	contracts := map[string]*PublicContract{}
	for _, decl := range c.file.Contracts {
		if decl == nil {
			continue
		}
		if _, dup := contracts[decl.Name]; dup {
			c.errorfAtSpan(DiagContractInput, decl.Span, "contract %q is declared twice: a name declares one contract", decl.Name)
			continue
		}
		contracts[decl.Name] = c.compilePublicContract(decl, vars)
	}
	if wf.Contract == "" {
		return contracts, nil
	}
	bound, ok := contracts[wf.Contract]
	if !ok {
		c.errorf(DiagContractInput, "workflow %q names contract %q, which no `contract %s:` declares", wf.Name, wf.Contract, wf.Contract)
		return contracts, nil
	}
	return contracts, bound
}

func (c *compiler) compilePublicContract(decl *ast.ContractDecl, vars map[string]*Var) *PublicContract {
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
		c.bindOutput(decl.Name, p, pp)
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
// it does. A builtin type takes its own JSON shape; a schema-typed port
// takes an object; `null` only on a nullable port; `json` takes anything.
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
// same type, its value comes from the launch (never a `from:`), and a
// default makes it optional.
func (c *compiler) bindInput(contract string, p *ast.PortDecl, pp *PublicPort, vars map[string]*Var) {
	if p.From != "" {
		c.errorfAtSpan(DiagContractInput, p.Span, "contract %q: input %q carries `from:` — an input is a declared var whose value comes from the launch; drop from:, or declare the port under outputs:", contract, p.Name)
	}
	v, ok := vars[p.Name]
	if !ok {
		c.errorfAtSpan(DiagContractInput, p.Span, "contract %q: input %q is not a declared var — declare `%s: %s` under vars: (the contract describes this program)", contract, p.Name, p.Name, p.Type)
	} else if p.Type != "" && v.Type.String() != p.Type {
		c.errorfAtSpan(DiagContractInput, p.Span, "contract %q: input %q is %s, the var %s is %s — the port takes the var's type", contract, p.Name, p.Type, p.Name, v.Type.String())
	}
	if p.Default != nil && pp.Required {
		c.errorfAtSpan(DiagContractInput, p.Span, "contract %q: input %q has a default and is required — set `required: false`, or drop the default", contract, p.Name)
	}
}

// bindOutput holds an output to its producer: `from: <node>.<field>` names
// a declared node and a field of its output schema, of the port's type;
// `from: <node>` binds a file port, or a port typed with the node's whole
// output schema. An output is produced, never defaulted.
func (c *compiler) bindOutput(contract string, p *ast.PortDecl, pp *PublicPort) {
	if p.Default != nil {
		c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q has a default — an output is produced by the program, never defaulted", contract, p.Name)
	}
	if p.From == "" {
		c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q names no producer — write `from: <node>.<field>` (a file port, or a port typed with a node's output schema: `from: <node>`)", contract, p.Name)
		return
	}
	nodeID, field, _ := strings.Cut(p.From, ".")
	n, ok := c.nodes[nodeID]
	if !ok {
		c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q: from: names node %q, which the program does not declare", contract, p.Name, nodeID)
		return
	}
	pp.FromNode, pp.FromField = nodeID, field
	if p.FileSpec != nil {
		if field != "" {
			c.errorfAtSpan(DiagContractOutput, p.Span, "contract %q: output %q is a file port: it names its node alone, `from: %s`", contract, p.Name, nodeID)
		}
		return
	}
	schemaName := NodeOutputSchema(n)
	if schemaName == "" {
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
// to its evaluator: a registered kind takes the port's type and its
// parameters compile; an unregistered kind is declared, not evaluated.
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
		class := "string"
		switch {
		case strings.HasSuffix(port.Type, "[]"):
			class = "array"
		case port.Type != "string":
			class = port.Type
		}
		if !slices.Contains(ev.Types, class) {
			c.errorfAtSpan(DiagContractCriterion, k.Span, "contract %q: criterion %q: %s checks a %s, port %s is %s", contract, k.Name, k.Kind, strings.Join(ev.Types, " or "), k.Port, port.Type)
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
