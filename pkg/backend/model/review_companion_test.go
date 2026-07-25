package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestReviewCompanionJSONSchemaIncludesStrictMediaRefs(t *testing.T) {
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

	media, _ := properties["media_refs"].(map[string]any)
	if media["type"] != "array" || media["maxItems"] != float64(12) {
		t.Fatalf("media_refs schema = %#v, want array maxItems=12", media)
	}
	items, _ := media["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Errorf("media_refs items must reject extra model-controlled fields: %#v", items)
	}
	itemProps, _ := items["properties"].(map[string]any)
	if _, ok := itemProps["path"]; !ok {
		t.Error("media_refs item missing path")
	}
	if _, ok := itemProps["caption"]; !ok {
		t.Error("media_refs item missing caption")
	}

	required, _ := schema["required"].([]any)
	wantRequired := map[string]bool{
		"decision": false, "needs_human_input": false, "message": false, "media_refs": false,
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
	for _, name := range []string{"needs_human_input", "message", "media_refs"} {
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
