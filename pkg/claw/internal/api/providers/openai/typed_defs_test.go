package openai

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func TestTypedDefinitionsOnBothWires(t *testing.T) {
	ref := "#/$defs/Node"
	literal := api.InputSchema{Type: "object", Properties: map[string]api.Property{"root": {Ref: &ref}}, Defs: map[string]api.Property{"Node": {Type: "object", Properties: map[string]api.Property{"child": {Ref: &ref}}}}}
	var decoded api.InputSchema
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"root":{"$ref":"#/$defs/Node"}},"$defs":{"Node":{"type":"object","properties":{"child":{"$ref":"#/$defs/Node"}}}}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	decoded.Raw = nil
	for name, s := range map[string]api.InputSchema{"literal": literal, "decoded-cleared": decoded} {
		for _, wire := range []string{api.OpenAIWireChat, api.OpenAIWireResponses} {
			t.Run(name+"/"+wire, func(t *testing.T) {
				body := captureSchemaRequest(t, wire, api.CreateMessageRequest{Tools: []api.Tool{{Name: "tree", InputSchema: s}}})
				var envelope map[string]any
				if err := json.Unmarshal(body, &envelope); err != nil {
					t.Fatal(err)
				}
				tool := envelope["tools"].([]any)[0].(map[string]any)
				if wire == api.OpenAIWireChat {
					tool = tool["function"].(map[string]any)
				}
				params := tool["parameters"].(map[string]any)
				defs, ok := params["$defs"].(map[string]any)
				if !ok || defs["Node"] == nil {
					t.Fatalf("typed definitions lost: %s", body)
				}
				child := defs["Node"].(map[string]any)["properties"].(map[string]any)["child"].(map[string]any)
				root := params["properties"].(map[string]any)["root"].(map[string]any)
				if child["$ref"] != ref || root["$ref"] != ref {
					t.Fatalf("typed references lost: %s", body)
				}
			})
		}
	}
}
