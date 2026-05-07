// Package mongoutil holds tiny helpers for the Mongo driver shared
// across iterion's storage packages (pkg/store/mongo, pkg/identity,
// pkg/secrets, pkg/auth). It only contains stateless, dependency-free
// utilities so any pkg/ subpackage can import it without creating
// cycles.
package mongoutil

import (
	"errors"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// IsIndexConflict reports whether err is the benign "index already
// exists with different options" / "key specs conflict" pair Mongo
// returns when EnsureSchema is re-run against a database whose
// indexes were created by an older driver version. Treating these
// as no-ops keeps EnsureSchema idempotent across binary upgrades;
// operators recreate indexes by hand when the geometry changes.
func IsIndexConflict(err error) bool {
	if err == nil {
		return false
	}
	var cmd mongo.CommandError
	if errors.As(err, &cmd) {
		switch cmd.Code {
		case 85, 86: // IndexOptionsConflict / IndexKeySpecsConflict
			return true
		}
	}
	return false
}
