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
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"go.yaml.in/yaml/v2"
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
// rather than somewhere a loose main.bot happens to sit. A manifest marks
// only when it is iterion's (manifestIsIterions): `manifest.yaml` and
// `manifest.yml` are common filenames of other tools, and a bundle that
// carries a marker but does not open is refused on every surface — so a
// foreign manifest beside a loose main.bot must mark nothing, while an
// iterion manifest that does not decode must still mark its bundle, so the
// open fails loudly instead of the run starting without its prompts and
// skills. Both spellings count, as the loader reads both: a marker the
// loader reads but the promotion ignored gave the file and the directory
// forms of the same bundle two verdicts. A manifest that decodes as
// iterion's but carries only generic keys (a foreign `name`+`version`
// file) marks too — as it did before any of this, and harmlessly: the
// loader accepts it and there is nothing beside it to merge.
var dirMarkers = []string{DirSkills, ManifestFile, ManifestFileAlt}

// genericManifestKeys are the Manifest's top-level keys every tool's
// manifest may carry too: their presence says nothing about whose file it
// is. Pinned by TestManifestKeysAreDerivedFromTheStruct — widening it
// would silently stop marking bundles.
var genericManifestKeys = map[string]bool{
	"name": true, "version": true, "description": true, "author": true,
	"icon": true, "enabled": true, "triggers": true, "repo": true,
}

// manifestStructKeys are the Manifest's top-level yaml keys, derived from
// the struct's tags (yaml.v2 reads an untagged field under its lowercased
// name), so a field added to the struct is known here without a second
// list to keep.
var manifestStructKeys = func() map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(Manifest{})
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		switch tag {
		case "-":
			continue
		case "":
			tag = strings.ToLower(f.Name)
		}
		keys[tag] = true
	}
	if len(keys) == 0 {
		panic("bundle: the Manifest struct declares no yaml key")
	}
	return keys
}()

// iterionOnlyKeys are manifestStructKeys minus genericManifestKeys: a key
// that only an iterion manifest carries.
var iterionOnlyKeys = func() map[string]bool {
	out := map[string]bool{}
	for k := range manifestStructKeys {
		if !genericManifestKeys[k] {
			out[k] = true
		}
	}
	return out
}()

// iterionOnlyKeyRe matches a key only an iterion manifest has, at line
// start — the text fallback for a file the YAML parser cannot read at
// all. Only the distinctive keys: a templated or tab-broken manifest of
// another tool carries `name:` at line start too, and matching it would
// refuse a loose main.bot beside it on every surface.
var iterionOnlyKeyRe = func() *regexp.Regexp {
	var keys []string
	for k := range iterionOnlyKeys {
		keys = append(keys, regexp.QuoteMeta(k))
	}
	sort.Strings(keys)
	return regexp.MustCompile(`(?m)^(` + strings.Join(keys, "|") + `)\s*:`)
}()

// maxManifestProbe bounds what manifestVerdict reads: a manifest is small,
// and this runs on every path a workflow is opened from. A file past it
// is not ours — judging the half a bound left readable would be judging
// another document.
const maxManifestProbe = 1 << 20

// manifestIsIterions reports whether path is a manifest file that is
// iterion's; the reason it is not, when it is not, is manifestVerdict's.
func manifestIsIterions(path string) bool {
	ok, _ := manifestVerdict(path)
	return ok
}

// manifestVerdict reads whether path is an iterion manifest off its
// SHAPE, never its values (whether it then opens is the loader's verdict,
// which must stay loud): a document whose top-level keys are all keys of
// ours is ours — a valid manifest may carry only generic ones,
// `schema_version` included since the loader defaults it; a broken one
// whose only defect is a value (an over-long icon, a bad `enabled`) is
// still ours and must still mark its bundle, so the open fails by name
// instead of the run starting without its prompts and skills; and so is an
// empty or comment-only one, which the loader opens as a manifest of no
// fields, as it always did. A document with foreign keys is another tool's
// unless it also carries a key only ours have. A file the YAML parser
// cannot read is judged on its text: a key only ours have at line start
// marks it. So a `manifest.yaml` of another tool beside a loose main.bot
// marks nothing (Helm, an extension, a compose file, a CI workflow — all
// carry keys of their own), and only a foreign file whose every key
// happens to be one of ours is refused, with the remedy in the message.
// The reason is "" when the file is absent, and names what kept a present
// file from marking otherwise — a typo in the only distinctive key would
// else drop a bundle in silence (ForeignManifestBeside surfaces it).
//
// The open is non-blocking: a fifo named manifest.yaml would otherwise
// park the caller until a writer shows up, and this runs inside HTTP
// handlers (validate on every debounced keystroke, launch, the board
// projection) with no timeout of their own — O_NONBLOCK returns at once
// and the handle's Stat then refuses it, with no window between two
// syscalls. Stat follows a symlink, so a linked manifest stays one.
func manifestVerdict(path string) (ok bool, reason string) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return false, ""
		}
		return false, "cannot be opened: " + err.Error()
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return false, "is not a regular file"
	}
	head, err := io.ReadAll(io.LimitReader(f, maxManifestProbe+1))
	if err != nil {
		return false, "cannot be read: " + err.Error()
	}
	if len(head) > maxManifestProbe {
		return false, "exceeds 1 MiB; no manifest of ours does"
	}
	var doc map[string]any
	if err := yaml.Unmarshal(head, &doc); err != nil {
		if iterionOnlyKeyRe.Match(head) {
			return true, ""
		}
		return false, "is not YAML this parser reads (" + strings.SplitN(err.Error(), "\n", 2)[0] + ") and carries no key only an iterion manifest has"
	}
	var unknown []string
	ours := 0
	for k := range doc {
		switch {
		case iterionOnlyKeys[k]:
			ours++
		case manifestStructKeys[k]:
		default:
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 || ours > 0 {
		return true, ""
	}
	sort.Strings(unknown)
	return false, "carries top-level keys no iterion manifest has (" + strings.Join(unknown, ", ") + ") and none only ours have"
}

// ForeignManifestBeside reports a file named like a manifest beside a
// main.bot that DirForMainBot did NOT read as the bundle's manifest, and
// why — the one outcome that would otherwise be silent: a typo in the only
// distinctive key, a file the parser cannot read, one over the size
// bound. The file then compiles alone, without the prompts, presets and
// skills beside it; validate says so. ("", "") when no such file exists,
// or when the sibling does mark.
func ForeignManifestBeside(mainBot string) (manifest, reason string) {
	abs, err := filepath.Abs(mainBot)
	if err != nil || filepath.Base(abs) != MainBotFile {
		return "", ""
	}
	for _, name := range []string{ManifestFile, ManifestFileAlt} {
		p := filepath.Join(filepath.Dir(abs), name)
		if ok, why := manifestVerdict(p); !ok && why != "" {
			return p, why
		}
	}
	return "", ""
}

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
		p := filepath.Join(parent, marker)
		if marker == DirSkills {
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				return parent
			}
			continue
		}
		if manifestIsIterions(p) {
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

// Name is the bundle's declared id — the manifest's `name:` — or "" when
// there is no bundle, or a bundle without a manifest (a directory marked
// by `skills/` alone is one). Every reader of the id goes through here:
// the Manifest field is nil in that second case, and a bare dereference
// of it took a dispatcher daemon down on its first dispatch.
func (b *Bundle) Name() string {
	if b == nil || b.Manifest == nil {
		return ""
	}
	return b.Manifest.Name
}
