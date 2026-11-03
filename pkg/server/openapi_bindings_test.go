package server

import (
	"strings"
	"testing"
)

// The bot-bindings surface is the one a studio client reads to drive the
// context kind (ADR-147): the typed record (kind, the pinned source, the
// degraded readout) and the POST's refusal vocabulary. The registry entries
// are one line each, so the guard that bites when one is dropped lives
// here — the generate-and-diff CI gate only sees a committed artifact.
func TestTheSpecCarriesTheBotBindingsSurface(t *testing.T) {
	doc, err := BuildOpenAPISpec()
	if err != nil {
		t.Fatal(err)
	}
	paths := doc["paths"].(map[string]any)
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)

	item, ok := paths["/api/teams/{id}/bots/{bot_id}/bindings"].(map[string]any)
	if !ok {
		t.Fatal("the bindings collection route is not in the spec")
	}
	post := item["post"].(map[string]any)
	responses := post["responses"].(map[string]any)
	created, ok := responses["201"].(map[string]any)
	if !ok {
		t.Fatalf("POST bindings has no typed 201: %+v", responses)
	}
	schema := created["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	if ref, _ := schema["$ref"].(string); ref != "#/components/schemas/BotSecretBinding" {
		t.Errorf("POST 201 $ref = %q, want BotSecretBinding", ref)
	}
	// The refusal a context binding can still meet on a deployment without
	// the materialiser: named by its stable code, the only field of a 5xx
	// body a client can act on.
	s501, ok := responses["501"].(map[string]any)
	if !ok || !strings.Contains(s501["description"].(string), "context_binding_not_materialised") {
		t.Errorf("the 501 does not name its stable code: %+v", responses["501"])
	}
	if _, ok := responses["409"].(map[string]any); !ok {
		t.Errorf("the name-conflict 409 is not documented: %+v", responses)
	}

	get := item["get"].(map[string]any)
	listSchema := get["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	items := listSchema["properties"].(map[string]any)["bindings"].(map[string]any)["items"].(map[string]any)
	if ref, _ := items["$ref"].(string); ref != "#/components/schemas/BotSecretBinding" {
		t.Errorf("GET 200 items $ref = %q, want BotSecretBinding", ref)
	}

	// The DELETE's success is a bodyless 204, and the spec says so: an entry
	// that only described it in prose would leave a generated client without
	// the status to expect.
	byID := paths["/api/teams/{id}/bots/{bot_id}/bindings/{binding_id}"].(map[string]any)
	del := byID["delete"].(map[string]any)
	noContent, ok := del["responses"].(map[string]any)["204"].(map[string]any)
	if !ok {
		t.Fatalf("DELETE bindings has no 204 response entry: %+v", del["responses"])
	}
	if _, hasContent := noContent["content"]; hasContent {
		t.Errorf("the 204 carries a content schema: %+v — a bodyless success must not pretend a body", noContent)
	}

	// The PATCH's request schema is the WRITABLE surface: no kind — the
	// handler ignores it (a binding's kind is immutable), and listing it
	// would teach a client a write that silently does nothing.
	patch := byID["patch"].(map[string]any)
	patchReq := patch["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	if ref, _ := patchReq["$ref"].(string); ref != "#/components/schemas/botBindingPatchReq" {
		t.Fatalf("PATCH requestBody $ref = %q, want botBindingPatchReq", ref)
	}
	patchProps := schemas["botBindingPatchReq"].(map[string]any)["properties"].(map[string]any)
	if _, exposed := patchProps["kind"]; exposed {
		t.Errorf("the PATCH schema exposes kind as writable — the route ignores it: %v", patchProps)
	}

	// The record the list serves: the kind discriminator, the context
	// payload, and the engine's readout — the fields a BindingsTab renders.
	binding := schemas["BotSecretBinding"].(map[string]any)
	props := binding["properties"].(map[string]any)
	for _, field := range []string{"kind", "context", "degraded_reason", "degraded_at", "secret_name_for_workflow", "allowed_hosts"} {
		if _, ok := props[field]; !ok {
			t.Errorf("BotSecretBinding misses %q — the studio surface cannot read it", field)
		}
	}
	ctxProp := props["context"].(map[string]any)
	if ref, _ := ctxProp["$ref"].(string); ref != "#/components/schemas/ContextSource" {
		t.Errorf("context $ref = %q, want ContextSource", ref)
	}
	source := schemas["ContextSource"].(map[string]any)
	required, _ := source["required"].([]string)
	if !containsAll(required, "git_url", "ref", "path") {
		t.Errorf("ContextSource required = %v, want git_url+ref+path (the pin is load-bearing)", required)
	}
}
