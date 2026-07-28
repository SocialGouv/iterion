package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestReviewCompanionJSONSchemaBoundsHumanMessage(t *testing.T) {
	raw, err := reviewCompanionJSONSchema(&ir.Schema{
		Name: "verdict",
		Fields: []*ir.SchemaField{
			{Name: "decision", Type: ir.FieldTypeString},
		},
	})
	if err != nil {
		t.Fatalf("reviewCompanionJSONSchema: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	properties, _ := schema["properties"].(map[string]any)
	decision, _ := properties["decision"].(map[string]any)
	if decision["type"] != "string" {
		t.Fatalf("base verdict field was not preserved: %#v", decision)
	}
	needsHuman, _ := properties["needs_human_input"].(map[string]any)
	if needsHuman["type"] != "boolean" {
		t.Fatalf("needs_human_input schema = %#v, want boolean", needsHuman)
	}
	message, _ := properties["message"].(map[string]any)
	if message["type"] != "string" ||
		message["maxLength"] != float64(maxReviewCompanionMessageChars) {
		t.Fatalf("message schema = %#v, want bounded string", message)
	}
	description, _ := message["description"].(string)
	for _, required := range []string{
		"Start with the action",
		"at most three short checks",
		"120 words",
		"file paths",
		"raw diff",
	} {
		if !strings.Contains(description, required) {
			t.Errorf("message description is missing %q: %q", required, description)
		}
	}

	if _, exists := properties["media_refs"]; exists {
		t.Error("media_refs must not be requested without the removed runtime media pipeline")
	}
	if schema["additionalProperties"] != false {
		t.Errorf("additionalProperties = %#v, want false", schema["additionalProperties"])
	}

	required, _ := schema["required"].([]any)
	wantRequired := map[string]bool{
		"decision": false, "needs_human_input": false, "message": false,
	}
	for _, value := range required {
		if key, ok := value.(string); ok {
			if _, wanted := wantRequired[key]; wanted {
				wantRequired[key] = true
			}
		}
	}
	for key, found := range wantRequired {
		if !found {
			t.Errorf("required is missing %q: %#v", key, required)
		}
	}
}

func TestReviewCompanionJSONSchemaRejectsReservedFieldCollision(t *testing.T) {
	for _, name := range []string{"needs_human_input", "message"} {
		t.Run(name, func(t *testing.T) {
			_, err := reviewCompanionJSONSchema(&ir.Schema{
				Name: "bad",
				Fields: []*ir.SchemaField{
					{Name: name, Type: ir.FieldTypeJSON},
				},
			})
			if err == nil {
				t.Fatalf("expected reserved %s collision to fail", name)
			}
		})
	}
}
