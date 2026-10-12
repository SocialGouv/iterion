package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestResumeBundleWorkflowUsesAndVerifiesExport(t *testing.T) {
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
	got, identityErr, err := ResumeBundleWorkflow(r, b, filepath.Join(dir, "main.bot"))
	if err != nil || identityErr != nil {
		t.Fatalf("ResumeBundleWorkflow: identity %v, err %v", identityErr, err)
	}
	if got != child {
		t.Fatalf("path = %q, want %q", got, child)
	}

	r.BundleVersion = "0.9.0"
	got, identityErr, err = ResumeBundleWorkflow(r, b, child)
	if err != nil || !errors.Is(identityErr, ErrWorkflowSourceChanged) {
		t.Fatalf("identity = %v, err = %v, want ErrWorkflowSourceChanged aside", identityErr, err)
	}
	if got != child {
		t.Fatalf("the path beside a changed identity = %q, want %q", got, child)
	}
}
