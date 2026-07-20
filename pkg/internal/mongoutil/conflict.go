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

// IsIndexNotFound reports whether err is Mongo's IndexNotFound, so a schema
// migration can drop a retired index unconditionally: absent is the expected
// steady state (fresh install, or already migrated), not a failure.
func IsIndexNotFound(err error) bool {
	if err == nil {
		return false
	}
	var cmd mongo.CommandError
	if errors.As(err, &cmd) {
		return cmd.Code == 27 // IndexNotFound
	}
	return false
}

// IsDuplicateKey reports whether err is a Mongo E11000 duplicate-key
// error, so InsertOne/ReplaceOne callers across storage packages can
// translate it to a domain sentinel without each re-deriving the check.
func IsDuplicateKey(err error) bool {
	return err != nil && mongo.IsDuplicateKeyError(err)
}
