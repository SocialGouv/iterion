package bundle

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// DirFragments holds the `.bot` fragments an `import "lib/x.bot"` reaches.
// The unit loader owns the name (pkg/dsl/unit.FragmentDir); it is restated
// here because a manifest key points into that directory and pkg/bundle sits
// below the DSL, which may not be imported from here.
const DirFragments = "lib"

// FragmentExport binds a stable fragment id to the bundle-relative path of
// the `.bot` fragment a consumer may resolve — the producer side of the
// two-sided key ADR-145 adds for the third artefact kind, beside
// `exports.workflows` (ADR-094) and `exports.skills` (ADR-144).
type FragmentExport struct {
	// ID is the name a consumer resolves this fragment by. Stable across
	// moves of Path: it is the published identity, not the layout.
	ID string `yaml:"id" json:"id"`
	// Path is the fragment inside the producer bundle, under lib/.
	Path string `yaml:"path" json:"path"`
}

// FragmentDependency names one shared fragment a consumer is allowed to
// resolve, and the name its own `import` lines reach it under. A
// producer-only key cannot express consumption: with no consumer
// declaration there is no allow-list, and a bundle would resolve a name from
// any installed bundle that happens to export it.
type FragmentDependency struct {
	// Name is the fragment's name inside THIS bundle: the install
	// materializes it at `lib/<Name>.bot`, which is what the consumer's
	// `import "lib/<Name>.bot"` already spells.
	Name string `yaml:"name" json:"name"`
	// From is the exact manifest name of the bundle that exports it.
	From string `yaml:"from" json:"from"`
	// ID is the exported id to resolve on the producer side. Empty means
	// Name, so a consumer that keeps the published name writes two lines.
	ID string `yaml:"id,omitempty" json:"id,omitempty"`
}

// ExportedID is the producer-side id this dependency resolves. It defaults
// to the consumer-side name rather than to the empty string: a dependency
// that resolves nothing has no written form.
func (d FragmentDependency) ExportedID() string {
	if d.ID == "" {
		return d.Name
	}
	return d.ID
}

// FragmentRelPath is the bundle-relative, slash-separated path the install
// materializes this dependency at — the one an `import` already names.
func (d FragmentDependency) FragmentRelPath() string {
	return DirFragments + "/" + d.Name + fragmentExtension
}

// fragmentExtension is the workflow file extension a fragment carries.
// pkg/dsl/workflowfile owns the accepted set; this is the one spelling a
// manifest key may construct, and validateFragmentSharing refuses any other.
const fragmentExtension = ".bot"

// validateFragmentSharing holds `exports.fragments` and
// `dependencies.fragments` to the shape ADR-145 decides, refusing per index
// so an author reads which entry is wrong. Every refusal is the manifest
// load's: a dependency the parser would discover as a missing file names the
// wrong actor, so the manifest is where a malformed declaration dies.
func validateFragmentSharing(exports BundleExports, dependencies BundleDependencies) error {
	exportIDs := make(map[string]struct{}, len(exports.Fragments))
	for i := range exports.Fragments {
		exp := &exports.Fragments[i]
		exp.ID = strings.TrimSpace(exp.ID)
		exp.Path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(exp.Path)))
		if exp.ID == "" {
			return fmt.Errorf("exports.fragments[%d].id is required", i)
		}
		if strings.ContainsAny(exp.ID, `/\`) || exp.ID == "." || exp.ID == ".." {
			return fmt.Errorf("exports.fragments[%d].id %q must not contain path separators", i, exp.ID)
		}
		if _, duplicate := exportIDs[exp.ID]; duplicate {
			return fmt.Errorf("exports.fragments[%d].id %q is duplicated", i, exp.ID)
		}
		exportIDs[exp.ID] = struct{}{}
		if exp.Path == "" || exp.Path == "." {
			return fmt.Errorf("exports.fragments[%d].path is required", i)
		}
		if filepath.IsAbs(exp.Path) || strings.HasPrefix(exp.Path, "../") || exp.Path == ".." {
			return fmt.Errorf("exports.fragments[%d].path %q must stay inside the bundle", i, exp.Path)
		}
		if !strings.HasSuffix(strings.ToLower(exp.Path), fragmentExtension) {
			return fmt.Errorf("exports.fragments[%d].path %q must name a %s fragment", i, exp.Path, fragmentExtension)
		}
		// A consumer materializes the fragment under its own lib/, where the
		// `lib/` confinement the unit loader enforces can reach it. A
		// producer that exports from elsewhere exports something no import
		// can name.
		if !strings.HasPrefix(exp.Path, DirFragments+"/") {
			return fmt.Errorf("exports.fragments[%d].path %q must live under %s/", i, exp.Path, DirFragments)
		}
	}

	dependencyNames := make(map[string]struct{}, len(dependencies.Fragments))
	for i := range dependencies.Fragments {
		dep := &dependencies.Fragments[i]
		dep.Name = strings.TrimSpace(dep.Name)
		dep.From = strings.TrimSpace(dep.From)
		dep.ID = strings.TrimSpace(dep.ID)
		if dep.Name == "" {
			return fmt.Errorf("dependencies.fragments[%d].name is required", i)
		}
		if strings.ContainsAny(dep.Name, `/\`) || dep.Name == "." || dep.Name == ".." {
			return fmt.Errorf("dependencies.fragments[%d].name %q must not contain path separators", i, dep.Name)
		}
		// The name becomes a path the install writes and records a line
		// about: whitespace or a control character is refused here rather
		// than after the fragment is already on disk.
		if strings.ContainsFunc(dep.Name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			return fmt.Errorf("dependencies.fragments[%d].name %q must not contain whitespace or control characters", i, dep.Name)
		}
		// The name is the STEM an `import "lib/<name>.bot"` spells: carrying
		// the extension would materialize `lib/x.bot.bot`, which no import
		// reaches.
		if strings.HasSuffix(strings.ToLower(dep.Name), fragmentExtension) {
			return fmt.Errorf("dependencies.fragments[%d].name %q must not carry the %s extension: it names the fragment the bundle imports as %s/%s%s", i, dep.Name, fragmentExtension, DirFragments, strings.TrimSuffix(dep.Name, fragmentExtension), fragmentExtension)
		}
		if _, duplicate := dependencyNames[dep.Name]; duplicate {
			// Two dependencies under one name would materialize over each
			// other, and which one won would depend on declaration order.
			return fmt.Errorf("dependencies.fragments[%d].name %q is duplicated", i, dep.Name)
		}
		dependencyNames[dep.Name] = struct{}{}
		if dep.From == "" {
			return fmt.Errorf("dependencies.fragments[%d].from is required: it names the bundle that exports %q", i, dep.Name)
		}
		if strings.ContainsAny(dep.From, `/\`) || dep.From == "." || dep.From == ".." {
			return fmt.Errorf("dependencies.fragments[%d].from %q must not contain path separators", i, dep.From)
		}
		if dep.ID != "" && (strings.ContainsAny(dep.ID, `/\`) || dep.ID == "." || dep.ID == "..") {
			return fmt.Errorf("dependencies.fragments[%d].id %q must not contain path separators", i, dep.ID)
		}
	}
	return nil
}
