package mongo

import (
	"crypto/sha256"
	"fmt"
	"net/url"
)

func portBackendIdentity(uri, database string) (string, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("store/mongo: parse backend identity: %w", err)
	}
	parsed.User = nil // principals are inventoried separately
	digest := sha256.Sum256([]byte(parsed.String() + "\x00" + database))
	return fmt.Sprintf("mongo-sha256:%x", digest), nil
}

// PortBackendIdentity binds a distributed activation proof to the configured
// Mongo endpoint and database without exposing credentials. The userinfo is
// excluded so server and runner principals can share one backend identity;
// the access census checks those principals separately.
func (s *Store) PortBackendIdentity() string { return s.portBackendIdentity }
