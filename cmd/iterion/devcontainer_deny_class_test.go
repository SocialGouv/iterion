package main

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
)

// This binary is its own in-container runner (`iterion __claw-runner`), and
// main() wires error tracking for every command but the permission hook — so
// the names errtrack resolves are read inside a container whose environment a
// found devcontainer.json seeds from the reviewed repository. Unset, the
// package is off and reaches no network; a planted DSN therefore does not
// redirect the operator's reporting, it switches an outbound channel on
// (which is why main() skips the whole startup for the permission hook, whose
// `.env` the gated agent can write). The devcontainer deny class must refuse
// these names, read from errtrack's own constants so the guard follows a
// rename.
func TestDevcontainerDenyClassCoversTheRunnersErrorTracking(t *testing.T) {
	for _, name := range []string{errtrack.EnvDSN, errtrack.EnvEnvironment, errtrack.EnvTracesSampleRate} {
		if !devcontainer.DeniedEnvKey(name) {
			t.Errorf("errtrack reads %s, which a repository's devcontainer.json may still set for this binary in the container: devcontainer.DeniedEnvKey must refuse it", name)
		}
	}
}
