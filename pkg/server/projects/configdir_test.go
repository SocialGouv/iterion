package projects

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Under go test the desktop project registry lives in the test process's own
// directory, never in the operator's config dir: tests that register
// projects rewrote the desktop app's current project.
func TestConfigDir_UnderGoTestIsTheTestProcessDir(t *testing.T) {
	dir, ok := store.TestProcessDir()
	if !ok {
		t.Fatal("store.TestProcessDir reports no test process")
	}
	// It is the test process's own home — the directory hometest.Isolate
	// releases at exit — not a location shared with other test binaries.
	t.Setenv("ITERION_HOME", "")
	if home, err := store.IterionHome(); err != nil || home != dir {
		t.Fatalf("store.TestProcessDir() = %q, but the test process's own home is %q (%v)", dir, home, err)
	}
	got, err := ConfigDir()
	if err != nil || got != filepath.Join(dir, "config") {
		t.Fatalf("ConfigDir() = %q, %v; want %q", got, err, filepath.Join(dir, "config"))
	}
	if operator, err := os.UserConfigDir(); err == nil && (got == operator || strings.HasPrefix(got, operator+string(filepath.Separator))) {
		t.Fatalf("ConfigDir() = %q lies in the operator's config dir %s", got, operator)
	}
	p, err := Path()
	if err != nil || !strings.HasPrefix(p, got+string(filepath.Separator)) {
		t.Fatalf("Path() = %q, %v; want the registry under ConfigDir %q", p, err, got)
	}
}
