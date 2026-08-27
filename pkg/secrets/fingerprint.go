package secrets

import (
	"crypto/sha256"
	"encoding/hex"
)

// fingerprintHex returns the first 8 bytes of SHA-256 hex-encoded.
// 64 bits is enough to distinguish credentials in audit logs while
// keeping the value short. It is an IDENTITY, and one that now travels
// further than a log line: the usage-cap meter composes it into its
// store key (usagecap.Key), so a collision would merge two credentials'
// meters — a wrong reading, never a disclosure, since the fingerprint is
// no more a handle on the secret than it ever was. The exported form is
// FingerprintSHA256 (sealer.go).
func fingerprintHex(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:8])
}
