package server

import (
	"fmt"
	"path"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// fragmentRemedy is the way forward a refusal offers once it has met a
// declared dependency's unmaterialized path. The planes differ, and one
// that answered the other's remedy would send the author somewhere they
// cannot act: a working tree SYNCS, a server PUBLISHES.
type fragmentRemedy int

const (
	// remedySync names `iterion bots sync`: the reader has a working tree,
	// where the install materializes the dependency under the consumer's
	// own lib/.
	remedySync fragmentRemedy = iota
	// remedyPublish names the tiers a server searched. A server has no
	// working tree to sync, so what unblocks it is the producer reaching a
	// tier — the sentence exportedFragmentForSnapshot answers on the launch
	// plane, for the same reason.
	remedyPublish
	// remedyUnchecked is the cloud plane on a request that searched NO
	// tier: the caller does not hold the team whose bundles a resolution
	// reads, or no bot of the store was named. "Publish it" is a verdict
	// about the producer, and a reading that never looked for one has not
	// earned it — a sentence telling the author the producer is nowhere,
	// about one the same server resolves and serves for their colleague,
	// advises a destructive edit on a correct manifest.
	remedyUnchecked
)

// fragmentDependencies is what one reading of a bot knows of the bundle's
// declared `dependencies.fragments`: the entries keyed by the path each
// materializes at, and the remedy a miss that is STILL a miss names. It is
// the ONE carrier of that knowledge through the authoring plane, so a
// refusal cannot be written that forgets the actor the miss belongs to.
//
// The zero value declares nothing, and every refusal through it reads
// exactly as it would with no manifest in reach: a site that has no bundle
// to ask passes it and says so, rather than inventing an answer.
type fragmentDependencies struct {
	declared map[string]bundle.FragmentDependency
	remedy   fragmentRemedy
	// unresolved names, per materialized path, what the plane's OWN
	// resolution found when it could not place that entry. It is per ENTRY
	// because a declaration set is not uniform — one producer serves, the
	// next resolves nowhere, the third resolves and exports no such
	// fragment — so a remedy stamped over the whole set tells the author
	// two of those three things that are false.
	unresolved map[string]string
}

// declaredFragments reads the manifest of the bundle a unit belongs to
// — from the files map a cloud bot source carries, or from the unit's
// directory on disk — and answers its declared dependencies.fragments
// entries keyed by the path each materializes at: the BUNDLE-relative
// `lib/<name>.bot` of ADR-145 clause 2, which is what both the install
// (pkg/botinstall/fragments.go) and the launch's freeze
// (resolveSnapshotFragments) write, and what an `import "lib/<name>.bot"`
// of a root entry resolves to. A manifest that does not read declares
// nothing here: the annotation this set serves is an explanation, never a
// verdict, so a read failure changes nothing a gate decides.
func declaredFragments(files map[string]string, dir string) fragmentDependencies {
	var m *bundle.Manifest
	if files != nil {
		// The one "which manifest file, decoded how" rule for a files map
		// lives on the stored bot source; a parse request's map is the same
		// shape, so the rule is not recopied.
		m = (&botsource.BotSource{Files: files}).Manifest()
	} else if dir != "" {
		m, _ = bundle.LoadManifestForDir(dir)
	}
	if m == nil {
		return fragmentDependencies{}
	}
	out := make(map[string]bundle.FragmentDependency, len(m.Dependencies.Fragments))
	for _, dep := range m.Dependencies.Fragments {
		out[path.Clean(dep.FragmentRelPath())] = dep
	}
	return fragmentDependencies{declared: out, remedy: remedySync}
}

// asDiskLoadOf answers the declarations as a load ROOTED AT mainRel's own
// directory sees them — unit.LoadDir's frame, which the write gate uses on
// the bundle it materializes. The loader names a missing fragment by the rel
// it resolved, and a rel is relative to the reading's own root: a files map
// is keyed from the bundle root, so a LoadMap's rels are already in the
// bundle's terms, but a load rooted BESIDE a workflow in a subdirectory
// names `lib/x.bot` for what is `<dir>/lib/x.bot` of the bundle. No
// dependency materializes there — the install writes at the bundle root
// (ADR-145 clause 2) — so such a load declares nothing, rather than reading
// the root's declaration into a frame it does not belong to.
func (fd fragmentDependencies) asDiskLoadOf(mainRel string) fragmentDependencies {
	if dir := path.Dir(mainRel); dir != "" && dir != "." {
		return fragmentDependencies{remedy: fd.remedy}
	}
	return fd
}

// miss reports whether d is the loader's "no such fragment" for the exact
// path a declared dependencies.fragments entry materializes — ADR-145
// clause 3's wrong actor: the file is a bundle DEPENDENCY's, never one the
// author was to write, and the miss belongs to the install, not to the
// import line. The message's parenthesized tail is the loader's own
// resolved rel, so the match reads what the loader looked for rather than
// re-deriving a path of its own — and a resolved rel the install does NOT
// write (an entry under a subdirectory, whose import resolves beside it
// rather than at the bundle root) matches nothing, because no dependency
// materializes there.
func (fd fragmentDependencies) miss(d parser.Diagnostic) (bundle.FragmentDependency, string, bool) {
	if d.Code != parser.DiagImportUnreadable {
		return bundle.FragmentDependency{}, "", false
	}
	for rel, dep := range fd.declared {
		if strings.HasSuffix(d.Message, "no such fragment ("+rel+")") {
			return dep, rel, true
		}
	}
	return bundle.FragmentDependency{}, "", false
}

// note names, for a refusal that met a declared dependency's unmaterialized
// path, the actor the miss belongs to: not the import, and not a file this
// bundle was to ship. The verdict stays the gate's own.
//
// What it offers as a way forward is, in order: what THIS entry's own
// resolution found (unresolved), the plane's remedy otherwise. An entry a
// resolution attempted knows more than its plane does, and a sentence that
// told the author "the producer is nowhere" about a producer that answered
// is worse than no sentence — which is why a cloud plane that searched no
// tier (remedyUnchecked) says that, rather than borrowing the verdict of the
// plane that did search.
func (fd fragmentDependencies) note(rel string, dep bundle.FragmentDependency) string {
	remedy := ": a synced working tree does (`iterion bots sync` resolves it into the consumer's own lib/, and the install names a miss, ADR-145)"
	switch found := fd.unresolved[rel]; {
	case found != "":
		remedy = ", and " + found
	case fd.remedy == remedyPublish:
		remedy = fmt.Sprintf(", and bundle %q resolves in no tier of this server (team, platform, baked catalog): publish it or drop the dependency (ADR-145)", dep.From)
	case fd.remedy == remedyUnchecked:
		remedy = fmt.Sprintf(", and this server did not resolve it on this request — a team's declared fragments resolve for a caller holding that team's bot authoring, through the bot's own editor path — so whether bundle %q serves it is unanswered here (ADR-145)", dep.From)
	}
	// The manifest KEY is in the head, not in one remedy: an author reading
	// either sentence has to know which declaration to look at.
	return fmt.Sprintf("%s is the path the manifest's `dependencies.fragments` entry %q (from bundle %q) materializes — no file of this bundle provides it%s", rel, dep.Name, dep.From, remedy)
}

// annotate is ONE diagnostic as a client reads it: the string
// Diagnostic.Error spells, plus the dependency note when that diagnostic
// met a declared dependency's unmaterialized path. Every refusal and every
// verdict of the authoring plane that quotes a load diagnostic goes through
// it (firstErrorDiagnostic, the write gate's per-diagnostic loop), so the
// note is not a courtesy one site remembers and the next forgets.
func (fd fragmentDependencies) annotate(d parser.Diagnostic) string {
	msg := d.Error()
	if dep, rel, ok := fd.miss(d); ok {
		msg += " — " + fd.note(rel, dep)
	}
	return msg
}

// missed answers the declared entries diags report as unmaterialized, keyed
// by the path each materializes at: the TRIGGER of a resolution, and the
// reason a bundle with nothing missing costs nothing — no manifest, no
// declaration or no miss and the answer is nil, so no tier is ever read.
func (fd fragmentDependencies) missed(diags []parser.Diagnostic) map[string]bundle.FragmentDependency {
	if len(fd.declared) == 0 {
		return nil
	}
	var out map[string]bundle.FragmentDependency
	for _, d := range diags {
		if d.Severity != parser.SeverityError {
			continue
		}
		dep, rel, ok := fd.miss(d)
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]bundle.FragmentDependency, len(fd.declared))
		}
		out[rel] = dep
	}
	return out
}

// withRemedy answers the same declarations with another way forward: the
// plane a refusal belongs to is decided where the reading happens, not where
// the manifest is read.
func (fd fragmentDependencies) withRemedy(r fragmentRemedy) fragmentDependencies {
	fd.remedy = r
	return fd
}

// withUnresolved answers the same declarations carrying, per entry, what a
// resolution of that entry actually found. Only the resolution knows it, so
// only the resolution says it.
func (fd fragmentDependencies) withUnresolved(why map[string]string) fragmentDependencies {
	fd.unresolved = why
	return fd
}
