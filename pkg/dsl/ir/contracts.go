package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// RuntimeSemanticsPortsV1 is an execution identity, independently versioned
// from text syntax, queue envelopes and durable storage schemas.
const RuntimeSemanticsPortsV1 = "ports-v1"

// PortSource locates a declaration in a captured source unit. Editor-created
// declarations have no source position until the document is written.
type PortSource struct {
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
}

// PublicContract is the resolved public view shared by nodes and workflows.
// Its identity is not an implementation identity: source snapshots and
// technical policies must additionally match before completed work is reused.
type PublicContract struct {
	Name           string            `json:"name"`
	DisplayName    string            `json:"display_name"`
	Responsibility string            `json:"responsibility"`
	Version        int               `json:"version"`
	Inputs         []PublicPort      `json:"inputs"`
	Outputs        []PublicPort      `json:"outputs"`
	Criteria       []PublicCriterion `json:"criteria,omitempty"`
	Effects        []PublicEffect    `json:"effects,omitempty"`
	Identity       string            `json:"identity"`
	Source         PortSource        `json:"source,omitempty"`
}

type PublicPort struct {
	Name        string          `json:"name"`
	Type        PortType        `json:"type"`
	Description string          `json:"description,omitempty"`
	Required    bool            `json:"required"`
	Nullable    bool            `json:"nullable,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	MinItems    *int            `json:"min_items,omitempty"`
	MaxItems    *int            `json:"max_items,omitempty"`
	File        *PortFile       `json:"file,omitempty"`
	Source      PortSource      `json:"source,omitempty"`
}

// PortType retains both the readable reference and the resolved shape.
// Equivalent compares shape and array depth; a matching schema name alone
// never proves compatibility. Arrays are lifted exactly one level by a map.
type PortType struct {
	Name       string  `json:"name"`
	ArrayDepth int     `json:"array_depth,omitempty"`
	ShapeHash  string  `json:"shape_hash"`
	Schema     *Schema `json:"schema,omitempty"`
}

func (t PortType) String() string {
	if t.ArrayDepth < 0 {
		return "<invalid array depth>"
	}
	return t.Name + strings.Repeat("[]", t.ArrayDepth)
}

func (t PortType) Equivalent(other PortType) bool {
	return t.ShapeHash != "" && t.ShapeHash == other.ShapeHash && t.ArrayDepth == other.ArrayDepth
}

func (t PortType) Array() PortType {
	t.ArrayDepth++
	return t
}

// Element reports whether this type is an array and, if so, its element type.
func (t PortType) Element() (PortType, bool) {
	if t.ArrayDepth <= 0 {
		return PortType{}, false
	}
	t.ArrayDepth--
	return t, true
}

type PortFile struct {
	MediaType string  `json:"media_type,omitempty"`
	MinBytes  int64   `json:"min_bytes,omitempty"`
	Schema    *Schema `json:"schema,omitempty"`
	ShapeHash string  `json:"shape_hash,omitempty"`
}

type PublicCriterion struct {
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Port   string          `json:"port"`
	Params json.RawMessage `json:"params,omitempty"`
	Source PortSource      `json:"source,omitempty"`
}

type PublicEffect struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Paid        bool       `json:"paid"`
	Source      PortSource `json:"source,omitempty"`
}

type PortPolicy struct {
	Name        string             `json:"name,omitempty"`
	MaxMapItems int                `json:"max_map_items,omitempty"`
	Effects     []PortEffectPolicy `json:"effects,omitempty"`
	Identity    string             `json:"identity"`
}

type PortEffectPolicy struct {
	Name     string `json:"name"`
	Resource string `json:"resource,omitempty"`
	Recovery string `json:"recovery"`
	Verifier string `json:"verifier,omitempty"`
}

const WorkflowInputPortNode = "input"

type PortEndpoint struct {
	Node string `json:"node"`
	Port string `json:"port"`
}

func (e PortEndpoint) String() string { return e.Node + "." + e.Port }

type PortBinding struct {
	From   PortEndpoint `json:"from"`
	To     PortEndpoint `json:"to"`
	Map    bool         `json:"map,omitempty"`
	Source PortSource   `json:"source,omitempty"`
}

// PortGraph is a data dependency graph. Order is a stable topological order
// for inspection and tie-breaking; it does not impose sequential execution.
// The runtime admits any ready instance under shared root limits.
type PortGraph struct {
	Nodes    map[string]*PortInstance `json:"nodes"`
	Order    []string                 `json:"order"`
	Bindings []PortBinding            `json:"bindings"`
	Exports  map[string]PortEndpoint  `json:"exports"`
	Products []string                 `json:"products"`
	Identity string                   `json:"identity"`
}

type PortInstance struct {
	ID             string                  `json:"id"`
	Implementation string                  `json:"implementation"`
	Contract       *PublicContract         `json:"contract"`
	Policy         *PortPolicy             `json:"policy"`
	Inputs         map[string]PortEndpoint `json:"inputs"`
	Dependencies   []string                `json:"dependencies"`
	MapInput       string                  `json:"map_input,omitempty"`
	MapInputs      []string                `json:"map_inputs,omitempty"`
	OutputTypes    map[string]PortType     `json:"output_types"`
	Source         PortSource              `json:"source,omitempty"`
}

func ParsePortEndpoint(ref string) (PortEndpoint, error) {
	node, port, ok := strings.Cut(ref, ".")
	if !ok || !validPublicName(node) || !validPublicName(port) {
		return PortEndpoint{}, fmt.Errorf("invalid port endpoint %q: expected node.port or input.port", ref)
	}
	return PortEndpoint{Node: node, Port: port}, nil
}

func FindPublicPort(ports []PublicPort, name string) *PublicPort {
	for i := range ports {
		if ports[i].Name == name {
			return &ports[i]
		}
	}
	return nil
}

// CanonicalPortSchemaFingerprint binds a public type to its resolved shape.
// A structured encoding keeps separators inside field names or enum values
// unambiguous. The existing legacy artifact digest is intentionally unchanged.
func CanonicalPortSchemaFingerprint(schema *Schema) string {
	if schema == nil {
		return ""
	}
	type shapeField struct {
		Name string   `json:"name"`
		Type string   `json:"type"`
		Enum []string `json:"enum,omitempty"`
	}
	fields := make([]shapeField, 0, len(schema.Fields))
	for _, f := range schema.Fields {
		if f == nil {
			continue
		}
		enum := append([]string(nil), f.EnumValues...)
		sort.Strings(enum)
		unique := enum[:0]
		for _, value := range enum {
			if len(unique) == 0 || unique[len(unique)-1] != value {
				unique = append(unique, value)
			}
		}
		fields = append(fields, shapeField{Name: f.Name, Type: f.Type.String(), Enum: unique})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	encoded, err := json.Marshal(fields)
	if err != nil {
		return "" // only concrete strings/slices are encoded
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// ResolvePortType resolves a name once, at compilation. Runtime scheduling
// never asks a model to guess a type or coerce one port into another.
func ResolvePortType(ref string, schemas map[string]*Schema) (PortType, error) {
	t := PortType{Name: ref}
	for strings.HasSuffix(t.Name, "[]") {
		t.Name = strings.TrimSuffix(t.Name, "[]")
		t.ArrayDepth++
	}
	switch t.Name {
	case "string", "bool", "int", "float", "json", "file":
		t.ShapeHash = "builtin:" + t.Name
		return t, nil
	}
	schema := schemas[t.Name]
	if schema == nil {
		return PortType{}, fmt.Errorf("unresolved port type %q", ref)
	}
	// Detach the resolved definition from caller-owned compilation inputs.
	t.Schema = &Schema{Name: schema.Name, Fields: make([]*SchemaField, 0, len(schema.Fields))}
	seen := map[string]bool{}
	for _, f := range schema.Fields {
		if f == nil {
			return PortType{}, fmt.Errorf("schema %q contains a missing field", t.Name)
		}
		if f.Name == "" || seen[f.Name] {
			return PortType{}, fmt.Errorf("schema %q has an unnamed or duplicate field %q", t.Name, f.Name)
		}
		if f.Type.String() == "unknown" {
			return PortType{}, fmt.Errorf("schema %q field %q has an unsupported type", t.Name, f.Name)
		}
		seen[f.Name] = true
		field := *f
		field.EnumValues = append([]string(nil), f.EnumValues...)
		t.Schema.Fields = append(t.Schema.Fields, &field)
	}
	t.ShapeHash = "ports-schema-v1:" + CanonicalPortSchemaFingerprint(t.Schema)
	return t, nil
}
