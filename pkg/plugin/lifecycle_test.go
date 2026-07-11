package plugin

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	writeFileT(t, filepath.Join(home, pluginsSubdir, "lc-demo", ManifestFile),
		"name: lc-demo\ncontributes:\n  lifecycle:\n    index: echo \"indexing {{workspace}}\" && pwd\n")
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	workspace := t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := RunLifecycle(context.Background(), reg, "lc-demo", "index", workspace, &stdout, &stderr); err != nil {
		t.Fatalf("RunLifecycle: %v (stderr: %s)", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "indexing "+workspace) {
		t.Errorf("stdout missing expanded {{workspace}}: %q", out)
	}
	// pwd proves the command ran with Dir = workspace.
	if !strings.Contains(out, "\n"+workspace+"\n") {
		t.Errorf("stdout missing pwd = workspace: %q", out)
	}

	// Error paths are explicit.
	if err := RunLifecycle(context.Background(), reg, "lc-demo", "compact", workspace, &stdout, &stderr); err == nil {
		t.Error("unknown phase accepted")
	}
	if err := RunLifecycle(context.Background(), reg, "lc-demo", "refresh", workspace, &stdout, &stderr); err == nil {
		t.Error("empty refresh command accepted")
	}
	if err := RunLifecycle(context.Background(), reg, "nope", "index", workspace, &stdout, &stderr); err == nil {
		t.Error("unknown plugin accepted")
	}
}
