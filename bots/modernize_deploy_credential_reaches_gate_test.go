package bots

import (
	"os"
	"path/filepath"
	"testing"
)

// Declaring a file secret is not the same as the gate being able to USE it.
// A file secret's `env:` is written into the SANDBOX container spec only; on
// a host run the runner materialises the file and exports nothing. So a
// contract writing its gate against $DEPLOY_CREDENTIAL would read it empty
// and refuse — for want of an identity that is, in fact, mounted. lot_verify
// therefore exports the name itself, from the rendered path.
//
// Both directions matter. Exporting unconditionally would be worse than not
// exporting at all: an unresolved optional secret renders an opaque
// placeholder, and a gate handed that as a path fails on a file that never
// existed instead of reporting a missing credential.
func TestLotVerifyPutsTheDeployCredentialWithinTheGatesReach(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	const plan = `version: 1
oracle:
  refs_dir: .golden-master/refs
lots:
  - id: L1
    title: "prove it against a deployed application"
    status: todo
    exit_gate:
      - "test -f \"$DEPLOY_CREDENTIAL\""
`

	t.Run("credential mounted: the gate command reads it", func(t *testing.T) {
		ws, _, git := modernizeRepo(t, plan)
		modernizeNet(t, ws)
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		base := git("rev-parse", "HEAD")

		// Outside the workspace, as a real mount is: a credential inside the
		// tree would read as uncommitted work of the run.
		cred := filepath.Join(t.TempDir(), "deploy_credential")
		if err := os.WriteFile(cred, []byte("never-read-me\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		out, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base,
			"test -f \"$DEPLOY_CREDENTIAL\"", nil, cred)
		if exit != 0 {
			t.Fatalf("lot_verify exited %d, want a verdict (out %+v)", exit, out)
		}
		if !out.GatePassed {
			t.Errorf("gate_passed = false — $DEPLOY_CREDENTIAL did not reach the gate command: %s", out.LogTail)
		}
	})

	t.Run("no credential: nothing is exported, and the gate says so", func(t *testing.T) {
		ws, _, git := modernizeRepo(t, plan)
		modernizeNet(t, ws)
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		base := git("rev-parse", "HEAD")

		// No credPath: the reference renders the placeholder an unresolved
		// optional secret gets. The gate is `test -z`, not `test -f`, and
		// that is the whole point: `test -f` fails BOTH when the variable is
		// unset and when the placeholder was exported as a path, so it could
		// not tell the two apart — it would stay green against the very
		// regression this case exists to catch. `test -z` passes only when
		// nothing was exported.
		out, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base,
			"test -z \"$DEPLOY_CREDENTIAL\"", nil)
		if exit != 0 {
			t.Fatalf("lot_verify exited %d, want a verdict (out %+v)", exit, out)
		}
		if !out.GatePassed {
			t.Errorf("gate_passed = false with no credential installed — $DEPLOY_CREDENTIAL is not empty, so the placeholder was exported as if it were a path: %s", out.LogTail)
		}
	})
}
