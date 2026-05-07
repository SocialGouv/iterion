package secrets

import (
	"crypto/sha256"
	"encoding/hex"
)

// fingerprintHex returns the first 8 bytes of SHA-256 hex-encoded.
// 64 bits is enough to distinguish keys in audit logs while keeping
// the value short; collisions in this space are not a security
// concern (we never use the fingerprint as a key).
func fingerprintHex(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:8])
}
