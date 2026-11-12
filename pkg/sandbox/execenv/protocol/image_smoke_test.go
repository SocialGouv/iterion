package protocol

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestImageSmokeFrame(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(source), "../../../../sandbox/finalize/execenv-smoke.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "frame='"
	_, tail, ok := strings.Cut(string(raw), prefix)
	if !ok {
		t.Fatal("smoke frame missing")
	}
	encoded, _, ok := strings.Cut(tail, "'")
	if !ok {
		t.Fatal("smoke frame unterminated")
	}
	got, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	wd := "/tmp"
	in := Invocation{Argv: []string{"/bin/sh", "-c", `printf '%s|%s|' "$EXECENV_SMOKE" "$PWD"; cat`}, Env: map[string]string{"EXECENV_SMOKE": "frame-v2"}, WorkDir: &wd, Profile: &ExecutionProfile{DefaultPath: "/bin:/usr/bin", Shell: "/bin/sh"}}
	var want bytes.Buffer
	if err := WriteInvocation(&want, in); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatal("image smoke frame no longer matches the production v2 encoder")
	}
	if InvocationReadiness(in) != "IENV-READY-V2\x00\x00\x00" {
		t.Fatal("image smoke readiness no longer matches protocol")
	}
}

// An application import here would execute initialization before native inputs
// are sanitized. Check the actual dependency closure, not just direct imports.
func TestGuestDependencyClosure(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../.."))
	cmd := exec.Command("go", "list", "-mod=vendor", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./cmd/iterion-execenv-guest")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("guest dependency closure: %v: %s", err, output)
	}
	allowed := map[string]bool{"github.com/SocialGouv/iterion/cmd/iterion-execenv-guest": true, "github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol": true}
	for _, dependency := range strings.Fields(string(output)) {
		if !allowed[dependency] {
			t.Errorf("guest imports non-leaf dependency %s", dependency)
		}
		delete(allowed, dependency)
	}
	if len(allowed) != 0 {
		t.Fatalf("guest dependency listing was incomplete: %v", allowed)
	}
}
