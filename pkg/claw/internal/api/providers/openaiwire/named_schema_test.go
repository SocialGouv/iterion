package openaiwire

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func TestNormalizedParametersPreservesNamedDefinitions(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/named_schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema api.InputSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	encoded, err := NormalizedParameters(schema)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("named schema changed through NormalizedParameters: got %s; want %s", encoded, raw)
	}
}
