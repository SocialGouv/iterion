package openaiwire

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// Candidate witness: a RECEIVED schema in the shapes OpenAI refuses still
// leaves NormalizedParameters as a root object with type "object", an object
// "properties" and no null "required".
func TestWitnessDecodedSchemaKeepsOpenAIRootDefaults(t *testing.T) {
	for _, in := range []string{
		`{}`,
		`{"properties":{}}`,
		`{"type":"object","properties":null}`,
		`{"type":"object","properties":{"q":{"type":"string"}},"required":null}`,
		`null`,
	} {
		var s api.InputSchema
		if err := json.Unmarshal([]byte(in), &s); err != nil {
			t.Fatalf("%s: decode: %v", in, err)
		}
		p, err := NormalizedParameters(s)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		var root map[string]json.RawMessage
		if err := json.Unmarshal(p, &root); err != nil || root == nil {
			t.Errorf("%s: parameters is not an object: %s", in, p)
			continue
		}
		if string(root["type"]) != `"object"` {
			t.Errorf("%s: root type = %s, want \"object\" (wire %s)", in, root["type"], p)
		}
		if props := root["properties"]; len(props) == 0 || props[0] != '{' {
			t.Errorf("%s: properties = %s, want an object (wire %s)", in, props, p)
		}
		if req, ok := root["required"]; ok && string(req) == "null" {
			t.Errorf("%s: required is null (wire %s)", in, p)
		}
	}
}

func TestNormalizedParametersEmptyRaw(t *testing.T) {
	got, err := NormalizedParameters(api.InputSchema{Type: "object", Raw: json.RawMessage{}})
	if err != nil || len(got) == 0 {
		t.Fatalf("empty Raw must use typed defaults: %s, %v", got, err)
	}
}
