//go:build !live

package e2e

import (
	"testing"

	"github.com/SocialGouv/iterion/internal/detecttest"
)

// isolateProviders keeps the host's provider credentials out of the
// deterministic layer (detecttest.IsolateProviders); the live layer runs on
// them by design (providers_isolation_live_test.go).
func isolateProviders() (restore func()) { return detecttest.IsolateProviders() }

// The provider isolation TestMain installs, witnessed: a fixture whose answer
// falls to the last-resort extraction must not reach a host provider.
func TestThisPackageDetectsNoProvider(t *testing.T) { detecttest.RequireNoProvider(t) }
