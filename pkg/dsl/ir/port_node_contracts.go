package ir

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

func projectedPortFieldType(port PublicPort) FieldType {
	if port.Nullable {
		return FieldTypeJSON
	}
	if port.Type.ArrayDepth != 0 {
		if port.Type.ArrayDepth == 1 && port.Type.Name == "string" && port.Type.Schema == nil {
			return FieldTypeStringArray
		}
		return FieldTypeJSON
	}
	switch port.Type.Name {
	case "string":
		return FieldTypeString
	case "bool":
		return FieldTypeBool
	case "int":
		return FieldTypeInt
	case "float":
		return FieldTypeFloat
	default:
		// A file port is a verified produced artifact, distinct from the
		// legacy operator-upload field which is restricted to human nodes.
		return FieldTypeJSON
	}
}

func (c *compiler) attachPublicNodeSchemas(w *Workflow, node Node, contract *PublicContract, span ast.Span) {
	compile := func(direction, legacy string, ports []PublicPort) string {
		if legacy != "" {
			schema := w.Schemas[legacy]
			if schema != nil {
				for _, field := range schema.Fields {
					port := FindPublicPort(ports, field.Name)
					if port == nil || !port.Required || field.Type != projectedPortFieldType(*port) || len(field.EnumValues) != 0 {
						c.contractError(DiagPortImplementation, span,
							"implementation %q %s schema %q field %q is not guaranteed by its public contract; align or remove the legacy schema declaration",
							node.NodeID(), direction, legacy, field.Name)
					}
				}
			}
		}
		name := "\x00ports/" + node.NodeID() + "/" + direction
		if w.Schemas[name] != nil {
			c.contractError(DiagPortImplementation, span, "schema name %q is reserved for the native public projection", name)
		}
		schema := &Schema{Name: name, NativePorts: true, PublicPorts: append([]PublicPort(nil), ports...)}
		for _, port := range ports {
			schema.Fields = append(schema.Fields, &SchemaField{Name: port.Name, Type: projectedPortFieldType(port)})
		}
		w.Schemas[name] = schema
		return name
	}
	var fields *SchemaFields
	switch n := node.(type) {
	case LLMNode:
		fields = n.GetSchemaFields()
	case *ToolNode:
		fields = &n.SchemaFields
	case *ComputeNode:
		fields = &n.SchemaFields
	case *WaitNode:
		fields = &n.SchemaFields
	case *SubbotNode:
		n.OutputSchema = compile("output", n.OutputSchema, contract.Outputs)
		return
	default:
		return
	}
	fields.InputSchema = compile("input", fields.InputSchema, contract.Inputs)
	fields.OutputSchema = compile("output", fields.OutputSchema, contract.Outputs)
}

func (c *compiler) validateNativePublicRefs(w *Workflow) {
	if w.Ports == nil {
		return
	}
	promptSpans := map[string]ast.Span{}
	for _, prompt := range c.file.Prompts {
		promptSpans[prompt.Name] = prompt.Span
	}
	refs := collectAllRefs(w, promptSpans, c.edgeSpans)
	// Subbot and event mappings consume data too. The legacy template walker
	// predates these public contracts; include their mappings explicitly.
	for id, node := range w.Nodes {
		var mappings []*DataMapping
		switch n := node.(type) {
		case *SubbotNode:
			mappings = n.With
		case *EmitNode:
			mappings = n.With
		}
		for _, mapping := range mappings {
			for _, ref := range mapping.Refs {
				refs = append(refs, refContext{Ref: ref, NodeID: id, Location: fmt.Sprintf("node %q mapping %q", id, mapping.Key)})
			}
		}
	}
	for _, rc := range refs {
		instance := w.Ports.Nodes[rc.NodeID]
		if instance == nil {
			continue
		}
		switch rc.Ref.Kind {
		case RefInput:
			if len(rc.Ref.Path) > 0 && FindPublicPort(instance.Contract.Inputs, rc.Ref.Path[0]) == nil {
				c.refErrorf(rc, DiagPortHiddenInput, "%s reads undeclared public input %q", rc.Location, rc.Ref.Path[0])
			}
		case RefOutputs, RefArtifacts, RefAttachments:
			c.refErrorf(rc, DiagPortHiddenInput, "%s reads %s outside its public inputs; bind a named port instead", rc.Location, rc.Ref.Raw)
		}
	}
}
