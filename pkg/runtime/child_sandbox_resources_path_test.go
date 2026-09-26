package runtime

import (
	"strings"
	"testing"
)

// TestChildDevboxStagingPathIsUniquePerProvisioning pins #1787: two
// provisions of the SAME run id stage at two different paths, so
// concurrent stagers cannot delete each other's install — the defect was
// a path keyed by the run id alone, and every process staging for the run
// id "child" raced on /tmp/iterion-devbox/child-<sha256("child")>.
func TestChildDevboxStagingPathIsUniquePerProvisioning(t *testing.T) {
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		p, err := childDevboxStagingPath("child")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if seen[p] {
			t.Fatalf("call %d: path %q already staged — concurrent stagers of one run id would clobber each other", i, p)
		}
		seen[p] = true
		// The run id stays readable in the path: a staged dir is named in
		// warnings and cleanup logs, and the operator follows it.
		if !strings.HasPrefix(p, "/tmp/iterion-devbox/child-") {
			t.Fatalf("path %q lost the child-<runid> shape", p)
		}
	}

	// The cleanup owns exactly what the path names: a relative or host-odd
	// spelling would leak the rm -rf in the sandbox beyond the staged dir.
	p, err := childDevboxStagingPath("child")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, "/tmp/") || strings.Contains(p, "..") {
		t.Fatalf("path %q is not a clean absolute path under /tmp", p)
	}
}
