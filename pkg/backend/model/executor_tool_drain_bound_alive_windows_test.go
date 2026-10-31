//go:build windows

package model

// processAlive is never reached on Windows — every witness that joins a
// holder skips the platform first (the fixtures are POSIX shell recipes) — so
// the stub only has to compile.
func processAlive(int) bool { return false }
