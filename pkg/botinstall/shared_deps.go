package botinstall

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// sharedArtefact is what one artefact kind of the two-sided sharing key needs
// from the resolution every kind goes through: the local-state kind its
// materialized files are recorded under, and the bundle directory they land
// in. A second kind that copied the reconciliation instead of naming itself
// here would be a second place deciding what an install may delete.
type sharedArtefact struct {
	kind bundle.LocalStateKind
	dir  string
}

var (
	sharedFragments = sharedArtefact{kind: bundle.LocalStateFragment, dir: bundle.DirFragments}
	sharedSkills    = sharedArtefact{kind: bundle.LocalStateSkill, dir: bundle.DirSkills}
)

// label names one artefact in a refusal ("fragment"), plural names the pass
// ("fragments"). Both read from the kind, so a refusal cannot drift from the
// record it is about.
func (a sharedArtefact) label() string  { return string(a.kind) }
func (a sharedArtefact) plural() string { return string(a.kind) + "s" }

// copyResolvedFile writes source's bytes at dest and returns their digest —
// the digest the local-state record stores for what this installer wrote.
func copyResolvedFile(source, dest string) (string, error) {
	data, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", err
	}
	return bundle.SHA256Bytes(data), nil
}

// dropUndeclaredLocalState removes what a previous resolution materialized
// and the manifest no longer declares. It deletes only a path the bundle's
// own record still describes exactly — in the artefact's directory, bytes
// unchanged — because leaving it would let a dropped dependency keep
// answering under a name the manifest retired, and deleting anything else
// would destroy a file this installer never wrote.
func dropUndeclaredLocalState(installedPath string, a sharedArtefact, previous []bundle.LocalStateEntry, declared map[string]struct{}) error {
	for _, f := range previous {
		if _, keep := declared[f.Path]; keep {
			continue
		}
		if !strings.HasPrefix(f.Path, a.dir+"/") || strings.Contains(f.Path, "../") {
			return fmt.Errorf("resolve shared %s for %s: recorded %s %q is not a path this installer writes", a.plural(), filepath.Base(installedPath), a.label(), f.Path)
		}
		stale := filepath.Join(installedPath, filepath.FromSlash(f.Path))
		info, err := os.Lstat(stale)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("resolve shared %s for %s: inspect %s: %w", a.plural(), filepath.Base(installedPath), f.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("resolve shared %s for %s: %s is no longer a regular file; remove it by hand", a.plural(), filepath.Base(installedPath), f.Path)
		}
		current, hashErr := bundle.FileSHA256(stale)
		if hashErr != nil {
			return fmt.Errorf("resolve shared %s for %s: hash %s: %w", a.plural(), filepath.Base(installedPath), f.Path, hashErr)
		}
		if current != f.SHA256 {
			return fmt.Errorf("resolve shared %s for %s: %s was edited after it was materialized and is no longer declared; remove it by hand", a.plural(), filepath.Base(installedPath), f.Path)
		}
		if err := os.Remove(stale); err != nil {
			return fmt.Errorf("resolve shared %s for %s: remove %s: %w", a.plural(), filepath.Base(installedPath), f.Path, err)
		}
	}
	return nil
}

// sharedDependencyRefusal is Install's answer to a bundle that declares a
// shared dependency: one install cannot resolve it, because the producer may
// be a bundle the same sync has not materialized yet — the order a lockfile
// is walked in is alphabetical, so a consumer `a-bot` is installed before
// `.botz/z-lib` exists. Resolving here would make a refusal depend on an
// accident of sorting; refusing names the command that does it in the right
// order.
func sharedDependencyRefusal(a sharedArtefact, name string, entries []string) error {
	return fmt.Errorf("bundle %q declares dependencies.%s %s: a single install cannot resolve them, because their producers are materialized by the project lockfile; add it to bots.lock and run `%s`", name, a.plural(), strings.Join(entries, ", "), syncCommand)
}
