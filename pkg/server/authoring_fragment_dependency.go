package server

import (
	"fmt"
	"path"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// declaredFragmentPaths reads the manifest of the bundle a unit belongs to
// — from the files map a cloud bot source carries, or from the unit's
// directory on disk — and answers its declared dependencies.fragments
// entries keyed by the unit-relative path each materializes at: the path an
// `import "lib/<name>.bot"` of the main resolves to, computed the loader's
// way (the main's directory joined to the entry's FragmentRelPath). A
// manifest that does not read declares nothing here: the annotation this
// set serves is an explanation, never a verdict, so a read failure changes
// nothing a gate decides.
func declaredFragmentPaths(files map[string]string, dir, mainRel string) map[string]bundle.FragmentDependency {
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
		return nil
	}
	base := path.Dir(mainRel)
	out := make(map[string]bundle.FragmentDependency, len(m.Dependencies.Fragments))
	for _, dep := range m.Dependencies.Fragments {
		out[path.Join(base, dep.FragmentRelPath())] = dep
	}
	return out
}

// declaredFragmentMiss reports whether d is the loader's "no such fragment"
// for the exact path a declared dependencies.fragments entry materializes —
// ADR-145 clause 3's wrong actor: the file is a bundle DEPENDENCY's, never
// one the author was to write, and the miss belongs to the install, not to
// the import line. The message's parenthesized tail is the loader's own
// resolved rel, so the match reads what the loader looked for rather than
// re-deriving a path of its own.
func declaredFragmentMiss(d parser.Diagnostic, declared map[string]bundle.FragmentDependency) (bundle.FragmentDependency, string, bool) {
	if d.Code != parser.DiagImportUnreadable {
		return bundle.FragmentDependency{}, "", false
	}
	for rel, dep := range declared {
		if strings.HasSuffix(d.Message, "no such fragment ("+rel+")") {
			return dep, rel, true
		}
	}
	return bundle.FragmentDependency{}, "", false
}

// fragmentDependencyNote names, for a refusal that met a declared
// dependency's unmaterialized path, the actor the miss belongs to: not the
// import, and not a file this bundle was to ship. The verdict stays the
// gate's own — whether the authoring surface RESOLVES or merely NAMES such
// a miss is the authoring-surface decision #2865 tracks; this is the half
// that decides nothing.
func fragmentDependencyNote(rel string, dep bundle.FragmentDependency) string {
	return fmt.Sprintf("%s is the path the manifest's declared dependency %q (from bundle %q) materializes — no file of this bundle provides it: a synced working tree does (`iterion bots sync` resolves dependencies.fragments into the consumer's own lib/, and the install names a miss, ADR-145)", rel, dep.Name, dep.From)
}
