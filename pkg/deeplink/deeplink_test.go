package deeplink

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// RunFromEnv is what a RUN publishes outside the deployment, so its policy
// on a missing origin is the opposite of Run's: no address rather than a
// root-relative path a pull-request reader cannot follow.
func TestRunFromEnvPublishesAnAbsolutePageOrNothing(t *testing.T) {
	const runID = "01a09ec6-b5b3-7660-8ef1-295031b7260d"

	t.Setenv(PublicBaseEnv, "https://iterion.cloud/")
	if got, want := RunFromEnv(runID), "https://iterion.cloud/studio/runs/"+runID; got != want {
		t.Errorf("RunFromEnv = %q, want %q", got, want)
	}
	if got, want := PublicBase(), "https://iterion.cloud"; got != want {
		t.Errorf("PublicBase = %q, want %q (trailing slash trimmed)", got, want)
	}

	// No origin configured (a local `iterion run`): the empty string says
	// "this deployment publishes no public address". A root-relative
	// "/studio/runs/<id>" posted on a forge resolves against the FORGE's
	// host, which is a link to someone else's 404.
	t.Setenv(PublicBaseEnv, "")
	if got := RunFromEnv(runID); got != "" {
		t.Errorf("RunFromEnv with no %s = %q, want the empty string", PublicBaseEnv, got)
	}
	t.Setenv(PublicBaseEnv, "   ")
	if got := RunFromEnv(runID); got != "" {
		t.Errorf("RunFromEnv with a blank %s = %q, want the empty string", PublicBaseEnv, got)
	}

	// No run id is the same refusal: a link to the run LIST would claim to
	// be the run's own page.
	t.Setenv(PublicBaseEnv, "https://iterion.cloud")
	if got := RunFromEnv("  "); got != "" {
		t.Errorf("RunFromEnv with no run id = %q, want the empty string", got)
	}
}

// An origin a stranger cannot follow is refused with a REASON, because the
// alternative is worse than nothing: a scheme-less value makes the published
// link relative (it resolves against the forge the comment sits on), a value
// with whitespace truncates the destination, and userinfo publishes the
// credential in a comment on someone else's repository.
func TestPublicOriginRefusesWhatAStrangerCannotFollow(t *testing.T) {
	const runID = "01a09ec6"
	for _, tc := range []struct{ name, env, wantIn string }{
		{"no scheme", "iterion.example", "no http(s) scheme"},
		{"protocol-relative", "//iterion.example", "no http(s) scheme"},
		{"not a url", "not a url at all", "whitespace"},
		{"inner newline", "https://a.example\nhttps://b.example", "whitespace"},
		{"userinfo", "https://admin:s3cr3t@iterion.example", "userinfo"},
		{"no host", "https://", "names no host"},
		// A no-break space survives PublicBase's TrimSpace only at the ends,
		// so an inner one reaches here — the realistic paste from a wiki or a
		// ticket. An ASCII-only check accepted it.
		{"no-break space", "https://a.example\u00a0b.example", "whitespace"},
		{"line separator", "https://a.example\u2028b.example", "whitespace"},
		// Studio() concatenates, so a query or fragment would swallow the
		// run's own path: right host, wrong page, no sign of it in the link.
		{"query", "https://iterion.example/?utm=x", "query or a fragment"},
		{"bare query", "https://iterion.example?utm=x", "query or a fragment"},
		{"fragment", "https://iterion.example#frag", "query or a fragment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(PublicBaseEnv, tc.env)
			base, err := PublicOrigin()
			if err == nil {
				t.Fatalf("PublicOrigin(%q) = %q, want a refusal", tc.env, base)
			}
			if !errors.Is(err, ErrPublicBaseUnusable) {
				t.Errorf("refusal is not typed: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("refusal %q does not say why (want %q)", err, tc.wantIn)
			}
			// What a bot would publish: nothing at all, so it prints the run
			// id alone — never a relative link, never a credential.
			if got := RunFromEnv(runID); got != "" {
				t.Errorf("RunFromEnv with %s=%q = %q, want the empty string", PublicBaseEnv, tc.env, got)
			}
		})
	}

	// A deployment served under a path prefix is legitimate, and the studio
	// sits under that prefix too.
	t.Setenv(PublicBaseEnv, "https://forge.example/iterion")
	if got, want := RunFromEnv(runID), "https://forge.example/iterion/studio/runs/"+runID; got != want {
		t.Errorf("RunFromEnv under a path prefix = %q, want %q", got, want)
	}

	// What is published is the form that was JUDGED, not the raw env value:
	// an accepted origin comes back parsed (the scheme normalised), so the
	// link a reader clicks is the one the predicate ruled on.
	t.Setenv(PublicBaseEnv, "HTTPS://ITERION.EXAMPLE/")
	base, err := PublicOrigin()
	if err != nil {
		t.Fatalf("PublicOrigin refused an uppercase scheme: %v", err)
	}
	if strings.HasPrefix(base, "HTTPS") {
		t.Errorf("PublicOrigin returned the raw value %q, not the parsed form it judged", base)
	}
	if got, want := RunFromEnv(runID), "https://ITERION.EXAMPLE/studio/runs/"+runID; got != want {
		t.Errorf("RunFromEnv with an uppercase scheme = %q, want %q", got, want)
	}

	// A port and a punycode host are ordinary, not findings.
	for _, ok := range []string{"https://iterion.example:4891", "https://xn--iterin-bva.example"} {
		t.Setenv(PublicBaseEnv, ok)
		if _, err := PublicOrigin(); err != nil {
			t.Errorf("PublicOrigin refused a legitimate origin %q: %v", ok, err)
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

// TestRunAttachmentAddressesTheRouteThatServesTheBytes pins the one
// spelling of a run attachment's download address, and the escaping a
// caller-chosen run id needs.
func TestRunAttachmentAddressesTheRouteThatServesTheBytes(t *testing.T) {
	cases := []struct {
		name        string
		base, runID string
		handle      string
		want        string
	}{
		{"plain", "https://iterion.example", "run-1", "report", "https://iterion.example/api/runs/run-1/attachments/report"},
		{"trailing slash on the base", "https://iterion.example/", "run-1", "report", "https://iterion.example/api/runs/run-1/attachments/report"},
		{"base served under a path prefix", "https://host/iterion", "run-1", "report", "https://host/iterion/api/runs/run-1/attachments/report"},
		// SanitizePathComponent rejects "..", a separator and control
		// characters — a space reaches this builder, and unescaped it
		// truncates the link at the first space wherever it is pasted.
		{"a run id holding a space", "https://iterion.example", "run 1", "report", "https://iterion.example/api/runs/run%201/attachments/report"},
		{"a handle holding a hash", "https://iterion.example", "run-1", "a#b", "https://iterion.example/api/runs/run-1/attachments/a%23b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RunAttachment(tc.base, tc.runID, tc.handle); got != tc.want {
				t.Errorf("RunAttachment(%q, %q, %q) = %q, want %q",
					tc.base, tc.runID, tc.handle, got, tc.want)
			}
		})
	}
}

// TestRunAttachmentFromEnvPublishesNothingItCannotStandBehind: the
// empty-string policy, one case per refusal. A link that resolves
// against the wrong origin is worse than no link, because nothing in it
// says so.
func TestRunAttachmentFromEnvPublishesNothingItCannotStandBehind(t *testing.T) {
	cases := []struct {
		name   string
		env    string
		runID  string
		handle string
		want   string
	}{
		{"usable origin", "https://iterion.example", "run-1", "report", "https://iterion.example/api/runs/run-1/attachments/report"},
		{"unset", "", "run-1", "report", ""},
		{"no scheme", "iterion.example", "run-1", "report", ""},
		{"whitespace", "https://iterion.example /x", "run-1", "report", ""},
		{"userinfo", "https://admin:pw@iterion.example", "run-1", "report", ""},
		{"query", "https://iterion.example?utm=x", "run-1", "report", ""},
		{"no run id", "https://iterion.example", "  ", "report", ""},
		{"no handle", "https://iterion.example", "run-1", "  ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(PublicBaseEnv, tc.env)
			if got := RunAttachmentFromEnv(tc.runID, tc.handle); got != tc.want {
				t.Errorf("RunAttachmentFromEnv(%q, %q) with %s=%q = %q, want %q",
					tc.runID, tc.handle, PublicBaseEnv, tc.env, got, tc.want)
			}
		})
	}
}
