package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

// A bundle and the engine that executes it move independently on a
// deployment: a push lands in a second, a runner image is pinned by digest and
// moves only on a deploy. A bundle that needs a builtin the runners' evaluator
// does not have COMPILES here (a function call parses generically) and dies at
// its first evaluation on the pod — which is what happened on 2026-09-06.
//
// A manifest may now declare the engine it needs (`requires.iterion`), and
// this file is what holds a push against the deployment's actual build.

// runnerBuildObserver is the store capability that answers "what build is
// actually EXECUTING runs here" — implemented by the Mongo store over the
// `runner_version` each runner stamps on the runs it takes. Absent in local
// mode, where server and engine are one process and appinfo answers already.
type runnerBuildObserver interface {
	ObservedRunnerBuilds(ctx context.Context, since time.Time, limit int) ([]string, error)
}

// engineFloorLookback bounds how far back a runner build counts as evidence
// about TODAY's fleet. Long enough that a quiet weekend still reports, short
// enough that an image retired last month does not veto a push.
const engineFloorLookback = 7 * 24 * time.Hour

// engineFloorSample caps the runs read per resolution.
const engineFloorSample = 200

// serverBuild names this process's own build. A var so a test can pin it:
// `go test` binaries carry appinfo.Version = "dev", which is deliberately
// unorderable, so the floor would otherwise be permanently inconclusive under
// test and every refusal assertion would pass for the wrong reason.
var serverBuild = appinfo.FullVersion

// engineFloor is the build a bundle must satisfy to run anywhere on this
// deployment: the MINIMUM over this server's own build and every runner build
// observed recently.
//
// Both halves matter, and in both directions. The server COMPILES the bot at
// launch, so a server below the requirement blocks just as hard as an old
// runner; the runners EVALUATE it, and on cloud they are the half that lags
// (`:edge` server, digest-pinned runners). The minimum is the honest answer
// because a queued run lands on whichever pod takes it.
//
// Returns the floor build plus the sources it was taken from, so a refusal can
// say where its number came from. A build no component can order (a `dev`
// server, a fork's naming scheme) is returned AS the floor rather than skipped:
// dropping it would let an orderable peer answer for a fleet that also carries
// something nobody can compare, and the caller reports "could not check"
// instead of passing.
func (s *Server) engineFloor(ctx context.Context) (build string, sources []string) {
	self := serverBuild()
	build, sources = self, []string{"server " + self}
	if s.runnerBuilds == nil {
		return build, sources
	}
	observed, err := s.runnerBuilds.ObservedRunnerBuilds(ctx, time.Now().UTC().Add(-engineFloorLookback), engineFloorSample)
	if err != nil {
		// Observational: a floor that could not read the fleet is still the
		// server's own build, and the log says the fleet half is missing.
		s.logWarn("engine floor: could not read the runner builds (%v) — the requirement check sees only this server's build %s", err, self)
		return build, sources
	}
	for _, v := range observed {
		if lower(v, build) {
			build, sources = v, []string{"runner " + v}
		}
	}
	if len(observed) > 0 && build == self {
		sources = append(sources, fmt.Sprintf("%d runner build(s) observed, none older", len(observed)))
	}
	return build, sources
}

// lower reports whether build a must replace the current floor b. An
// UNORDERABLE build always wins: it is the participant the comparison cannot
// clear, so it must be the answer the caller reports as unchecked.
func lower(a, b string) bool {
	if !orderableBuild(a) {
		return true
	}
	if !orderableBuild(b) {
		return false
	}
	c, _ := bundle.CompareVersions(a, b)
	return c < 0
}

// orderableBuild reports whether a build string can be ordered at all. It
// delegates to pkg/bundle so the grammar a manifest is validated against and
// the grammar a build is measured with cannot drift apart.
func orderableBuild(v string) bool {
	_, ok := bundle.CompareVersions(v, v)
	return ok
}

// guardBundleEngineRequirement holds a bundle about to be STORED against the
// deployment's engine floor. It returns "" when the push may proceed, or the
// warning it must carry when force was passed; a refusal is written to w and
// signalled by ok=false.
//
// Refuse rather than warn, deliberately, on the one path where warning was
// already tried: `docs/platform-bots.md` documents the two-halves rule in
// prose, and a production push broke it anyway. The escape hatch is explicit
// (`--force` / `?force=1`) and never silent — the forced push carries the
// overridden requirement in its response warnings and in the audit trail the
// write already produces.
func (s *Server) guardBundleEngineRequirement(w http.ResponseWriter, r *http.Request, bs botsource.BotSource) (warning string, ok bool) {
	m := bs.Manifest()
	if m == nil || m.Requires == nil || strings.TrimSpace(m.Requires.Iterion) == "" {
		// No floor declared. A bundle written in a syntax profile above 1
		// needs one: the main workflow reaches a runner as an AST, but a
		// subbot child is re-parsed as text by the runner's own binary, and
		// a build older than the profile fails at that parse — after
		// admission, on a pod. Refused here instead, unless forced.
		if profile, by := bundle.MaxSyntaxProfile(bs.Files); profile >= 2 {
			if forceRequested(r) {
				return fmt.Sprintf("FORCED past the profile-floor guard: %q is written in dsl profile %d (%s) and declares no requires.iterion — a runner older than the profile will fail at its first parse of a child",
					bs.Slug, profile, strings.Join(by, ", ")), true
			}
			s.httpErrorFor(w, r, http.StatusConflict,
				"bot %q is written in dsl profile %d (%s) but its manifest declares no requires.iterion: declare `requires: { iterion: \">= <the release that reads the profile>\" }` (`iterion dsl migrate` writes it), or push anyway with --force",
				bs.Slug, profile, strings.Join(by, ", "))
			return "", false
		}
		return "", true
	}
	floor, sources := s.engineFloor(r.Context())
	verdict, reason := bundle.CheckManifestEngine(m, floor)
	forced := forceRequested(r)
	switch verdict {
	case bundle.EngineOK:
		return "", true
	case bundle.EngineUnknown:
		// Never a silent pass: the operator learns the check did not run.
		return fmt.Sprintf("%s (floor from %s) — the engine requirement could not be checked, so this push is unverified",
			reason, strings.Join(sources, ", ")), true
	}
	if forced {
		return fmt.Sprintf("FORCED past the engine-requirement guard: %s (floor from %s) — a launch of %q will be refused on any pod below the requirement",
			reason, strings.Join(sources, ", "), bs.Slug), true
	}
	s.httpErrorFor(w, r, http.StatusConflict,
		"bot %q: %s (floor from %s). Bump the runner image and restart the server first (docs/platform-bots.md § Shipping a baked-catalog change), or push anyway with --force",
		bs.Slug, reason, strings.Join(sources, ", "))
	return "", false
}

// forceRequested reads the explicit override off the request.
func forceRequested(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("force"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}
