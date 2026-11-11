//go:build unix

package docker

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// Unlike argv-only Command tests, this starts the fake runtime through real
// Run.Exec. The name-only Docker channel must find its nonce in the client's
// Cmd.Env, not ambient or argv. No daemon/container is used.
func TestExecPreservesDriverEnvironment(t *testing.T) {
	const key = "ITERION_TEST_DOCKER_EXEC_NONCE"
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	fake := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
set -eu
[ "$1" = exec ] || exit 41
shift
while [ "$#" -gt 0 ]; do
 case "$1" in
  --workdir|--user) shift 2 ;;
  --env) [ "$2" = ITERION_TEST_DOCKER_EXEC_NONCE ] || exit 42; shift 2 ;;
  fixture-container) shift; break ;;
  *) exit 43 ;;
 esac
done
[ "$1" = printenv ] && [ "$2" = ITERION_TEST_DOCKER_EXEC_NONCE ] || exit 44
printf '%s' "${ITERION_TEST_DOCKER_EXEC_NONCE-}"
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	r := execEnvRun(iterlog.Nop())
	r.driver.cli = CLI{rt: RuntimeDocker, path: fake}
	r.containerID = "fixture-container"
	nonce := "driver-transport-" + rand.Text()
	opts := sandbox.ExecOpts{Env: map[string]string{key: nonce}}
	cmd := r.Command(context.Background(), []string{"printenv", key}, opts)
	if strings.Contains(strings.Join(cmd.Args, "\x00"), nonce) {
		t.Fatal("nonce leaked into client argv")
	}
	result, err := r.Exec(context.Background(), []string{"printenv", key}, opts)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("fake runtime: %+v %v", result, err)
	}
	if string(result.Stdout) != nonce {
		t.Fatalf("TRANSPORT_LOST: got %q, want private driver nonce %q", result.Stdout, nonce)
	}
	if _, ok := os.LookupEnv(key); ok {
		t.Fatal("driver changed ambient environment")
	}
}
