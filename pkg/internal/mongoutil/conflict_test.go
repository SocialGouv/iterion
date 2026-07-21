package mongoutil

import (
	"errors"
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// The regression: dropping a retired index during EnsureSchema returned
// NamespaceNotFound (26) on a FRESH database — the collection does not exist
// yet, so Mongo reports the missing collection, never the missing index (27).
// Tolerating only 27 made EnsureSchema fail on every empty database, which
// fails server boot: invisible to an existing deployment, fatal to a new one
// (it turned cloud-e2e red while prod stayed healthy).
func TestIsIndexNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"IndexNotFound (already migrated)", mongo.CommandError{Code: 27, Message: "index not found"}, true},
		{"NamespaceNotFound (fresh database)", mongo.CommandError{Code: 26, Message: "ns not found"}, true},
		{"wrapped NamespaceNotFound", fmt.Errorf("ensure schema: %w", mongo.CommandError{Code: 26}), true},
		{"nil", nil, false},
		{"a real failure must NOT be swallowed", mongo.CommandError{Code: 13, Message: "unauthorized"}, false},
		{"a non-command error", errors.New("connection refused"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsIndexNotFound(c.err); got != c.want {
				t.Errorf("IsIndexNotFound(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
