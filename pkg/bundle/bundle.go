// Package bundle implements the `.botz` archive format: a ZIP archive
// that packages an iterion workflow (`main.bot`) with adjacent resources
// (skills, prompts, presets, default attachments, manifest). A downloaded
// `.botz` therefore extracts with `unzip` / double-click. Older bundles
// were gzipped tarballs (tar.gz) — those are still read transparently
// (the loader auto-detects the container format via magic bytes), so the
// migration is backward-compatible. A bundle is loaded once per run,
// extracted into a content-addressed cache directory, and then exposed to
// the engine as a *Bundle so skills/prompts become visible to claude_code
// and the claw tool registry without authoring changes.
//
// The bundle content hash (Bundle.Hash / PackResult.Hash) is computed
// over the LOGICAL content — the sorted sequence of (relative-path,
// file-bytes) — independent of the container format, so the same files
// hash identically whether packed as ZIP or read from a legacy tar.gz.
package bundle

import (
	"os"
	"path/filepath"
)

// Layout directory names. A bundle resolves each by convention at its
// root, so these strings ARE the format — spelling one differently
// silently disables that resource kind.
const (
	// DirSkills holds `SKILL.md` files mirrored into the run
	// workspace's `.claude/skills/`.
	DirSkills = "skills"
	// DirPrompts holds reusable `.md` prompts; the filename stem
	// becomes the prompt name.
	DirPrompts = "prompts"
	// DirAttachments holds default binary inputs referenced from the
	// manifest's `attachments:` map.
	DirAttachments = "attachments"
	// DirPresets holds file-based presets (named sous-bots).
	DirPresets = "presets"
)

// LayoutDirs is the canonical order of the layout directories, shared by
// the loader (which resolves them), the packer (which archives them),
// and pkg/botscaffold (which creates them). Kept as one list so adding a
// convention directory is not a three-package hunt.
var LayoutDirs = []string{DirSkills, DirPrompts, DirAttachments, DirPresets}

// Bundle root file names. Like the layout directories, these strings ARE
// the format: several packages outside pkg/bundle reach into a bundle by
// name, and they must all spell it the same way.
const (
	// MainBotFile is the workflow source at a bundle's root — the
	// familiar main.go / main.rs convention, independent of the bundle
	// directory's own name.
	MainBotFile = "main.bot"
	// ManifestFile is the bundle manifest.
	ManifestFile = "manifest.yaml"
	// ManifestFileAlt is the accepted `.yml` spelling of ManifestFile.
	ManifestFileAlt = "manifest.yml"
)

// dirMarkers are the sibling entries that mark a directory as a bundle
// rather than somewhere a loose main.bot happens to sit.
var dirMarkers = []string{DirSkills, ManifestFile}

// DirForMainBot returns the bundle directory holding path, or "" when
// path is not a bundle's main.bot.
//
// Callers outside pkg/bundle need this to decide whether to open a
// workflow as a bundle (picking up its skills, prompts, presets and
// attachments) or as a loose file. It lives here because it encodes what
// a bundle IS — when two packages answered that question with their own
// copy of the marker list, they could disagree about it after any change
// to the layout.
func DirForMainBot(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if filepath.Base(abs) != MainBotFile {
		return ""
	}
	parent := filepath.Dir(abs)
	for _, marker := range dirMarkers {
		if _, err := os.Stat(filepath.Join(parent, marker)); err == nil {
			return parent
		}
	}
	return ""
}

// Kind discriminates how a workflow path was supplied.
type Kind int

const (
	// KindBot is a plain `.bot` source file.
	KindBot Kind = iota
	// KindBundle is a `.botz` archive (ZIP; older bundles: tar.gz).
	KindBundle
	// KindBundleDir is a directory whose root already contains a
	// recognised bundle layout (`main.bot` at the top).
	// Useful for dev workflows that author bundles in-place.
	KindBundleDir
)

func (k Kind) String() string {
	switch k {
	case KindBot:
		return "bot"
	case KindBundle:
		return "bundle"
	case KindBundleDir:
		return "bundle-dir"
	}
	return "unknown"
}

// Bundle is a resolved, on-disk bundle ready for runtime consumption.
// All path fields are absolute; optional resource directories are the
// empty string when not present in the bundle.
type Bundle struct {
	// Dir is the absolute path of the extracted (or in-place) bundle
	// root. Engine consumers should treat it as read-only.
	Dir string

	// Manifest holds the parsed `manifest.yaml`. Nil when the bundle
	// omits the file (allowed — the field is optional).
	Manifest *Manifest

	// IterPath is the absolute path of the workflow source file
	// inside the bundle (`main.bot`, at the bundle root).
	IterPath string

	// SkillsDir is `<Dir>/skills` when the directory exists, else "".
	SkillsDir string

	// PromptsDir is `<Dir>/prompts` when the directory exists, else "".
	PromptsDir string

	// AttachmentsDir is `<Dir>/attachments` when the directory exists,
	// else "". Holds pre-bundled default values for the workflow's
	// `attachments:` block — runtime uploads (Launch modal) override.
	AttachmentsDir string

	// PresetsDir is `<Dir>/presets` when the directory exists, else "".
	// Holds file-based presets (`<name>.md`, YAML frontmatter + prompt
	// body) — named sous-bots that bias the workflow at launch. Parsed
	// by LoadPresets; merged into the runtime workflow's preset set by
	// the engine at run start.
	PresetsDir string

	// Hash is the SHA-256 of the bundle's logical content (the sorted
	// (relative-path, file-bytes) sequence), used as the cache key. It is
	// independent of the container format, so a ZIP bundle and a legacy
	// tar.gz bundle with identical files share the same hash. Empty for
	// KindBundleDir bundles (no archive to hash; callers handle directory
	// bundles per-run).
	Hash string

	// SourcePath is the original `.botz` filesystem path for KindBundle,
	// or the source directory for KindBundleDir. Persisted with the run
	// so resume can re-extract from the same archive after a cache GC.
	SourcePath string

	// Kind discriminates how the bundle was supplied.
	Kind Kind
}
