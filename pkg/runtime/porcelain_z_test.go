package runtime

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// #1577: git prints a rename's source UNQUOTED whenever the name holds no
// byte it escapes, so a source literally named `x -> y.md` put an arrow
// inside the source and the line-oriented porcelainPaths cut there —
// `porcelainPaths("R  x -> y.md -> z.md")` returned ["y.md -> z.md"], and
// `R  x -> .claude/evil.md -> z.md` made commitWorkPaths read the run's
// work as mirror noise (IsMirror on ".claude/evil.md -> z.md"), refusing a
// commit-and-finalize whose only change was that rename. The probes now
// read only the `-z` porcelain, where the rename carries destination and
// source as two NUL-separated fields. These are the ticket's executed
// cases in the form the parser consumes; the red round ran them against
// the line-oriented parser verbatim.
func TestPorcelainPathsARenameSourceHoldingAnArrow(t *testing.T) {
	cases := []struct {
		name      string
		porcelain string
		want      []string
	}{
		{"unquoted source holding an arrow", "R  z.md\x00x -> y.md\x00", []string{"z.md"}},
		{"source holding an arrow and a non-ASCII byte", "R  z.md\x00x -> é.md\x00", []string{"z.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := porcelainPaths(tc.porcelain)
			if len(got) != 1 || got[0] != tc.want[0] {
				t.Fatalf("porcelainPaths(%q) = %q, want %q", tc.porcelain, got, tc.want)
			}
		})
	}

	t.Run("the destination of an arrowed source is not read as the mirror", func(t *testing.T) {
		got := commitWorkPaths("R  z.md\x00x -> .claude/evil.md\x00")
		if len(got) != 1 || got[0] != "z.md" {
			t.Fatalf("commitWorkPaths = %q, want [z.md] — the run's work refused as mirror noise is the #1577 consumer impact", got)
		}
	})
}

// The `-z` record order the parser assumes — destination first, source
// second — pinned against the git on this machine, end to end through the
// production invocation: a real repository whose only change is a rename
// whose source holds an arrow reports exactly the destination as the run's
// output.
func TestRunOutputPathsARenameSourceHoldingAnArrowAgainstRealGit(t *testing.T) {
	repo, _ := initBareishRepo(t)
	writeFile(t, repo+"/x -> y.md", "the run's work\n")
	gittest.Run(t, repo, "add", "-A")
	gittest.Run(t, repo, "commit", "-qm", "track the arrowed name")
	gittest.Run(t, repo, "mv", "x -> y.md", "z.md")

	porcelain, err := runGit(repo, "status", "--porcelain", "-z")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(porcelain, "R  z.md\x00x -> y.md\x00") {
		t.Fatalf("the -z record order is not destination-first on this git: %q", porcelain)
	}
	if got := runOutputPaths(porcelain); len(got) != 1 || got[0] != "z.md" {
		t.Fatalf("runOutputPaths = %q, want [z.md]", got)
	}
}
