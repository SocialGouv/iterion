package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestResolveResumeBundleWorkflowUsesAndVerifiesExport(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "workflows", "child.bot")
	if err := os.Mkdir(filepath.Dir(child), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(dir, "main.bot"): "workflow main {}\n",
		child:                          "workflow child {}\n",
		filepath.Join(dir, "manifest.yaml"): `name: shared-planner
version: 1.0.0
schema_version: 1
exports:
  workflows:
    - id: child
      path: workflows/child.bot
`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := bundle.ContentHashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := &store.Run{ID: "run-1", BundleName: "shared-planner", BundleVersion: "1.0.0", BundleWorkflow: "child", BundleHash: hash}
	got, err := ResolveResumeBundleWorkflow(r, b, filepath.Join(dir, "main.bot"), false)
	if err != nil {
		t.Fatalf("ResolveResumeBundleWorkflow: %v", err)
	}
	if got != child {
		t.Fatalf("path = %q, want %q", got, child)
	}

	r.BundleVersion = "0.9.0"
	if _, err := ResolveResumeBundleWorkflow(r, b, child, false); !errors.Is(err, ErrWorkflowSourceChanged) {
		t.Fatalf("error = %v, want ErrWorkflowSourceChanged", err)
	}
	if forced, err := ResolveResumeBundleWorkflow(r, b, child, true); err != nil || forced != child {
		t.Fatalf("forced path = %q, err = %v", forced, err)
	}
}
