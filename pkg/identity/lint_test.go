package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// UpdateTeam replaces a team document WHOLE (ReplaceOneChecked). It is how
// a stale read silently erases a field its build predates — the sovereign
// runner-pool mapping among them (#2029 F2). Every production writer is a
// PatchTeam now; this lint keeps it that way: any new non-test caller of
// UpdateTeam outside this package must justify itself here, not in code.
// (The memory twin's aliasing hides the erase from value-based tests, and
// the mongo leg of the patch conformance only bites the patch path — the
// lint is the durable gate.)
func TestUpdateTeamHasNoProductionCallers(t *testing.T) {
	root := "../.."
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch filepath.Base(path) {
			case ".git", "vendor", "node_modules", ".works", ".claude", ".iterion":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		isTest := strings.HasSuffix(path, "_test.go")
		inIdentity := strings.HasPrefix(filepath.ToSlash(path), "pkg/identity/")
		if isTest || inIdentity {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(body), ".UpdateTeam(") {
			t.Errorf("%s calls UpdateTeam — a whole-document team replace outside pkg/identity (use PatchTeam; see the F2 hazard)", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
