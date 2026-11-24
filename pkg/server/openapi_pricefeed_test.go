package server

import "testing"

// The price-feed PUT bodies carry `feeds` as a one-of(list, null) the
// handler decodes from a json.RawMessage — a type the generator renders
// as a base64 string. A typed request schema would teach every generated
// client to send base64, so the three PUTs (platform, org, team) declare
// none and describe the body in prose, the routing-policy precedent.
func TestThePriceFeedPutsDeclareNoBase64Body(t *testing.T) {
	doc, err := BuildOpenAPISpec()
	if err != nil {
		t.Fatal(err)
	}
	paths := doc["paths"].(map[string]any)
	for _, path := range []string{"/api/admin/price-feeds", "/api/orgs/{id}/price-feeds", "/api/teams/{id}/price-feeds"} {
		item, ok := paths[path].(map[string]any)
		if !ok {
			t.Fatalf("%s is not in the spec", path)
		}
		put, ok := item["put"].(map[string]any)
		if !ok {
			t.Fatalf("PUT %s is not in the spec", path)
		}
		if body, ok := put["requestBody"]; ok {
			t.Errorf("PUT %s declares a request body %v — its feeds field renders as base64", path, body)
		}
		if desc, _ := put["description"].(string); desc == "" {
			t.Errorf("PUT %s declares no description of its body", path)
		}
	}
}
