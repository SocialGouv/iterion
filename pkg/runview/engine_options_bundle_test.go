package runview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileForLaunchRejectsMissingBundleExport(t *testing.T) {
	dir := promptedBundle(t)
	path := filepath.Join(dir, "main.bot")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest := "name: mybot\nversion: 0.1.0\nexports:\n  workflows:\n    - id: extra\n      path: workflows/gone.bot\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, inline := range []bool{false, true} {
		src, bundleDir := "", ""
		if inline {
			src, bundleDir = string(source), dir
		}
		wf, _, b, err := compileForLaunch(path, src, bundleDir)
		if err == nil || !strings.Contains(err.Error(), "gone.bot") || wf != nil || b != nil {
			t.Fatalf("inline=%v: invalid bundle reached execution: wf=%v bundle=%v err=%v", inline, wf, b, err)
		}
	}
}
