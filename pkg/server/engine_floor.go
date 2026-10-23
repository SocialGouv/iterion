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
// deployment's engine floor. It returns the warnings the guard's forces
// carry — none when the push may proceed unremarked, one per guard forced
// past — or writes a refusal to w and signals it by ok=false. stored is the
// bundle as the write read it at the version it is pinned to (nil for a
// creation): the guard judges the RESULTING state, and its requires half
// reads the manifest OF THAT STATE — the write's when it lands one, and when
// the write DELETES the manifest (a whole-bundle push or a per-file DELETE
// that omits manifest.yaml), the requirement the stored manifest declared
// dies with the file and is checked by nothing afterwards — that deletion is
// refused like a floor the manifest does not reach, not passed in silence. A
// manifest the write REPLACES is judged by the text it lands (a knowing
// edit, held by the digest rule); only the deletion is the silent shed.
//
// Refuse rather than warn, deliberately, on the paths where warning was
// already tried: `docs/platform-bots.md` documents the two-halves rule in
// prose, and a production push broke it anyway. The escape hatch is explicit
// (`--force` / `?force=1`) and never silent — the forced push carries every
// overridden requirement in its response warnings and in the audit trail the
// write already produces. Forcing past one guard checks the other still: a
// push forced past a source the reader cut short is still told the engine
// requirement its manifest declares.
func (s *Server) guardBundleEngineRequirement(w http.ResponseWriter, r *http.Request, bs botsource.BotSource, stored *botsource.BotSource) (warnings []string, ok bool) {
	forced := forceRequested(r)
	if gap := bundleSyntaxFloorGap(bs); gap != nil {
		if !forced {
			s.httpErrorFor(w, r, http.StatusConflict, "%s, or push anyway with --force", gap.refusal(bs.Slug))
			return nil, false
		}
		warnings = append(warnings, gap.forced(bs.Slug))
	}
	out := s.requiresFloorGap(r.Context(), bs, stored, forced)
	if out.refusal != "" {
		s.httpErrorFor(w, r, http.StatusConflict, "%s", out.refusal)
		return nil, false
	}
	return append(warnings, out.warnings...), true
}

// requiresFloorOutcome is what the requires half of the engine-requirement
// guard concludes: pass (warnings carries what a forced guard or an
// unchecked floor would have told the operator), or refusal (non-empty: the
// write is refused with this reason).
type requiresFloorOutcome struct {
	warnings []string
	refusal  string
}

// requiresFloorGap is the REQUIRES half of guardBundleEngineRequirement,
// judged on the bundle as the write leaves it, shared by the push routes and
// the assistant's authoring commit. A declared requires.iterion above this
// deployment's engine floor is refused; forced past on the paths that carry
// the override, with the FORCED warning. No requirement in the resulting
// state, but a write that DELETES the manifest of the stored bundle, sheds
// the requirement the stored one declared — refused the same way (stored
// nil on the paths whose change shape cannot delete the manifest, the
// assistant commit among them: replace-only, create refused for cloud, so
// the manifest never leaves the file set). A floor the comparison cannot
// order is EngineUnknown: passed, carrying the warning that the push went
// unverified.
func (s *Server) requiresFloorGap(ctx context.Context, bs botsource.BotSource, stored *botsource.BotSource, forced bool) requiresFloorOutcome {
	m := bs.Manifest()
	if m == nil || m.Requires == nil || strings.TrimSpace(m.Requires.Iterion) == "" {
		// No requirement in the resulting state — but a write that DELETES
		// the manifest sheds the requirement the stored one declared. The
		// same refusal a too-low declaration meets, same escape hatch.
		if m == nil && stored != nil {
			if declared, need := declaredEngineRequirement(stored); declared {
				refusal := fmt.Sprintf("bot %q: this write deletes the manifest that declared requires.iterion %q — the requirement dies with the file and a launch below it is checked by nothing: push the bundle with its manifest, or push anyway with --force", bs.Slug, need)
				if forced {
					return requiresFloorOutcome{warnings: []string{"FORCED past the engine-requirement guard: " + refusal}}
				}
				return requiresFloorOutcome{refusal: refusal}
			}
		}
		return requiresFloorOutcome{}
	}
	floor, sources := s.engineFloor(ctx)
	verdict, reason := bundle.CheckManifestEngine(m, floor)
	switch verdict {
	case bundle.EngineOK:
		return requiresFloorOutcome{warnings: nil}
	case bundle.EngineUnknown:
		// Never a silent pass: the operator learns the check did not run.
		return requiresFloorOutcome{warnings: []string{fmt.Sprintf("%s (floor from %s) — the engine requirement could not be checked, so this push is unverified",
			reason, strings.Join(sources, ", "))}}
	}
	if forced {
		return requiresFloorOutcome{warnings: []string{fmt.Sprintf("FORCED past the engine-requirement guard: %s (floor from %s) — a launch of %q will be refused on any pod below the requirement",
			reason, strings.Join(sources, ", "), bs.Slug)}}
	}
	return requiresFloorOutcome{refusal: fmt.Sprintf("bot %q: %s (floor from %s). Bump the runner image and restart the server first (docs/platform-bots.md § Shipping a baked-catalog change), or push anyway with --force",
		bs.Slug, reason, strings.Join(sources, ", "))}
}

// syntaxFloorGap is what a bundle's sources use that its manifest's floor
// does not reach: the shape every write of a bundle refuses on (the push
// routes, the assistant's authoring commit), one predicate for all of them.
type syntaxFloorGap struct {
	uses string // what the sources use: "`contract` (main.bot)"
	gap  string // what the manifest declares, or does not
	need string // the release the floor must reach
	// short names the sources a bound of the reader cut short, each with
	// its refusal (bundle.SyntaxRequirements.Short): what they use is not
	// known, so no declared floor is checked against them.
	short string
}

// forced is the warning a push forced past the gap carries: the sources a
// bound of the reader cut short, each with its refusal and remedy, when that
// is the gap — what they need was not checked, and every reader held to the
// same budget refuses them — or the syntax the floor does not reach.
func (g *syntaxFloorGap) forced(slug string) string {
	if g.short != "" {
		return fmt.Sprintf("FORCED past the syntax-floor guard: %q has executable sources the server does not read whole, so the release they need was not checked (C256) — and every reader held to this read budget refuses them: %s", slug, g.short)
	}
	return fmt.Sprintf("FORCED past the syntax-floor guard: %q uses %s and %s — a runner older than the release that reads it will fail at its first parse of a child, a fragment or the contract",
		slug, g.uses, g.gap)
}

// refusal is the sentence a write is refused with.
func (g *syntaxFloorGap) refusal(slug string) string {
	if g.short != "" {
		return fmt.Sprintf("bot %q has executable sources the server does not read whole, so the release they need cannot be told (C256): %s", slug, g.short)
	}
	return fmt.Sprintf("bot %q uses %s but its manifest %s: declare `requires: { iterion: \">= %s\" }` (`iterion dsl migrate` writes it for a profile)", slug, g.uses, g.gap, g.need)
}

// bundleSyntaxFloorGap holds a bundle about to be WRITTEN to what its
// sources use. A bundle written in a syntax profile above 1, in several
// files whose fragments a runner parses as text, or declaring a contract,
// needs a floor at or above the release that reads it
// (bundle.CheckSyntaxFloor, shared with `validate`'s C252;
// bundle.RequiredRelease is the one list of what asks): a build older than
// the syntax fails at its first parse of a child, a fragment or the
// contract — after admission, on a pod. A floor declared but lower admits
// exactly those builds. nil when the floor reads what the sources use.
func bundleSyntaxFloorGap(bs botsource.BotSource) *syntaxFloorGap {
	syntax := bundle.MaxSyntaxRequirements(bs.Files)
	if len(syntax.Short) > 0 {
		// A source the walk could not read whole uses what nobody read: a
		// floor that admits it is certified by nothing. The files are the
		// bundle's own names, the refusal its remedy.
		return &syntaxFloorGap{uses: "sources not read whole", gap: "cannot be checked", short: strings.Join(syntax.Short, "; ")}
	}
	if !syntax.Asks() {
		return nil
	}
	pf := bundle.CheckSyntaxFloor(bs.Manifest(), syntax)
	if pf.OK {
		return nil
	}
	need := pf.Need
	if need == "" {
		need = "<the release that reads " + pf.Reason + ">"
	}
	gap := "declares no requires.iterion"
	if pf.Declared != "" {
		gap = fmt.Sprintf("declares requires.iterion %q, below %s (the release that reads it)", pf.Declared, need)
	}
	return &syntaxFloorGap{uses: syntax.Describe(), gap: gap, need: need}
}

// declaredEngineRequirement reports whether a stored bundle's manifest
// declares `requires.iterion`, and what it asks: the requirement a write
// that deletes the manifest would shed.
func declaredEngineRequirement(bs *botsource.BotSource) (bool, string) {
	if bs == nil {
		return false, ""
	}
	m := bs.Manifest()
	if m == nil || m.Requires == nil {
		return false, ""
	}
	need := strings.TrimSpace(m.Requires.Iterion)
	return need != "", need
}

// forceRequested reads the explicit override off the request.
func forceRequested(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("force"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}
