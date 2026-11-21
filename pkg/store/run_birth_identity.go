package store

import "crypto/rand"

// NewRunBirthID creates an opaque store-owned identity for a new run document.
// It is distinct from ExecutionEpoch, which rotates with execution ownership.
// Existing legacy documents must not acquire one during a metadata save.
func NewRunBirthID() string { return rand.Text() }
