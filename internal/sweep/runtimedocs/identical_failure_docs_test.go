package runtimedocs

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// The default is ONE constant, and the two pages an operator reads state its
// value once each: the variable's row, and the table of what a limit means.
// Changing the default is that line and these two numbers; this is what
// names them.
func TestTheDocsStateTheDefaultLimitOnceEach(t *testing.T) {
	root := sweepwalk.ModuleRoot(t)
	want := strconv.Itoa(runtime.DefaultNodeMaxIdenticalFailures)
	for _, doc := range []struct {
		path string
		// stated captures the value the page gives as the default.
		stated *regexp.Regexp
	}{
		{"docs/environment-variables.md", regexp.MustCompile("(?m)^\\| `" + runtime.EnvNodeMaxIdenticalFailures + "` \\|.*\\| `([0-9]+)` \\|$")},
		{"docs/resume.md", regexp.MustCompile("(?m)^\\| `([0-9]+)` \\(the default\\) \\|")},
	} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(doc.path)))
		if err != nil {
			t.Fatal(err)
		}
		found := doc.stated.FindAllStringSubmatch(string(raw), -1)
		switch {
		case len(found) != 1:
			t.Errorf("%s states the default limit %d time(s) in the form %s, want exactly once", doc.path, len(found), doc.stated)
		case found[0][1] != want:
			t.Errorf("%s gives %s as the default limit, and runtime.DefaultNodeMaxIdenticalFailures is %s", doc.path, found[0][1], want)
		}
	}
}
