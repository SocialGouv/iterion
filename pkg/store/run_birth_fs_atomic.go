//go:build linux || darwin

package store

func runBirthAtomicPublicationSupported() bool { return true }
