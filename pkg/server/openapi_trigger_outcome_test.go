package server

import "testing"

// The trigger read routes are typed, and the type they publish carries the
// outcome of the run the subscription launched (#1501). The spec is the only
// place a generated client learns that `last_run_status` exists: untyped, the
// route's payload is a free-form object and a subscription that fired and
// whose run died is indistinguishable, to every client built from this spec,
// from one that fired and worked.
//
// The `last_error` assertion is deliberate beside them: the launch verdict
// and the run outcome are different records of different moments, and a spec
// that published one without the other would re-create the ambiguity the
// ticket names.
func TestTheSpecTypesTheTriggerSubscriptionOutcome(t *testing.T) {
	doc, err := BuildOpenAPISpec()
	if err != nil {
		t.Fatal(err)
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatal("spec has no paths")
	}
	list, ok := paths["/api/v1/triggers"].(map[string]any)
	if !ok {
		t.Fatal("GET /api/v1/triggers is not in the spec")
	}
	get, ok := list["get"].(map[string]any)
	if !ok {
		t.Fatalf("the triggers list has no get operation: %+v", list)
	}
	resp, ok := get["responses"].(map[string]any)["200"].(map[string]any)
	if !ok {
		t.Fatalf("the triggers list has no typed 200 response: %+v", get["responses"])
	}
	schema := resp["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	items, ok := schema["properties"].(map[string]any)["subscriptions"].(map[string]any)["items"].(map[string]any)
	if !ok {
		t.Fatalf("the 200 schema does not carry a subscriptions array: %+v", schema)
	}
	if ref, _ := items["$ref"].(string); ref != "#/components/schemas/Subscription" {
		t.Errorf("subscriptions items $ref = %q, want the Subscription schema", ref)
	}

	// The single-subscription read publishes the same type.
	one, ok := paths["/api/v1/triggers/{id}"].(map[string]any)["get"].(map[string]any)
	if !ok {
		t.Fatal("GET /api/v1/triggers/{id} is not in the spec")
	}
	oneSchema := one["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	if ref, _ := oneSchema["$ref"].(string); ref != "#/components/schemas/Subscription" {
		t.Errorf("GET /api/v1/triggers/{id} 200 $ref = %q, want the Subscription schema", ref)
	}

	schemas, ok := doc["components"].(map[string]any)["schemas"].(map[string]any)
	if !ok {
		t.Fatal("spec has no components.schemas")
	}
	sub, ok := schemas["Subscription"].(map[string]any)
	if !ok {
		t.Fatalf("components.schemas has no Subscription (schemas: %d)", len(schemas))
	}
	props, ok := sub["properties"].(map[string]any)
	if !ok {
		t.Fatalf("the Subscription schema has no properties: %+v", sub)
	}
	for _, want := range []string{
		"last_run_id", "last_run_status", "last_run_error", "last_run_error_code", "last_run_at",
		"last_error", "last_error_at",
	} {
		if _, ok := props[want]; !ok {
			t.Errorf("the published Subscription schema hides %q — a client generated from this spec cannot read the run's outcome", want)
		}
	}
}
