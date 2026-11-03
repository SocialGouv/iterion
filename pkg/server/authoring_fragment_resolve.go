package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// authoringFragments is one request's licence to resolve a bundle's declared
// `dependencies.fragments` on the AUTHORING plane — the cloud editor's open,
// its per-file apply, its write-back and the bot-source write gate — through
// the producer chokepoint the launch resolves through: resolveBotTieredRaw
// via snapshotProducers.dir, botinstall.ExportedFragmentSource,
// botinstall.RefuseFragmentShadow. ADR-145 clause 4's rule is explicit — "a
// rule recopied for the second plane is the rule that drifts" — so the third
// plane reuses it rather than keeping a producer rule of its own.
//
// The zero value resolves NOTHING, and every surface holding it keeps the
// naming refusal #2865's first half landed: a request that names no cloud
// bot, or a caller that does not hold the team the path names, reads no tier
// at all.
//
// teamID is a field rather than a ctx read for the reason
// exportedFragmentForSnapshot states: a tier resolved off whichever tenant
// happened to be on the ctx answers a different question, and here it would
// compile one team's bot against another team's logic.
type authoringFragments struct {
	s      *Server
	teamID string
	// gate is true when this resolution serves a WRITE. ADR-145 clause 2's
	// no-shadow verdict is READ on both planes, over every DECLARED entry;
	// what gate decides is the consequence. A WRITE refuses: a gate that
	// stored a shadowing bundle would bless one no launch takes. A READ
	// opens anyway — a bundle already in that state must still OPEN, or
	// its author has no way to the screen where they fix it, which is
	// #2865's first half — and leaves the shadowed entry unplaced, so the
	// document is built on the author's OWN file and the editor still
	// offers the edit that repairs it.
	gate bool
	// cloud is true when the request names a bot of the cloud's own store,
	// whatever right the caller holds: the PLANE decides the remedy a
	// surviving miss names — publish, never a working tree the server has
	// not got — and a caller the resolution refuses must still read the
	// sentence of the plane they are on.
	cloud bool
}

// authoringFragmentsForWrite is the resolution a WRITE of a route that has
// ALREADY authorized the caller on teamID may perform — what botSourceCtx
// answers before any bot-source route body runs. Its gate holds the launch's
// no-shadow verdict; a read uses authoringFragmentsFor.
func (s *Server) authoringFragmentsForWrite(teamID string) authoringFragments {
	if s == nil || teamID == "" {
		// No tier to read, and the gate's no-shadow verdict needs none:
		// it reads the caller's own files and their record.
		return authoringFragments{gate: true, cloud: true}
	}
	return authoringFragments{s: s, teamID: teamID, gate: true, cloud: true}
}

// authoringFragmentsFor is the resolution a DSL route may perform for the
// cloud bot its editor path names: `botsource://<team>/<slug>/<rel>`, the
// virtual path the studio already carries for every cloud bot it opens, so
// this costs the API no new field.
//
// The caller must hold the team twice over: the identity's own team must BE
// the path's — the check s.mergeBotSourcePrompts holds a stored-bundle read
// to, on this very plane — and the caller must hold the right the bot-source
// routes require of a reader of a team's bots (canEditBots, what
// botSourceCtx answers), since what a resolution reads IS a team's stored
// bundle. Without both, no tier is read: /api/parse takes a caller-supplied
// files map, so a resolution without this check would turn the route into a
// cross-team read primitive — the producer's fragment text lands inside the
// merged document the response echoes.
func (s *Server) authoringFragmentsFor(r *http.Request, editorPath string) authoringFragments {
	if !strings.HasPrefix(editorPath, botSourceScheme) {
		return authoringFragments{}
	}
	// From here the plane is known — the path names a bot of the cloud
	// store — so every refusal below still answers the cloud's remedy.
	cloud := authoringFragments{cloud: true}
	if s == nil || r == nil {
		return cloud
	}
	parts := strings.SplitN(strings.TrimPrefix(editorPath, botSourceScheme), "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return cloud
	}
	id, ok := auth.FromContext(r.Context())
	if !ok || id.TeamID != parts[0] {
		return cloud
	}
	if !s.canEditBots(r.Context(), id, parts[0]) {
		return cloud
	}
	return authoringFragments{s: s, teamID: parts[0], cloud: true}
}

// ok reports whether this resolution may read a tier at all.
func (a authoringFragments) ok() bool { return a.s != nil && a.teamID != "" }

// declaredFragments reads the bundle's declarations with THIS plane's remedy
// already on them. The plane is known before any reading happens, so no pass
// of a cloud gate can quote the working tree's remedy — the one chokepoint
// for "which way forward does a miss name", rather than a stamp each caller
// remembers to apply to the pass that ends up being answered.
//
// The cloud plane's remedy depends on whether this resolution may read a
// tier at all (ok): "publish it" is a verdict about the producer, and a
// request that searched no tier — a caller without the team's bot
// authoring, a path naming no bot of the store — has not earned it. Decided
// HERE, at the one chokepoint, rather than stamped on each early return of
// authoringFragmentsFor, where the next return added would forget it.
func (a authoringFragments) declaredFragments(files map[string]string) fragmentDependencies {
	fd := declaredFragments(files, "")
	if !a.cloud {
		return fd
	}
	if !a.ok() {
		return fd.withRemedy(remedyUnchecked)
	}
	return fd.withRemedy(remedyPublish)
}

// fragmentResolution is what one resolution of a bundle's declared
// dependencies.fragments did.
type fragmentResolution struct {
	// files is the bundle's files with every dependency the resolution
	// PLACED in it, keyed the way the bundle keys them. It is the caller's
	// own map when nothing was placed, so the common case copies nothing.
	files map[string]string
	// placed names each dependency this resolution put into files, by the
	// path it materializes at. They are files of the UNIT and of no save: a
	// persisted lib/<name>.bot is the shadow the launch refuses
	// (botinstall.RefuseFragmentShadow), which is the second horn of #2865.
	placed map[string]bundle.FragmentDependency
	// deps is what every refusal of this reading says from here on. A miss
	// that SURVIVED a resolution carries what the resolution of THAT entry
	// found, never a remedy stamped over the whole declaration set.
	deps fragmentDependencies
	// resolved is true when this resolution went to the tiers. It travels
	// with the result rather than being re-derived by the caller from
	// len(missing): the caller's FIRST reading's diagnostics are stale once
	// it is true — they were drawn before the dependencies were placed and
	// before the resolution knew why the rest are missing — and a caller
	// that answered them would answer the wrong sentence.
	resolved bool
}

// resolve answers files with every declared dependency in missing resolved
// into it — LAZILY, which is the whole cost argument: the caller loads or
// compiles FIRST and passes what that reading actually missed
// (fragmentDependencies.missed), so a bundle with no declared miss reads no
// tier, materializes no producer and copies no map. One resolution per
// request, not one per keystroke-save.
//
// A declared path the bundle itself SHIPS is not resolved either: the
// author's own file stands there, and replacing it would build the document
// on another bundle's content (the write gate refuses that bundle, which is
// where the verdict belongs).
//
// A producer that resolves NOWHERE is not an error: the dependency stays
// unplaced, the reading keeps its own `no such fragment`, and the refusal
// names the dependency with the server's remedy — the typed refusal #2865's
// naming half landed, now pointed at the case that is still a miss. A tier
// that FAILS is an error: a store blip must surface, never read as "the
// producer is nowhere", which would retire a bundle that resolves.
func (a authoringFragments) resolve(ctx context.Context, files map[string]string, fd fragmentDependencies, missing map[string]bundle.FragmentDependency) (fragmentResolution, error) {
	if len(fd.declared) == 0 {
		return fragmentResolution{files: files, deps: fd}, nil
	}
	// A bundle that SHIPS the path a dependency materializes at is ADR-145
	// clause 2's shadow, and the launch refuses it. The verdict is read on
	// BOTH planes, over every DECLARED entry — a shipped one is exactly the
	// entry a miss cannot report, so a verdict guarded by "something is
	// missing" is guarded by something it is not about — and in a stable
	// order, so one bundle draws one sentence on every run. What differs is
	// the consequence: a WRITE refuses, a READ leaves that entry unplaced
	// (the loop below) and keeps the author's own bytes. It reads no tier:
	// only the caller's own files and their record.
	shadowed, recErr := a.shadows(files, fd)
	if a.gate {
		// A record nobody can read is no state to store on: the gate that
		// wrote on a guess is how a shadow lands.
		if recErr != nil {
			return fragmentResolution{}, fragmentResolveError{recErr}
		}
		if len(shadowed.rels) > 0 {
			return fragmentResolution{}, fragmentResolveError{shadowed}
		}
	}
	if len(missing) == 0 || !a.ok() {
		return fragmentResolution{files: files, deps: fd}, nil
	}
	// The producer's identity is fixed once for the whole resolution, the
	// way a freeze fixes it: two dependencies on one bundle are one store
	// round trip, and the materialization is released with the request.
	producers := newSnapshotProducers()
	defer producers.release()
	out := files
	placed := make(map[string]bundle.FragmentDependency, len(fd.declared))
	var why map[string]string
	// EVERY declared entry, not only the ones this reading missed: the
	// launch's own shape (resolveSnapshotFragments loops the manifest), and
	// the reason a declared fragment that itself imports a second declared
	// fragment resolves here as it does there. Resolving only the first
	// reading's misses would refuse, one level deeper, exactly the bundle
	// #2865 is about. The trigger stays lazy — nothing missing above and no
	// tier is read at all.
	for _, rel := range sortedFragmentRels(fd.declared) {
		dep := fd.declared[rel]
		if reason, ok := shadowed.reasons[rel]; ok {
			// The BUNDLE holds that path, so the file there is the
			// author's own: placing the producer's bytes over it would
			// build the document on another bundle's content and answer
			// the author's file as a dependency's — which the editor
			// renders read-only, locking them out of the one file they
			// have to drop. The entry stays unplaced, its reason the
			// launch's own no-shadow verdict, and the verdict itself is
			// the gate's on the next save.
			if why == nil {
				why = make(map[string]string, len(fd.declared))
			}
			why[rel] = reason
			continue
		}
		content, found, unserved, err := a.exportedFragment(ctx, producers, dep)
		if err != nil {
			return fragmentResolution{}, fragmentResolveError{err}
		}
		if !found {
			// The entry is a miss that survived a resolution, and what the
			// resolution found is what its refusal must say. The reading
			// keeps its own `no such fragment`: the verdict is the gate's.
			if why == nil {
				why = make(map[string]string, len(fd.declared))
			}
			why[rel] = unresolvedReason(dep, unserved)
			continue
		}
		if len(placed) == 0 {
			out = cloneAuthoringStringMap(files)
		}
		out[rel] = string(content)
		placed[rel] = dep
	}
	if len(placed) == 0 {
		placed = nil
	}
	return fragmentResolution{files: out, placed: placed, deps: fd.withUnresolved(why), resolved: true}, nil
}

// fragmentShadowRefusal is ADR-145 clause 2's verdict over a bundle's
// declared entries: every path this bundle SHIPS that a declared
// `dependencies.fragments` entry materializes at, with the launch's own
// sentence for each (botinstall.RefuseFragmentShadow).
//
// It carries EVERY violating path, not the first one walked. A caller that
// scopes a refusal to what its own edit introduced compares the two states,
// and a refusal naming one of several would make that comparison read the
// walk ORDER instead of the states — refusing a delete that REMOVED a
// shadow, and naming a file the request never touched.
type fragmentShadowRefusal struct {
	// rels is sorted, so one bundle draws one sentence whatever order a
	// map handed its entries over in.
	rels    []string
	reasons map[string]string
}

func (e fragmentShadowRefusal) Error() string {
	msgs := make([]string, 0, len(e.rels))
	for _, rel := range e.rels {
		msgs = append(msgs, e.reasons[rel])
	}
	return strings.Join(msgs, "; ")
}

// covers reports whether every path in rels is one this refusal already
// names — "a failure the bundle already drew is not this edit's", answered
// by PATH and not by sentence.
func (e fragmentShadowRefusal) covers(rels []string) bool {
	for _, rel := range rels {
		if _, ok := e.reasons[rel]; !ok {
			return false
		}
	}
	return true
}

// shadows holds ADR-145 clause 2 over every declared entry of the bundle a
// plane holds, through the launch's own verdict
// (botinstall.RefuseFragmentShadow). It reads no tier: the caller's own
// files and their record of what a previous materialization wrote.
//
// The record's own decode failure travels beside the verdict rather than
// replacing it: a WRITE refuses on it, and a READ — which must still OPEN
// the bundle — treats every shipped declared path as a shadow, since
// without the record nothing tells an install's output from a file the
// bundle ships, and says so in that entry's reason.
func (a authoringFragments) shadows(files map[string]string, fd fragmentDependencies) (fragmentShadowRefusal, error) {
	recorded, recErr := fragmentLocalStateRecord(files)
	var out fragmentShadowRefusal
	for _, rel := range sortedFragmentRels(fd.declared) {
		dep := fd.declared[rel]
		content, present := files[rel]
		current := ""
		if present {
			current = bundle.SHA256Bytes([]byte(content))
		}
		err := botinstall.RefuseFragmentShadow(rel, current, present, dep, recorded)
		if err == nil {
			continue
		}
		msg := err.Error()
		if recErr != nil {
			// The record is WHY this path reads as a shadow, and an
			// author shown the shadow sentence alone would hunt for a
			// file to drop instead of a record to repair.
			msg += " (" + recErr.Error() + ")"
		}
		if out.reasons == nil {
			out.reasons = make(map[string]string, len(fd.declared))
		}
		out.rels = append(out.rels, rel)
		out.reasons[rel] = msg
	}
	return out, recErr
}

// unresolvedReason is the sentence one entry's own failed resolution offers
// the author: a producer no tier serves is published, one that resolved and
// cannot serve the id is a declaration to correct on one side or the other.
// Neither carries the server's filesystem path (#1934).
func unresolvedReason(dep bundle.FragmentDependency, served error) string {
	if served == nil {
		return fmt.Sprintf("bundle %q resolves in no tier of this server (team, platform, baked catalog): publish it or drop the dependency (ADR-145)", dep.From)
	}
	return fmt.Sprintf("bundle %q resolves and cannot serve it: %s — export it there or drop the dependency (ADR-145)", dep.From, served.Error())
}

// exportedFragment resolves one dependency's fragment through the server's
// own tiers — the launch's chokepoint, byte for byte. What it tells apart is
// WHOSE problem a failure is, because an OPEN and a WRITE answer the same
// three answers differently:
//
//   - found — the producer served the fragment.
//   - !found, unserved == nil — the producer resolves in NO tier. A
//     declaration error, permanent; the reading keeps its own miss.
//   - !found, unserved != nil — the producer resolved and cannot serve this
//     id (exports nothing, exports another id, the exported path is not
//     there or is no regular file). A declaration error too, and permanent:
//     answered as the reason the entry stayed unplaced rather than as a
//     failure of the whole reading, so a typo in one `dependencies.fragments`
//     id cannot make every consumer of a bundle impossible to OPEN in the
//     editor — #2865's first half, and philosophy clause 1's "warn rather
//     than reject". The write gates still refuse it: the import it exists
//     for does not resolve, so the compile does not pass.
//   - err — a tier that FAILED: transient, errBotSnapshotResolve rides the
//     chain, and the route retries rather than retiring a bundle that
//     resolves.
//
// Neither sentence carries the producer's directory: it is the server's
// baked-catalog mount or the temp dir a tier materialization used, and no
// diagnostic of this plane discloses the server's filesystem (#1934).
func (a authoringFragments) exportedFragment(ctx context.Context, producers *snapshotProducers, dep bundle.FragmentDependency) ([]byte, bool, error, error) {
	dir, err := producers.dir(ctx, a.s, a.teamID, dep.From)
	if err != nil {
		// %w keeps the transient sentinel reachable through this sentence:
		// the route tells a store blip (retry) from a declaration the author
		// must change.
		return nil, false, nil, fmt.Errorf("resolve shared fragment %q from bundle %q: %w", dep.ExportedID(), dep.From, err)
	}
	if dir == "" {
		return nil, false, nil, nil
	}
	source, err := botinstall.ExportedFragmentSource(dir, dep)
	if err != nil {
		return nil, false, errors.New(withoutProducerDir(dir, err.Error())), nil
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return nil, false, fmt.Errorf("read shared fragment %q: %s", dep.ExportedID(), withoutProducerDir(dir, err.Error())), nil
	}
	return content, true, nil, nil
}

// withoutProducerDir strips the server's own filesystem path out of a
// resolution failure's sentence, the way relUnitDiagnostics strips the
// workspace root out of a unit's (#1934): the producer's directory is a
// baked-catalog mount or a materialization's temp dir, and these sentences
// travel to an HTTP client. What stays is the bundle-relative remainder,
// which is what the author can act on.
func withoutProducerDir(dir, msg string) string {
	if dir == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, " at "+dir+"/", " at ")
	msg = strings.ReplaceAll(msg, " at "+dir, "")
	msg = strings.ReplaceAll(msg, dir+"/", "")
	return strings.ReplaceAll(msg, dir, "the bundle the tier served")
}

// fragmentLocalStateRecord is the bundle's own record of what a previous
// resolution materialized, read from the files map the authoring plane holds
// — the half RefuseFragmentShadow needs to tell an installer's own output
// from a fragment the bundle ships. Both carriers that bring a bundle here
// keep the record (botsource.ReadBundleDir copies it, the studio's own
// bundles never had one), and the decode rule is pkg/bundle's.
func fragmentLocalStateRecord(files map[string]string) (map[string]string, error) {
	body, ok := files[bundle.LocalStateFile]
	if !ok {
		return nil, nil
	}
	entries, err := bundle.DecodeLocalStateOfKind([]byte(body), bundle.LocalStateFragment)
	if err != nil {
		return nil, fmt.Errorf("read the bundle's record of its materialized fragments (%s): %w", bundle.LocalStateFile, err)
	}
	recorded := make(map[string]string, len(entries))
	for _, entry := range entries {
		recorded[entry.Path] = entry.SHA256
	}
	return recorded, nil
}

// sortedFragmentRels is the stable order a resolution walks its dependencies
// in: a verdict read off a map's iteration is a verdict that changes between
// runs, so a refusal would name a different shadow each time.
func sortedFragmentRels(m map[string]bundle.FragmentDependency) []string {
	rels := make([]string, 0, len(m))
	for rel := range m {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	return rels
}

// fragmentResolveError is a failure of the authoring plane's own resolution
// of a declared shared fragment: a tier that did not answer, a producer that
// is no readable bundle, a shadow the bundle ships. It is neither a request
// the server cannot read (400) nor a load the reader refused (422 by its own
// refusal): the reading was fine and the server's resolution of a dependency
// was not, so the routes answer it as its own thing (unitRequestError).
type fragmentResolveError struct{ err error }

func (e fragmentResolveError) Error() string {
	return fmt.Sprintf("the bot's shared fragment dependencies do not resolve: %v", e.err)
}

func (e fragmentResolveError) Unwrap() error { return e.err }
