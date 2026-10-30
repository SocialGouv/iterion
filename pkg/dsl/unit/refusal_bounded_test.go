package unit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// The load's FIRST refusal joins the unit's list beside the loader's report
// — it is the ShortError's answer, which no report cap may drop — so it is
// bounded where it is recorded (loader.refuse), not by the report (#2801).
// An import path past the filesystem's name limit reads as ENAMETOOLONG and
// was answered with the path quoted three times: 1 MiB of path must come
// back under the bound.
func TestTheFirstRefusalIsBoundedAtTheDoor(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("a", 1<<20)
	main := "import \"lib/" + long + ".bot\"\n\nworkflow w:\n  entry: done\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadDir(filepath.Join(dir, "main.bot"))
	var short *ShortError
	if !errors.As(err, &short) {
		t.Fatalf("a fragment the OS cannot read leaves the load short: %v", err)
	}
	d := short.ReadRefusal().Diagnostic
	if len(d.Message) > expr.MaxDiagnosticText+len("…") || len(d.Hint) > expr.MaxDiagnosticText+len("…") {
		t.Fatalf("the import path rode the refusal whole: %d bytes of message, %d of hint, want each bounded to %d of content", len(d.Message), len(d.Hint), expr.MaxDiagnosticText)
	}
	if !strings.Contains(d.Message, "import") {
		t.Errorf("the refusal names its cause: %.200s", d.Message)
	}
}
