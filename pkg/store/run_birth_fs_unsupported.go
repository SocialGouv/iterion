//go:build !linux && !darwin

package store

// Windows' current PID lock retains its pathname, so moving its directory is
// not a qualified lease relocation. Other platforms lack atomic no-replace.
// Never fall back to the check-then-rename helper for a published birth.
func runBirthAtomicPublicationSupported() bool { return false }
