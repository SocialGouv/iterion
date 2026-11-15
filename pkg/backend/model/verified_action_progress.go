package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
)

var errSelfRepairUnchanged = errors.New("self-repair proposal and failure evidence unchanged")

// selfRepairFingerprint identifies the command the rung would execute and the
// complete evidence it answers. Length framing preserves arbitrary string bytes;
// neither delimiters nor JSON's replacement of invalid UTF-8 can alias inputs.
// This identity belongs to one ladder invocation and is never logged or stored.
func selfRepairFingerprint(corrected, stdout, stderr, lastCmd string) string {
	h := sha256.New()
	for _, field := range [...]string{corrected, stdout, stderr, lastCmd} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}
