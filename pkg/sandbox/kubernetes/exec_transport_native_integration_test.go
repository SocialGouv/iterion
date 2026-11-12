package kubernetes

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestNativeExecTransportThroughCommand(t *testing.T) {
	isolateTransportConfig(t)
	hostHelper, guestHelper := nativeExecTransport(t)
	t.Setenv(execTransportPrefix+"HOST_HELPER", hostHelper)
	t.Setenv(execTransportPrefix+"GUEST_HELPER", guestHelper)
	cfg, err := loadExecTransportConfig()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	witness := filepath.Join(dir, "client-config")
	client := filepath.Join(dir, "kubectl")
	t.Setenv("CLIENT_WITNESS_PATH", witness)
	t.Setenv("KUBECONFIG", "operator-config")
	script := `#!/bin/sh
printf '%s' "$KUBECONFIG" > "$CLIENT_WITNESS_PATH"
for target; do :; done
exec /usr/bin/env -i PATH=/bin:/usr/bin NATIVE_ONLY="$POD_VALUE" KUBECONFIG=pod-config "$target"
`
	if err := os.WriteFile(client, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	r := testRun()
	r.driver.kubectl = Kubectl(client)
	r.driver.transport = cfg
	for _, pod := range []string{"pod-first", "pod-second"} {
		t.Setenv("POD_VALUE", pod)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		cmd := r.Command(ctx, []string{"/bin/sh", "-c", `printf '%s|%s|%s|' "$SECRET" "$KUBECONFIG" "$NATIVE_ONLY"; cat`}, sandbox.ExecOpts{Env: map[string]string{"SECRET": "private-fixture", "KUBECONFIG": "target-config"}, Stdin: strings.NewReader("user-stdin")})
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		stdout, err := cmd.Output()
		cancel()
		want := "private-fixture|target-config|" + pod + "|user-stdin"
		if err != nil || string(stdout) != want || stderr.Len() != 0 {
			t.Fatalf("native result=%q stderr=%q err=%v want=%q", stdout, stderr.String(), err, want)
		}
		got, err := os.ReadFile(witness)
		if err != nil || string(got) != "operator-config" {
			t.Fatalf("host config=%q err=%v", got, err)
		}
		for _, arg := range cmd.Args {
			if strings.Contains(arg, "private-fixture") {
				t.Fatal("target environment leaked into host argv")
			}
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, execErr := r.Exec(ctx, []string{"/bin/sh", "-c", `printf '%s|%s' "$X" "$PWD"; exit 7`}, sandbox.ExecOpts{Env: map[string]string{"X": "exec-output"}, WorkDir: dir})
	if execErr != nil || result.ExitCode != 7 || string(result.Stdout) != "exec-output|"+dir || len(result.Stderr) != 0 {
		t.Fatalf("native Exec result=%+v err=%v", result, execErr)
	}
	cmd := r.Command(ctx, []string{"/bin/sh", "-c", `IFS= read -r line; printf 'reply:%s' "$line"`}, sandbox.ExecOpts{Env: map[string]string{"X": "fixture"}, KeepStdinOpen: true})
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err = input.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	// The target exits after a line; its caller deliberately retains stdin.
	if err = cmd.Wait(); err != nil || stdout.String() != "reply:ping" || stderr.Len() != 0 {
		t.Fatalf("open-stdin result=%q stderr=%q err=%v", stdout.String(), stderr.String(), err)
	}
}
