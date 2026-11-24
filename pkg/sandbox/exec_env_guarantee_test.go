//go:build unix

package sandbox

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// ExecOpts.Env belongs to the workload; ExecCmd must execute the environment
// the driver already prepared, not rebuild it from either ambient or opts.
func TestExecCmdPreservesDriverEnvironment(t *testing.T) {
	const key = "ITERION_TEST_EXECCMD_DRIVER_ENV"
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	for _, streamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streamed"}[streamed], func(t *testing.T) {
			nonce := filepath.Base(t.TempDir())
			cmd := exec.Command("sh", "-c", "printf '%s' \"$"+key+"\"; printf err >&2; exit 7")
			cmd.Env = append(os.Environ(), key+"="+nonce)
			// This is intentionally different: forwarding workload opts must not
			// replace the host transport while the convenience wrapper wires IO.
			opts := ExecOpts{Env: map[string]string{key: "workload-only"}}
			var stdout, stderr bytes.Buffer
			if streamed {
				opts.Stdout, opts.Stderr = &stdout, &stderr
			}
			result, err := ExecCmd(cmd, opts)
			if err != nil || result.ExitCode != 7 {
				t.Fatalf("exit normalization: %+v %v", result, err)
			}
			out, errOut := result.Stdout, result.Stderr
			if streamed {
				if len(out) != 0 || len(errOut) != 0 {
					t.Fatal("streamed outputs also buffered")
				}
				out, errOut = stdout.Bytes(), stderr.Bytes()
			}
			if string(out) != nonce {
				t.Fatalf("TRANSPORT_LOST: got %q, want private driver nonce %q", out, nonce)
			}
			if string(errOut) != "err" {
				t.Fatalf("stderr=%q", errOut)
			}
		})
	}
}
