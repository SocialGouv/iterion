package secrets

import (
	"crypto/sha256"
	"encoding/hex"
)

// fingerprintHex returns the first 8 bytes of SHA-256 hex-encoded. 64 bits
// is enough to distinguish credentials while keeping the value short, and
// it reveals nothing about the secret. FingerprintSHA256 is its exported
// form — the one every caller outside this package uses.
//
// It IS a key: the usage-cap meter composes it (usagecap.Key), so two
// distinct credentials colliding here would silently share one ledger.
// They would have to collide on 64 bits AND land on the same backend and
// tenant scope, which is far below the rate at which the readings they
// hold expire on their own. What the value must never be is a key to the
// SECRET: it is one-way, and nothing may treat holding it as proof of
// holding the credential it names.
func fingerprintHex(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:8])
}
