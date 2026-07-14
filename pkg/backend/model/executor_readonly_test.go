package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestExtractBackendFieldsCarriesReadonly(t *testing.T) {
	tests := []struct {
		name string
		node ir.Node
	}{
		{
			name: "agent",
			node: &ir.AgentNode{LLMFields: ir.LLMFields{Readonly: true}},
		},
		{
			name: "judge",
			node: &ir.JudgeNode{LLMFields: ir.LLMFields{Readonly: true}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields, err := extractBackendFields(tt.node)
			if err != nil {
				t.Fatalf("extractBackendFields: %v", err)
			}
			if !fields.readonly {
				t.Fatal("readonly flag was lost before Task construction")
			}
		})
	}
}
