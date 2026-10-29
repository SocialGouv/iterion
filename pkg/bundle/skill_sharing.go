package bundle

import (
	"fmt"
	"strings"
	"unicode"
)

// skillExtension is the extension a skill document carries. pkg/skilllib owns
// the skill format; this is the one spelling a manifest key may construct, and
// validateSkillSharing refuses any other.
const skillExtension = ".md"

// ExportedID is the producer-side id this dependency resolves. It defaults to
// the consumer-side name rather than to the empty string: a dependency that
// resolves nothing has no written form. decodeManifest already fills it in,
// so this answers for a value a caller built in code too.
func (d SkillDependency) ExportedID() string {
	if d.ID == "" {
		return d.Name
	}
	return d.ID
}

// SkillRelPath is the bundle-relative, slash-separated path the install
// materializes this dependency at: where a shipped copy would have been
// (ADR-144 clause 2), so the run-start mirror, the install's skill count and
// `validate`'s skill-file lints keep reading what they read today instead of
// learning that a skill can be a dependency.
func (d SkillDependency) SkillRelPath() string {
	return DirSkills + "/" + d.Name + skillExtension
}

// refuseUnmaterializableSkillName refuses a dependency name that cannot
// become the file the install writes for it. The name is a PATH component and
// a line in the bundle's local-state record, so a name no install could
// materialize dies in the manifest rather than halfway through a sync.
func refuseUnmaterializableSkillName(i int, dep SkillDependency) error {
	if strings.ContainsFunc(dep.Name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("dependencies.skills[%d].name %q must not contain whitespace or control characters", i, dep.Name)
	}
	// The name is the STEM a `skills:` list spells: carrying the extension
	// would materialize `skills/x.md.md`, which no reference reaches.
	if strings.HasSuffix(strings.ToLower(dep.Name), skillExtension) {
		stem := SkillDependency{Name: strings.TrimSuffix(dep.Name, skillExtension)}
		return fmt.Errorf("dependencies.skills[%d].name %q must not carry the %s extension: it names the skill a `skills:` list spells, which the install materializes at %s", i, dep.Name, skillExtension, stem.SkillRelPath())
	}
	// One path, one owner. The generated bot catalog of a materialized
	// router bundle is the OTHER kind of local state at exactly this path
	// (LocalStateCatalog, kept bytes and all): a dependency that took it
	// would have two writers reconciling one line, each erasing the other's.
	if dep.SkillRelPath() == DirSkills+"/"+GeneratedCatalogFile {
		return fmt.Errorf("dependencies.skills[%d].name %q is the generated bot catalog at %s/%s, which `iterion bots regen-catalog` owns: a shared skill cannot be declared under that name", i, dep.Name, DirSkills, GeneratedCatalogFile)
	}
	return nil
}
