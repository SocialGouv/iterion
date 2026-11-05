package recovery_test

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/detecttest"
)

func TestMain(m *testing.M) {
	restoreProviders := detecttest.IsolateProviders()
	code := m.Run()
	restoreProviders()
	os.Exit(code)
}

// The provider isolation TestMain installs, witnessed: a fixture whose answer
// falls to the last-resort extraction must not reach a host provider.
func TestThisPackageDetectsNoProvider(t *testing.T) { detecttest.RequireNoProvider(t) }
