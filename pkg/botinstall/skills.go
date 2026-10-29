package botinstall

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/skilllib"
)

// ResolveSkills materializes the `dependencies.skills` of the bundle
// installed at installedPath into that bundle's own skills/, by copy (ADR-144
// clause 2): the shared document becomes an ordinary file where a shipped
// copy would have been, so the run-start mirror, the install's skill count
// and `validate`'s skill-file lints learn nothing and every surface that
// reads the bundle keeps reading what it reads now.
//
// The caller owns the ORDER, for the reason ResolveFragments states: a
// producer must already be materialized beside the consumer or under
// <workdir>/.botz, which only botdeps.Sync can promise.
//
// It is a reconciliation, not an append: the returned set is exactly what the
// manifest declares, and a skill a previous resolution materialized and the
// manifest no longer declares is removed — but only when the bundle's own
// record still describes it exactly (under skills/, digest unchanged), so
// nothing this installer did not write is ever deleted.
func ResolveSkills(workdir, installedPath string) ([]bundle.LocalStateEntry, error) {
	consumer, err := bundle.OpenDir(installedPath)
	if err != nil {
		return nil, fmt.Errorf("resolve shared skills: open %s: %w", installedPath, err)
	}
	previous, err := bundle.ReadLocalStateOfKind(installedPath, bundle.LocalStateSkill)
	if err != nil {
		return nil, fmt.Errorf("resolve shared skills for %s: %w", filepath.Base(installedPath), err)
	}
	var deps []bundle.SkillDependency
	if consumer.Manifest != nil {
		deps = consumer.Manifest.Dependencies.Skills
	}
	sorted := make([]bundle.SkillDependency, len(deps))
	copy(sorted, deps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	layers := producerLayers(workdir, installedPath)
	recorded := make(map[string]string, len(previous))
	for _, s := range previous {
		recorded[s.Path] = s.SHA256
	}

	resolved := make([]bundle.LocalStateEntry, 0, len(sorted))
	for _, dep := range sorted {
		rel := dep.SkillRelPath()
		dest := filepath.Join(installedPath, filepath.FromSlash(rel))
		if err := refuseSkillShadow(dest, rel, dep, recorded); err != nil {
			return nil, err
		}
		source, srcErr := locateExportedSkill(layers, dep)
		if srcErr != nil {
			return nil, srcErr
		}
		if err := refuseARenamedSkill(source, dep); err != nil {
			return nil, err
		}
		digest, copyErr := copyResolvedFile(source, dest)
		if copyErr != nil {
			return nil, fmt.Errorf("resolve shared skill %q of %q from %q: %w", dep.ExportedID(), filepath.Base(installedPath), dep.From, copyErr)
		}
		resolved = append(resolved, bundle.LocalStateEntry{Path: rel, SHA256: digest})
	}

	if len(resolved) == 0 && len(previous) == 0 {
		// Nothing declared, nothing recorded as a skill: the bundle owns
		// every file it has under skills/, and this pass has no business
		// writing or removing one — nor touching a record that exists for
		// another kind of local state (a fragment, a regenerated catalog).
		return nil, nil
	}

	declared := make(map[string]struct{}, len(resolved))
	for _, s := range resolved {
		declared[s.Path] = struct{}{}
	}
	if err := dropUndeclaredLocalState(installedPath, sharedSkills, previous, declared); err != nil {
		return nil, err
	}
	if err := bundle.WriteLocalStateOfKind(installedPath, bundle.LocalStateSkill, resolved); err != nil {
		return nil, fmt.Errorf("resolve shared skills for %s: %w", filepath.Base(installedPath), err)
	}
	return resolved, nil
}

// locateExportedSkill finds the document a dependency names, naming every
// layer it looked in when it does not — ADR-144 clause 4. The dependency
// exists at install time, so this is where its miss is reported: later, the
// `skills:` reference is soft by design (a name no file answers mirrors
// nothing and the agent answers from its own priors instead), which is
// exactly the silent failure the record exists to end.
func locateExportedSkill(layers []string, dep bundle.SkillDependency) (string, error) {
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
			return "", fmt.Errorf("shared skill %q: bundle at %s declares manifest name %q, not %q", dep.ExportedID(), producerDir, actual, dep.From)
		}
		exports := producer.Manifest.Exports.Skills
		for i := range exports {
			if exports[i].ID != dep.ExportedID() {
				continue
			}
			source := filepath.Join(producer.Dir, filepath.FromSlash(exports[i].Path))
			info, statErr := os.Lstat(source)
			if statErr != nil {
				return "", fmt.Errorf("bundle %q exports skill %q at %s, which is not there: %w", dep.From, dep.ExportedID(), exports[i].Path, statErr)
			}
			if !info.Mode().IsRegular() {
				// A symlink is refused by the snapshot as unportable and by
				// the mirror inside the sandbox bind-mount (a target outside
				// /workspace is ENOENT): a bundle that resolved on one disk
				// would fail in the snapshot and in cloud.
				return "", fmt.Errorf("bundle %q exports skill %q at %s, which is not a regular file", dep.From, dep.ExportedID(), exports[i].Path)
			}
			return source, nil
		}
		available := make([]string, 0, len(exports))
		for i := range exports {
			available = append(available, exports[i].ID)
		}
		if len(available) == 0 {
			return "", fmt.Errorf("bundle %q at %s exports no skill, so it cannot provide %q", dep.From, producerDir, dep.ExportedID())
		}
		return "", fmt.Errorf("bundle %q at %s does not export skill %q (it exports: %s)", dep.From, producerDir, dep.ExportedID(), strings.Join(available, ", "))
	}
	return "", fmt.Errorf("shared skill %q comes from bundle %q, which is not materialized (looked in: %s); run `%s`", dep.ExportedID(), dep.From, strings.Join(tried, ", "), syncCommand)
}

// refuseSkillShadow holds ADR-144 clause 5's no-shadow half: a dependency may
// not overwrite a skill the bundle itself ships. A silent overwrite would
// make `skills: [x]` mean one thing in the repository and another after an
// install — and the bytes it replaced would be gone from a bundle whose
// producer shipped them. A file a previous resolution materialized and still
// describes exactly is this installer's own output and is replaced.
func refuseSkillShadow(dest, rel string, dep bundle.SkillDependency, recorded map[string]string) error {
	info, err := os.Lstat(dest)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve shared skill %q: inspect %s: %w", dep.ExportedID(), rel, err)
	}
	digest, ok := recorded[rel]
	if ok && info.Mode().IsRegular() {
		if current, hashErr := bundle.FileSHA256(dest); hashErr == nil && current == digest {
			return nil
		}
	}
	return fmt.Errorf("dependencies.skills %q would shadow %s, which this bundle ships: rename the dependency or drop the shipped skill", dep.Name, rel)
}

// refuseARenamedSkill refuses a dependency that would materialize a document
// under a name its own frontmatter contradicts. A skill is addressed by the
// `name:` its frontmatter declares as much as by its file name, so a copy
// where the two disagree is reachable reliably under neither — the same
// failure `bots/catalog_skill_refs_test.go` already refuses for shipped
// skills, which a dependency must not reintroduce by renaming on the way in.
// Substituting anything INSIDE the document is what ADR-144 leaves
// undecided, so the refusal names the two sides instead of rewriting bytes.
func refuseARenamedSkill(source string, dep bundle.SkillDependency) error {
	f, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("resolve shared skill %q from %q: read %s: %w", dep.ExportedID(), dep.From, source, err)
	}
	declared, _ := skilllib.ScanFrontmatter(f)
	closeErr := f.Close()
	if closeErr != nil {
		return fmt.Errorf("resolve shared skill %q from %q: close %s: %w", dep.ExportedID(), dep.From, source, closeErr)
	}
	if declared == "" || declared == dep.Name {
		return nil
	}
	return fmt.Errorf("shared skill %q of bundle %q carries `name: %s` in its frontmatter: declare it as `name: %s` (with `id: %s` if the exported id differs), or a `skills: [%s]` reference reaches a document that answers to another name", dep.ExportedID(), dep.From, declared, declared, dep.ExportedID(), dep.Name)
}

// skillDependenciesRefusal is Install's answer to a bundle that declares
// `dependencies.skills`, for the reason sharedDependencyRefusal states.
func skillDependenciesRefusal(name string, deps []bundle.SkillDependency) error {
	names := make([]string, 0, len(deps))
	for _, dep := range deps {
		names = append(names, fmt.Sprintf("%s (from %s)", dep.Name, dep.From))
	}
	sort.Strings(names)
	return sharedDependencyRefusal(sharedSkills, name, names)
}
