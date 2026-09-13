package mongo

import (
	"strings"
	"testing"
)

func TestPortBackendIdentityBindsEndpointButNotCredentials(t *testing.T) {
	first, err := portBackendIdentity("mongodb://server:secret@mongo.example:27017/?replicaSet=rs0", "iterion")
	if err != nil {
		t.Fatal(err)
	}
	second, err := portBackendIdentity("mongodb://runner:other@mongo.example:27017/?replicaSet=rs0", "iterion")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || strings.Contains(first, "secret") || strings.Contains(first, "runner") {
		t.Fatalf("backend identity leaked or bound credentials: %q / %q", first, second)
	}
	otherDB, _ := portBackendIdentity("mongodb://mongo.example:27017/?replicaSet=rs0", "other")
	otherEndpoint, _ := portBackendIdentity("mongodb://mongo-other.example:27017/?replicaSet=rs0", "iterion")
	if first == otherDB || first == otherEndpoint {
		t.Fatal("proof would survive backend/database change")
	}
}
