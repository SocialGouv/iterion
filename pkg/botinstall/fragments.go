package botinstall

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// syncCommand is the command that materializes every locked dependency
// before resolving the fragments that depend on them. Named in every refusal
// that comes from an unresolved dependency, so the operator reads the way
// forward instead of the obstacle.
const syncCommand = "iterion bots sync"

// ResolveFragments materializes the `dependencies.fragments` of the bundle
// installed at installedPath into that bundle's own lib/, by copy (ADR-145
// clause 2): the fragment becomes an ordinary file under lib/, so the
// parser, the unit loader, `E045` and the lib/ confinement learn nothing and
// every surface that reads the unit keeps reading what it reads now.
//
// The caller owns the ORDER. A producer must already be materialized beside
// the consumer, or under <workdir>/.botz; botdeps.Sync materializes every
// locked dependency FIRST and calls this as a second pass, so a consumer
// that sorts before its producer resolves anyway. botinstall.Install, which
// installs one bundle and cannot vouch for the rest of the workspace,
// refuses instead of resolving — see fragmentDependenciesRefusal.
//
// It is a reconciliation, not an append: the returned set is exactly what
// the manifest declares, and a fragment a previous resolution materialized
// and the manifest no longer declares is removed — but only when the
// bundle's own note still describes it exactly (under lib/, digest
// unchanged), so nothing this installer did not write is ever deleted.
func ResolveFragments(workdir, installedPath string) ([]bundle.MaterializedFragment, error) {
	consumer, err := bundle.OpenDir(installedPath)
	if err != nil {
		return nil, fmt.Errorf("resolve shared fragments: open %s: %w", installedPath, err)
	}
	previous, err := bundle.ReadMaterializedFragments(installedPath)
	if err != nil {
		return nil, fmt.Errorf("resolve shared fragments for %s: %w", filepath.Base(installedPath), err)
	}
	var deps []bundle.FragmentDependency
	if consumer.Manifest != nil {
		deps = consumer.Manifest.Dependencies.Fragments
	}
	sorted := make([]bundle.FragmentDependency, len(deps))
	copy(sorted, deps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	layers := producerLayers(workdir, installedPath)
	recorded := make(map[string]string, len(previous))
	for _, f := range previous {
		recorded[f.Path] = f.SHA256
	}

	resolved := make([]bundle.MaterializedFragment, 0, len(sorted))
	for _, dep := range sorted {
		rel := dep.FragmentRelPath()
		dest := filepath.Join(installedPath, filepath.FromSlash(rel))
		if err := refuseFragmentShadow(dest, rel, dep, recorded); err != nil {
			return nil, err
		}
		source, srcErr := locateExportedFragment(layers, dep)
		if srcErr != nil {
			return nil, srcErr
		}
		digest, copyErr := copyFragment(source, dest)
		if copyErr != nil {
			return nil, fmt.Errorf("resolve shared fragment %q of %q from %q: %w", dep.ExportedID(), filepath.Base(installedPath), dep.From, copyErr)
		}
		resolved = append(resolved, bundle.MaterializedFragment{Path: rel, SHA256: digest})
	}

	if len(resolved) == 0 && len(previous) == 0 {
		// Nothing declared, nothing recorded: the bundle owns every file it
		// has, and this pass has no business writing or removing one.
		return nil, nil
	}

	declared := make(map[string]struct{}, len(resolved))
	for _, f := range resolved {
		declared[f.Path] = struct{}{}
	}
	if err := dropUndeclaredFragments(installedPath, previous, declared); err != nil {
		return nil, err
	}
	if err := bundle.WriteMaterializedFragments(installedPath, resolved); err != nil {
		return nil, fmt.Errorf("resolve shared fragments for %s: %w", filepath.Base(installedPath), err)
	}
	return resolved, nil
}

// producerLayers are the directories a producer bundle is looked for in, in
// order: the consumer's own install root first (a sibling installed by the
// same sync), then the workspace's .botz (a consumer staged into a private
// destination still resolves against the real workspace). Deduplicated, so a
// refusal does not name the same directory twice.
func producerLayers(workdir, installedPath string) []string {
	layers := []string{filepath.Dir(installedPath)}
	if workdir == "" {
		return layers
	}
	workspace := filepath.Join(workdir, ".botz")
	// Compared resolved: SyncOne's callers may pass a relative workspace,
	// and a refusal that names one directory twice reads as two misses.
	if sameDir(workspace, layers[0]) {
		return layers
	}
	return append(layers, workspace)
}

func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && absA == absB
}

// locateExportedFragment finds the file a dependency names, naming every
// layer it looked in when it does not. The dependency exists at install
// time, so this is where its miss is reported: at parse time the file is
// simply absent, and `E046` would then say "no such fragment" about a file
// the author never wrote.
func locateExportedFragment(layers []string, dep bundle.FragmentDependency) (string, error) {
	var tried []string
	for _, layer := range layers {
		producerDir := filepath.Join(layer, dep.From)
		tried = append(tried, producerDir)
		producer, err := bundle.OpenDir(producerDir)
		if err != nil {
			continue
		}
		if producer.Manifest == nil || producer.Manifest.Name != dep.From {
			actual := "<no manifest>"
			if producer.Manifest != nil {
				actual = producer.Manifest.Name
			}
			return "", fmt.Errorf("shared fragment %q: bundle at %s declares manifest name %q, not %q", dep.ExportedID(), producerDir, actual, dep.From)
		}
		exports := producer.Manifest.Exports.Fragments
		for i := range exports {
			if exports[i].ID != dep.ExportedID() {
				continue
			}
			source := filepath.Join(producer.Dir, filepath.FromSlash(exports[i].Path))
			info, statErr := os.Lstat(source)
			if statErr != nil {
				return "", fmt.Errorf("bundle %q exports fragment %q at %s, which is not there: %w", dep.From, dep.ExportedID(), exports[i].Path, statErr)
			}
			if !info.Mode().IsRegular() {
				// A symlink is refused by the snapshot as unportable and by
				// the unit loader as ErrOutside: a bundle that resolved on
				// one disk would fail in the snapshot and in cloud.
				return "", fmt.Errorf("bundle %q exports fragment %q at %s, which is not a regular file", dep.From, dep.ExportedID(), exports[i].Path)
			}
			return source, nil
		}
		available := make([]string, 0, len(exports))
		for i := range exports {
			available = append(available, exports[i].ID)
		}
		if len(available) == 0 {
			return "", fmt.Errorf("bundle %q at %s exports no fragment, so it cannot provide %q", dep.From, producerDir, dep.ExportedID())
		}
		return "", fmt.Errorf("bundle %q at %s does not export fragment %q (it exports: %s)", dep.From, producerDir, dep.ExportedID(), strings.Join(available, ", "))
	}
	return "", fmt.Errorf("shared fragment %q comes from bundle %q, which is not materialized (looked in: %s); run `%s`", dep.ExportedID(), dep.From, strings.Join(tried, ", "), syncCommand)
}

// refuseFragmentShadow holds ADR-145's no-shadow clause: a dependency may
// not overwrite a fragment the bundle itself ships. A silent overwrite would
// make `import "lib/x.bot"` mean one thing in the repository and another
// after an install — the drift the record exists to end. A file a previous
// resolution materialized and still describes exactly is this installer's
// own output and is replaced.
func refuseFragmentShadow(dest, rel string, dep bundle.FragmentDependency, recorded map[string]string) error {
	info, err := os.Lstat(dest)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve shared fragment %q: inspect %s: %w", dep.ExportedID(), rel, err)
	}
	digest, ok := recorded[rel]
	if ok && info.Mode().IsRegular() {
		if current, hashErr := bundle.FileSHA256(dest); hashErr == nil && current == digest {
			return nil
		}
	}
	return fmt.Errorf("dependencies.fragments %q would shadow %s, which this bundle ships: rename the dependency or drop the shipped fragment", dep.Name, rel)
}

// dropUndeclaredFragments removes what a previous resolution materialized
// and the manifest no longer declares. It deletes only a path the bundle's
// own note still describes exactly — under lib/, bytes unchanged — because
// leaving it would let a dropped dependency keep answering an `import` with
// a stale copy, and deleting anything else would destroy a file this
// installer never wrote.
func dropUndeclaredFragments(installedPath string, previous []bundle.MaterializedFragment, declared map[string]struct{}) error {
	for _, f := range previous {
		if _, keep := declared[f.Path]; keep {
			continue
		}
		if !strings.HasPrefix(f.Path, bundle.DirFragments+"/") || strings.Contains(f.Path, "../") {
			return fmt.Errorf("resolve shared fragments for %s: recorded fragment %q is not a path this installer writes", filepath.Base(installedPath), f.Path)
		}
		stale := filepath.Join(installedPath, filepath.FromSlash(f.Path))
		info, err := os.Lstat(stale)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("resolve shared fragments for %s: inspect %s: %w", filepath.Base(installedPath), f.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("resolve shared fragments for %s: %s is no longer a regular file; remove it by hand", filepath.Base(installedPath), f.Path)
		}
		current, hashErr := bundle.FileSHA256(stale)
		if hashErr != nil {
			return fmt.Errorf("resolve shared fragments for %s: hash %s: %w", filepath.Base(installedPath), f.Path, hashErr)
		}
		if current != f.SHA256 {
			return fmt.Errorf("resolve shared fragments for %s: %s was edited after it was materialized and is no longer declared; remove it by hand", filepath.Base(installedPath), f.Path)
		}
		if err := os.Remove(stale); err != nil {
			return fmt.Errorf("resolve shared fragments for %s: remove %s: %w", filepath.Base(installedPath), f.Path, err)
		}
	}
	return nil
}

func copyFragment(source, dest string) (string, error) {
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

// fragmentDependenciesRefusal is Install's answer to a bundle that declares
// `dependencies.fragments`: one install cannot resolve them, because the
// producer may be a bundle the same sync has not materialized yet — the
// order a lockfile is walked in is alphabetical, so a consumer `a-bot` is
// installed before `.botz/z-lib` exists. Resolving here would make a refusal
// depend on an accident of sorting; refusing names the command that does it
// in the right order.
func fragmentDependenciesRefusal(name string, deps []bundle.FragmentDependency) error {
	names := make([]string, 0, len(deps))
	for _, dep := range deps {
		names = append(names, fmt.Sprintf("%s (from %s)", dep.Name, dep.From))
	}
	sort.Strings(names)
	return fmt.Errorf("bundle %q declares dependencies.fragments %s: a single install cannot resolve them, because their producers are materialized by the project lockfile; add it to bots.lock and run `%s`", name, strings.Join(names, ", "), syncCommand)
}
