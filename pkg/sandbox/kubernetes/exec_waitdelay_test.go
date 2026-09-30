package kubernetes

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// TestExec_keepsItsDeadlineWhenAKubectlChildHoldsThePipes: a kubectl whose
// child — an exec credential plugin, say — still holds its pipes when the
// deadline passes does not hold the exec past it: the scratch bank's
// listing, tar and restore run on this path, inside the teardown's budget.
func TestExec_keepsItsDeadlineWhenAKubectlChildHoldsThePipes(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n(sleep 20) &\nsleep 20\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Run{
		driver:    &Driver{logger: iterlog.Nop(), kubectl: fake},
		podName:   "pod",
		namespace: "ns",
		prepared:  &Prepared{workspace: "/workspace"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	start := time.Now()
	_, _ = r.Exec(ctx, []string{"tar", "-C", "/tmp/iterion-scratch", "-czf", "-", "."}, sandbox.ExecOpts{Stdout: &out, Stderr: &errb})
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("an exec with a 1s deadline returned after %s: a kubectl child holding its pipes outlived the deadline", took.Round(100*time.Millisecond))
	}
}
