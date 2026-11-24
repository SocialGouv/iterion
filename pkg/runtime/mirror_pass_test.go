package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// testMirrorPass opens a mirror pass for workDir with the given trust and
// registers its Close. The trusted form is what every pre-handle test means:
// a passthrough handle over an operator-controlled workspace.
func testMirrorPass(t *testing.T, workDir string, trust store.RunTrust) *mirrorPass {
	t.Helper()
	p, err := newMirrorPass(workDir, trust)
	if err != nil {
		t.Fatalf("newMirrorPass: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// testTrustedPass is the trusted-workspace pass every pre-handle test runs
// under: the handle is a passthrough and the collision policy keeps its
// workspace-wins shape.
func testTrustedPass(t *testing.T) *mirrorPass {
	t.Helper()
	return testMirrorPass(t, "", store.RunTrustDefault)
}

// testTrustedRoot is the passthrough handle for the marker/manifest
// primitives' tests.
func testTrustedRoot(t *testing.T) *workspaceRoot {
	t.Helper()
	return testTrustedPass(t).root()
}
