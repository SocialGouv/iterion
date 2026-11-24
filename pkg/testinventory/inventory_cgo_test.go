//go:build cgo && !windows

package testinventory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestReadAcceptsANativeCompilerWarningOnAColdBuild(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"go.mod": "module example.test/inventorywarning\n\ngo 1.26\n",
		"warning.go": fmt.Sprintf(`package inventorywarning
/*
#warning inventory compiler warning
static int answer(void) { return 42; }
*/
import "C"
func Answer() int { return int(C.answer()) }
// Unique source makes the compiler execute even when other fixtures are cached: %s
`, dir),
		"warning_test.go": "package inventorywarning\nimport \"testing\"\nfunc TestAnswer(t *testing.T) { if Answer()!=42 { t.Fatal(\"wrong answer\") } }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, temperature := range []string{"cold", "warm"} {
		got, err := Read(ctx, ".")
		if err != nil || !slices.Equal(got, []string{"TestAnswer"}) {
			t.Fatalf("%s compiler-warning inventory = %v, err %v", temperature, got, err)
		}
	}
}
