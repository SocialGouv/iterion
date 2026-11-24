package botinstall

import (
	"errors"
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
func ResolveFragments(workdir, installedPath string) ([]bundle.LocalStateEntry, error) {
	consumer, err := bundle.OpenDir(installedPath)
	if err != nil {
		return nil, fmt.Errorf("resolve shared fragments: open %s: %w", installedPath, err)
	}
	previous, err := bundle.ReadLocalStateOfKind(installedPath, bundle.LocalStateFragment)
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

	resolved := make([]bundle.LocalStateEntry, 0, len(sorted))
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
		digest, copyErr := copyResolvedFile(source, dest)
		if copyErr != nil {
			return nil, fmt.Errorf("resolve shared fragment %q of %q from %q: %w", dep.ExportedID(), filepath.Base(installedPath), dep.From, copyErr)
		}
		resolved = append(resolved, bundle.LocalStateEntry{Path: rel, SHA256: digest})
	}

	if len(resolved) == 0 && len(previous) == 0 {
		// Nothing declared, nothing recorded as a fragment: the bundle owns
		// every file it has under lib/, and this pass has no business
		// writing or removing one — nor touching a record that exists for
		// another kind of local state (a regenerated catalog).
		return nil, nil
	}

	declared := make(map[string]struct{}, len(resolved))
	for _, f := range resolved {
		declared[f.Path] = struct{}{}
	}
	if err := dropUndeclaredLocalState(installedPath, sharedFragments, previous, declared); err != nil {
		return nil, err
	}
	if err := bundle.WriteLocalStateOfKind(installedPath, bundle.LocalStateFragment, resolved); err != nil {
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
		source, err := ExportedFragmentSource(producerDir, dep)
		if errors.Is(err, ErrNoProducerBundle) {
			continue
		}
		if err != nil {
			return "", err
		}
		return source, nil
	}
	return "", fmt.Errorf("shared fragment %q comes from bundle %q, which is not materialized (looked in: %s); run `%s`", dep.ExportedID(), dep.From, strings.Join(tried, ", "), syncCommand)
}

// ExportedFragmentSource is the file one producer bundle DIRECTORY provides
// for a dependency, with the refusals of ADR-145 clause 2 that depend on the
// producer alone: the manifest must be the bundle the consumer named, the
// export must exist, and its file must be a regular file.
//
// It is the chokepoint BOTH planes resolve through, for the reason
// [ExportedSkillSource] states for the other artefact kind. The install walks
// the locations a project materializes producers into
// (`locateExportedFragment`); the server resolves the producer through its own
// tiers before asking here (pkg/server/bot_snapshot_fragments.go, ADR-145's
// launch-plane resolution). What a MISS reads differs by plane — one names
// `iterion bots sync`, the other the tiers it searched, because a server has
// no working tree to sync — but what a FOUND producer is held to does not, or
// the two planes would disagree about the same bundle.
func ExportedFragmentSource(producerDir string, dep bundle.FragmentDependency) (string, error) {
	producer, err := bundle.OpenDir(producerDir)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrNoProducerBundle, producerDir, err)
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

// refuseFragmentShadow holds ADR-145's no-shadow clause over a FILESYSTEM
// destination, which is what the install has. The verdict itself is
// [RefuseFragmentShadow].
func refuseFragmentShadow(dest, rel string, dep bundle.FragmentDependency, recorded map[string]string) error {
	info, err := os.Lstat(dest)
	if os.IsNotExist(err) {
		return RefuseFragmentShadow(rel, "", false, dep, recorded)
	}
	if err != nil {
		return fmt.Errorf("resolve shared fragment %q: inspect %s: %w", dep.ExportedID(), rel, err)
	}
	current := ""
	if info.Mode().IsRegular() {
		if digest, hashErr := bundle.FileSHA256(dest); hashErr == nil {
			current = digest
		}
	}
	return RefuseFragmentShadow(rel, current, true, dep, recorded)
}

// RefuseFragmentShadow is ADR-145 clause 2's no-shadow verdict, over what is
// ALREADY at rel rather than over a filesystem: present says whether anything
// is there, currentDigest is its sha256 ("" when it could not be hashed or is
// not a regular file), recorded is the bundle's own record of what a previous
// resolution materialized. A silent overwrite would make
// `import "lib/x.bot"` mean one thing in the repository and another after an
// install — the drift the record exists to end. A file a previous resolution
// materialized and still describes exactly is this installer's own output and
// is replaced.
//
// Pure, because the two planes hold the destination differently — the install
// reads a file, the launch snapshot holds the frozen bytes — and a rule
// recopied for the second reader is the rule that drifts.
func RefuseFragmentShadow(rel, currentDigest string, present bool, dep bundle.FragmentDependency, recorded map[string]string) error {
	if !present {
		return nil
	}
	if digest, ok := recorded[rel]; ok && currentDigest != "" && currentDigest == digest {
		return nil
	}
	return fmt.Errorf("dependencies.fragments %q would shadow %s, which this bundle ships: rename the dependency or drop the shipped fragment", dep.Name, rel)
}

// fragmentDependenciesRefusal is Install's answer to a bundle that declares
// `dependencies.fragments`, for the reason sharedDependencyRefusal states.
func fragmentDependenciesRefusal(name string, deps []bundle.FragmentDependency) error {
	names := make([]string, 0, len(deps))
	for _, dep := range deps {
		names = append(names, fmt.Sprintf("%s (from %s)", dep.Name, dep.From))
	}
	sort.Strings(names)
	return sharedDependencyRefusal(sharedFragments, name, names)
}
