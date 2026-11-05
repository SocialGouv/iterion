//go:build live

package e2e

// isolateProviders leaves the host's provider credentials in place: the live
// layer spends them by design.
func isolateProviders() (restore func()) { return func() {} }
