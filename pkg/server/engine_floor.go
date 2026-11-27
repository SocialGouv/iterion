package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// A bundle and the engine that executes it move independently on a
// deployment: a push lands in a second, a runner image is pinned by digest and
// moves only on a deploy. A bundle that needs a builtin the runners' evaluator
// does not have COMPILES here (a function call parses generically) and dies at
// its first evaluation on the pod — which is what happened on 2026-09-06.
//
// A manifest may now declare the engine it needs (`requires.iterion`), and
// this file is what holds a push against the deployment's actual build.

// FleetRegistry is the live fleet the engine floor reads: one presence record
// per server and runner process (natsq.Conn.FleetMembers), kept fresh by each
// process's heartbeat and expired by the registry TTL when a process dies.
type FleetRegistry interface {
	FleetMembers(ctx context.Context) ([]queue.FleetMember, error)
}

// serverBuild names this process's own build. A var so a test can pin it:
// `go test` binaries carry appinfo.Version = "dev", which is deliberately
// unorderable, so the floor would otherwise be permanently inconclusive under
// test and every refusal assertion would pass for the wrong reason.
var serverBuild = appinfo.FullVersion

// engineFloorReading is what the floor resolution concludes.
type engineFloorReading struct {
	// build is the floor: the lowest build among the participants.
	build string
	// sources says where the floor's number came from, and what the
	// registry held that did not count — a refusal quotes it.
	sources []string
	// unchecked names a half of the deployment the floor could not see. A
	// push that passes says so: its requirement was checked against part of
	// the deployment only, never silently against all of it.
	unchecked []string
}

// engineFloor is the build a bundle must satisfy to run on any process that
// can take a run launched NOW — the minimum over:
//
//   - this server's own build, always;
//   - every live server of the fleet registry: a server COMPILES the bot at
//     launch, and one in its lame-duck window still answers the requests
//     routed to it;
//   - every live runner that can take a new launch: not draining (a draining
//     runner has stopped fetching) and at or above the lowest live server's
//     epoch (a runner below a publisher's epoch refuses that publisher's
//     every message — the queue/nats rollout fence).
//
// The registry, not the runs of the past week: what the guard protects is
// the next launch, so a pod finishing its last run, or gone since, must not
// refuse a push the processes that will execute it can run.
//
// A build no component can order (a `dev` server, a fork's naming scheme) is
// returned AS the floor rather than skipped: dropping it would let an
// orderable peer answer for a fleet that also carries something nobody can
// compare, and the caller reports "could not check" instead of passing.
func (s *Server) engineFloor(ctx context.Context) engineFloorReading {
	self := serverBuild()
	if s.fleet == nil {
		return engineFloorReading{build: self, sources: []string{"this server " + self}}
	}
	members, err := s.fleet.FleetMembers(ctx)
	if err != nil {
		s.logWarn("engine floor: could not read the fleet registry (%v) — the requirement check sees only this server's build %s", err, self)
		return engineFloorReading{
			build:     self,
			sources:   []string{"this server " + self},
			unchecked: []string{fmt.Sprintf("the fleet registry could not be read (%v), so no runner was checked", err)},
		}
	}
	return floorOverFleet(self, s.cfg.RunnerEpoch, members)
}

// floorParticipant is one process whose build the floor counts.
type floorParticipant struct {
	label string
	build string
}

// floorOverFleet applies the floor rule to one registry snapshot. selfEpoch
// is this server's own epoch: it publishes, whatever the registry holds of
// it. A record of a role this build does not know is counted — a
// participant nobody understands is not dropped from the minimum.
func floorOverFleet(self string, selfEpoch uint64, members []queue.FleetMember) engineFloorReading {
	publisherEpoch := selfEpoch
	for _, m := range members {
		if m.Role == queue.FleetRoleServer && m.Epoch < publisherEpoch {
			publisherEpoch = m.Epoch
		}
	}
	participants := []floorParticipant{{label: "this server", build: self}}
	var draining, fenced []string
	runners := 0
	for _, m := range members {
		who := m.Role + " " + m.Instance
		switch {
		case m.Role == queue.FleetRoleRunner && m.Draining:
			draining = append(draining, who+" "+m.Build)
		case m.Role == queue.FleetRoleRunner && m.Epoch < publisherEpoch:
			fenced = append(fenced, fmt.Sprintf("%s %s (epoch %d)", who, m.Build, m.Epoch))
		default:
			if m.Role == queue.FleetRoleRunner {
				runners++
			}
			participants = append(participants, floorParticipant{label: who, build: m.Build})
		}
	}

	rd := engineFloorReading{build: self}
	for _, p := range participants {
		if lower(p.build, rd.build) {
			rd.build = p.build
		}
	}
	// The holders are every participant the ordering ranks equal to the
	// floor — another commit of the same version holds it as much — and,
	// for a floor nothing can order, every participant nothing can order:
	// each one is a reason the check cannot run.
	unorderedFloor := !orderableBuild(rd.build)
	var holders []string
	for _, p := range participants {
		if (unorderedFloor && !orderableBuild(p.build)) || (!unorderedFloor && sameBuildRank(p.build, rd.build)) {
			holders = append(holders, p.label)
		}
	}
	rd.sources = []string{
		fmt.Sprintf("%s held by %s", rd.build, joinCapped(holders, 3)),
		fmt.Sprintf("%d live runner(s) can take a new launch", runners),
	}
	if len(draining) > 0 {
		rd.sources = append(rd.sources, "not counted, draining: "+joinCapped(draining, 3))
	}
	if len(fenced) > 0 {
		rd.sources = append(rd.sources, fmt.Sprintf("not counted, below the publishers' epoch %d: %s", publisherEpoch, joinCapped(fenced, 3)))
	}
	if runners == 0 {
		rd.unchecked = append(rd.unchecked, "no live runner can take a new launch (none registered, or every one draining or below the publishers' epoch), so no runner was checked")
	}
	return rd
}

// uncheckedWarnings is what a push carries when part of the deployment went
// unseen: one warning per gap, naming the requirement. verdict is what the
// requirement got from the part that was seen — "passed" on a pass, "went
// unverified" when the floor could not be ordered.
func (rd engineFloorReading) uncheckedWarnings(requirement, verdict string) []string {
	var out []string
	for _, gap := range rd.unchecked {
		out = append(out, fmt.Sprintf("requires.iterion %q %s, and besides %s", strings.TrimSpace(requirement), verdict, gap))
	}
	return out
}

// sameBuildRank reports whether two builds rank equal for the floor: equal
// by bundle.CompareVersions when both order, else the same string.
func sameBuildRank(a, b string) bool {
	if c, ok := bundle.CompareVersions(a, b); ok {
		return c == 0
	}
	return a == b
}

// joinCapped joins the first n items and counts the rest.
func joinCapped(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(items[:n], ", "), len(items)-n)
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
	floor := s.engineFloor(ctx)
	sources := strings.Join(floor.sources, "; ")
	verdict, reason := bundle.CheckManifestEngine(m, floor.build)
	switch verdict {
	case bundle.EngineOK:
		// A pass over part of the deployment says which part it missed.
		return requiresFloorOutcome{warnings: floor.uncheckedWarnings(m.Requires.Iterion, "passed")}
	case bundle.EngineUnknown:
		// Never a silent pass: the operator learns the check did not run,
		// and which half of the deployment went unseen besides.
		return requiresFloorOutcome{warnings: append([]string{fmt.Sprintf("%s (floor from %s) — the engine requirement could not be checked, so this push is unverified",
			reason, sources)}, floor.uncheckedWarnings(m.Requires.Iterion, "went unverified")...)}
	}
	if forced {
		return requiresFloorOutcome{warnings: []string{fmt.Sprintf("FORCED past the engine-requirement guard: %s (floor from %s) — a launch of %q will be refused on any pod below the requirement",
			reason, sources, bs.Slug)}}
	}
	return requiresFloorOutcome{refusal: fmt.Sprintf("bot %q: %s (floor from %s). Roll the runners and servers that hold the floor first (docs/platform-bots.md § Shipping a baked-catalog change), or push anyway with --force",
		bs.Slug, reason, sources)}
}

// syntaxFloorGap is what a bundle's sources use that its manifest's floor
// does not reach: the shape every write of a bundle refuses on (the push
// routes, the assistant's authoring commit), one predicate for all of them.
type syntaxFloorGap struct {
	uses string // what the bundle asks for: "`contract` (main.bot)"
	gap  string // what the manifest declares, or does not
	need string // the release the floor must reach
	// consequence is what a build below the floor does with the bundle,
	// from the floors table entry that decided it (bundle.ProfileFloor):
	// a forced push says it, and it is never a sentence this package
	// guesses for a floor it does not know.
	consequence string
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
	return fmt.Sprintf("FORCED past the syntax-floor guard: %q uses %s and %s — %s",
		slug, g.uses, g.gap, g.consequence)
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
	// What asks for the floor is the whole subject — the sources AND the
	// keys the manifest declares (bundle.FloorSubject) — so the verdict is
	// what answers "nothing asks" here: an early exit on the sources alone
	// would admit a manifest key's floor unchecked.
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
	return &syntaxFloorGap{uses: pf.Uses, gap: gap, need: need, consequence: pf.Consequence}
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
