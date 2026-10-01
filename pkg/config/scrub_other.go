//go:build !linux

package config

// setNonDumpable has nothing to do where /proc/<pid>/environ does not exist.
func setNonDumpable() error { return nil }
