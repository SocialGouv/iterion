package deeplink

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestStudioLinkShapes(t *testing.T) {
	const base = "https://iterion.cloud"
	const runID = "01a09ec6-b5b3-7660-8ef1-295031b7260d"

	for _, tc := range []struct{ name, got, want string }{
		{"a run page", Run(base, runID), "https://iterion.cloud/studio/runs/" + runID},
		{"the run list", Runs(base), "https://iterion.cloud/studio/runs"},
		{"an arbitrary studio route", Studio(base, "/board"), "https://iterion.cloud/studio/board"},
		{"the legacy run spelling, for reading only", LegacyRun(base, runID), "https://iterion.cloud/runs/" + runID},
		{"a root-relative studio path", Path("/teams/t1"), "/studio/teams/t1"},
		{"the studio root", Path(""), "/studio"},
		// A base with a trailing slash used to produce "//runs/<id>" at two of
		// the six call sites, because only four of them trimmed it.
		{"a base with a trailing slash", Run(base+"/", runID), "https://iterion.cloud/studio/runs/" + runID},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// An empty base yields a root-relative path rather than an empty string: the
// web-push service worker resolves the link against its own origin, and
// blanking it would remove the only address a notification carries. Callers
// that need an absolute URL (mail, a forge status) keep their own guard.
func TestEmptyBaseYieldsARootRelativePath(t *testing.T) {
	if got, want := Run("", "r1"), "/studio/runs/r1"; got != want {
		t.Errorf("Run with no base = %q, want %q", got, want)
	}
}

func TestRunIDIsEscapedIntoThePath(t *testing.T) {
	// Run ids are ULIDs in practice; an id that is not must not be able to
	// leave the path it was put in.
	if got := Run("https://x", "a/../../etc"); got != "https://x/studio/runs/a%2F..%2F..%2Fetc" {
		t.Errorf("Run did not escape a path-bearing id: %q", got)
	}
}

// LegacyRun reconstructs a string ANOTHER build wrote, and that build did not
// escape. Escaping here would invent a URL that never existed, so the run
// would not recognise its own in-flight commit status and the pull request
// would wait on a claim nothing resolves. Run ids are caller-chosen on the
// launch API, so an id that needs escaping is reachable.
func TestLegacyRunReproducesWhatWasWrittenNotWhatWouldBeWrittenToday(t *testing.T) {
	const base = "https://iterion.cloud"
	for _, id := range []string{"run x", "runé", "run/x", "run+x", "01a09ec6-b5b3-7660-8ef1-295031b7260d"} {
		// Byte-for-byte what the pre-move writer produced: `base + "/runs/" + id`.
		want := base + "/runs/" + id
		if got := LegacyRun(base, id); got != want {
			t.Errorf("LegacyRun(%q) = %q, want %q — a status carrying the second string would not be recognised", id, got, want)
		}
	}
}

// The studio's router and this package must name the same prefix. Nothing
// executes both, so the agreement is checked against the studio's source
// rather than inferred — a convenience guard, not a proof the router honours
// the constant (studio/src/__tests__ covers that side).
func TestStudioBaseMatchesTheStudioConstant(t *testing.T) {
	path := filepath.Join("..", "..", "studio", "src", "lib", "scope.ts")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := regexp.MustCompile(`STUDIO_BASE\s*=\s*"([^"]*)"`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s declares no STUDIO_BASE — the studio's router prefix must stay greppable from here", path)
	}
	if got := string(m[1]); got != StudioBase {
		t.Errorf("STUDIO_BASE = %q in %s, but deeplink.StudioBase = %q — server-built links and client routes would disagree", got, path, StudioBase)
	}
}
