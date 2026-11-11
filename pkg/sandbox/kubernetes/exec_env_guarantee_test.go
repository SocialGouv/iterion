//go:build unix

package kubernetes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// Kubernetes currently leaves Cmd.Env nil and carries workload Env on the
// remote argv. This witness proves that actual contract through Run.Exec:
// the client keeps its ambient configuration and cannot inherit workload
// values. TestExecCmdPreservesDriverEnvironment in sandbox separately proves
// preservation of a non-nil driver environment at the common ExecCmd seam.
func TestExecPreservesDriverEnvironment(t *testing.T) {
	isolateExecEnvironment(t)
	const workload = "ITERION_TEST_KUBE_WORKLOAD"
	t.Setenv(workload, "")
	if err := os.Unsetenv(workload); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(t.TempDir(), "client-config")
	t.Setenv("KUBECONFIG", host)
	fake := filepath.Join(t.TempDir(), "kubectl")
	script := `#!/bin/sh
set -eu
[ -z "${ITERION_TEST_KUBE_WORKLOAD+x}" ] || exit 41
printf 'client=%s\n' "$KUBECONFIG"
while [ "$#" -gt 0 ] && [ "$1" != -- ]; do shift; done
[ "$#" -gt 0 ] || exit 42
shift
[ "$1" = env ] || exit 43
exec "$@"
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	r := testRun()
	r.driver.kubectl = Kubectl(fake)
	nonce := filepath.Base(t.TempDir())
	opts := sandbox.ExecOpts{Env: map[string]string{workload: nonce, "KUBECONFIG": "workload-config"}}
	result, err := r.Exec(context.Background(), []string{"sh", "-c", "printf 'workload=%s|%s' \"$" + workload + "\" \"$KUBECONFIG\""}, opts)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("fake kubectl: %+v %v", result, err)
	}
	want := "client=" + host + "\nworkload=" + nonce + "|workload-config"
	if strings.TrimSpace(string(result.Stdout)) != want {
		t.Fatalf("TRANSPORT_LOST: got %q, want %q", result.Stdout, want)
	}
}
