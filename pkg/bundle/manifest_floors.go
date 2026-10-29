package bundle

// The manifest keys a release floor is asked for by, spelled as
// DeclaredManifestKeys reads them — the one table of floors
// (syntaxFloors) is what reads them, never a check of its own.
const (
	keyCategory           = "category"
	keyTags               = "tags"
	keyExportsSkills      = "exports.skills"
	keyDependenciesSkills = "dependencies.skills"
)

// CatalogTaxonomySince is the release whose manifest decoder knows the
// catalogue taxonomy keys: top-level `category` and `tags`, the six-verb
// navigation spine every bot picker groups by. A build below it meets
// either key in a manifest, decodes strictly, knows no such field and
// refuses the whole document — so what the floor buys is the actionable
// sentence at authoring time ("this bundle needs iterion >= X") instead of
// a decode failure on a pod.
const CatalogTaxonomySince = "3.225.0"

// SkillSharingSince is the release whose manifest decoder knows the shared
// skill keys: `exports.skills` and `dependencies.skills` (ADR-144). It is
// the symbolic pin until the lot that ships them is cut, which is what
// keeps it strictly above every released build: a bundle declaring either
// key needs a build that reads it, and any build that does not refuses the
// whole manifest at its strict decode — the field is not one it knows.
const SkillSharingSince = "next"
