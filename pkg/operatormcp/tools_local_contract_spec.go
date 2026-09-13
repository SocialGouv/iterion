package operatormcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// contractSpecKind is a small, stable JSON projection of the same registry
// used by the parser diagnostics and generated DSL documentation.
type contractSpecKind struct {
	Name        string                 `json:"name"`
	Role        spec.Role              `json:"role"`
	Opener      string                 `json:"opener,omitempty"`
	Hosts       []string               `json:"hosts,omitempty"`
	Header      string                 `json:"header,omitempty"`
	Description string                 `json:"description"`
	Properties  []contractSpecProperty `json:"properties,omitempty"`
	Entries     *contractSpecEntries   `json:"entries,omitempty"`
}

type contractSpecEntries struct {
	Shape       string `json:"shape"`
	Description string `json:"description"`
	Body        string `json:"body,omitempty"`
}

type contractSpecProperty struct {
	Name        string    `json:"name"`
	Form        spec.Form `json:"form"`
	Values      []string  `json:"values,omitempty"`
	Body        string    `json:"body,omitempty"`
	Description string    `json:"description"`
	Until       int       `json:"until,omitempty"`
}

func publicContractSpecKind(kind spec.Kind) contractSpecKind {
	result := contractSpecKind{Name: kind.Name, Role: kind.Role, Opener: kind.Opener, Hosts: kind.Hosts,
		Header: kind.Header, Description: kind.Doc}
	if kind.Entries != nil {
		result.Entries = &contractSpecEntries{Shape: kind.Entries.Shape, Description: kind.Entries.Doc, Body: kind.Entries.Body}
	}
	for _, property := range kind.Properties {
		// The default workflow slice is its public interface. A caller asking
		// explicitly for kind=workflow receives the complete registry kind.
		if kind.Name == "workflow" && property.Name != "runtime_semantics" && property.Name != "contract" && property.Name != "graph" {
			continue
		}
		result.Properties = append(result.Properties, contractSpecProperty{Name: property.Name, Form: property.Form,
			Values: property.Values, Body: property.Body, Description: property.Doc, Until: property.Until})
	}
	return result
}

func fullContractSpecKind(kind spec.Kind) contractSpecKind {
	result := publicContractSpecKind(kind)
	if kind.Name == "workflow" {
		result.Properties = nil
		for _, property := range kind.Properties {
			result.Properties = append(result.Properties, contractSpecProperty{Name: property.Name, Form: property.Form,
				Values: property.Values, Body: property.Body, Description: property.Doc, Until: property.Until})
		}
	}
	return result
}

func handleLocalContractSpec(_ context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		Kind string `json:"kind"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	response := struct {
		RuntimeSemantics string                     `json:"runtime_semantics"`
		Kinds            []contractSpecKind         `json:"kinds"`
		Criteria         []spec.PublicCriterionSpec `json:"criteria,omitempty"`
	}{RuntimeSemantics: ir.RuntimeSemanticsPortsV1}
	if args.Kind != "" {
		kind, ok := spec.Lookup(args.Kind)
		if !ok {
			return "", false, fmt.Errorf("unknown DSL kind %q", args.Kind)
		}
		response.Kinds = []contractSpecKind{fullContractSpecKind(kind)}
	} else {
		for _, kind := range spec.Kinds {
			if kind.Name == "workflow" || kind.Name == "contract" || strings.HasPrefix(kind.Name, "contract.") || kind.Name == "graph" || strings.HasPrefix(kind.Name, "graph.") {
				response.Kinds = append(response.Kinds, publicContractSpecKind(kind))
			}
		}
		response.Criteria = spec.PublicCriteria
	}
	encoded, err := json.Marshal(response)
	return string(encoded), false, err
}
