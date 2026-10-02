package floorsalign

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// Options is one run of the aligner.
type Options struct {
	// Root is the repository root (release-it's cwd, the Taskfile's dir).
	Root string
	// Apply writes the realignments; without it the run only reports, and
	// holds the tree to the release test's rule.
	Apply bool
	// Floors overrides the registry (tests inject a fixture's pins); nil is
	// bundle.SyntaxFloors.
	Floors func() map[string]string
	// Stdout receives the report lines; nil discards them.
	Stdout io.Writer
}

// Run plans the cut against the registry, refuses what it cannot realign,
// and — in apply mode — rewrites the pins the cut claims.
func Run(opts Options) error {
	out := opts.Stdout
	if out == nil {
		out = io.Discard
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return err
	}
	floors := opts.Floors
	if floors == nil {
		floors = bundle.SyntaxFloors
	}
	registry := floors()
	if len(registry) == 0 {
		return errors.New("the floor registry is empty: this is not the tree the release step realigns")
	}
	pins, err := ResolvePins(root, registry)
	if err != nil {
		return err
	}
	version, err := packageVersion(root)
	if err != nil {
		return err
	}
	changelog, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		return err
	}
	if opts.Apply {
		committed, err := committedVersion(root)
		if err != nil {
			return err
		}
		if committed == version {
			return fmt.Errorf("package.json carries %s, the version HEAD already carries: no release is being cut — --apply belongs to release-it's before:git:beforeRelease hook, on the bumped tree it is about to commit; preview without it", version)
		}
	}

	mode := "preview"
	if opts.Apply {
		mode = "cutting"
	}
	fmt.Fprintf(out, "release-floors: %s %s (newest release in CHANGELOG.md: %s)\n", mode, version, bundle.NewestChangelogRelease(string(changelog)))
	actions := Plan(pins, version, string(changelog), opts.Apply)
	var refusals []string
	for _, a := range actions {
		switch a.Kind {
		case Refuse:
			refusals = append(refusals, a.Detail)
		case Realign:
			// Reported once the file is written, never before: a line
			// naming a rewrite that did not happen is worse than no line.
		default:
			fmt.Fprintf(out, "release-floors: %s\n", a.Detail)
		}
	}
	// Every rewrite is computed before any is written, and a literal the
	// cut cannot locate joins the refusals: the release is refused with the
	// tree untouched, never with half the floors moved and half not — a
	// state no later cut can read, since the moved ones would then name a
	// release nobody cut.
	writes, planErrs := rewrites(root, actions, opts.Apply)
	refusals = append(refusals, planErrs...)
	if len(refusals) > 0 {
		verdict := "the syntax floors do not hold (the release test says the same)"
		if opts.Apply {
			verdict = "refusing the release"
		}
		return errors.New(verdict + ":\n  - " + strings.Join(refusals, "\n  - "))
	}
	// The tree moves all at once or not at all. Every rewrite is staged
	// beside its file first; only once every stage succeeded are they
	// renamed into place. A run that wrote as it went would leave, on any
	// I/O failure, some floors naming the release being cut and some naming
	// the pending minor — a split no later cut can read (the moved ones
	// name a release nobody cut) and one `git checkout` of the bumped files
	// does not undo.
	staged := make([]string, len(writes))
	for i, w := range writes {
		staged[i] = w.path + ".floors-tmp"
		if err := os.WriteFile(staged[i], w.content, w.perm); err != nil {
			discard(staged[:i])
			return fmt.Errorf("staging the realignment of %s: %w", w.file, err)
		}
	}
	for i, w := range writes {
		if err := os.Rename(staged[i], w.path); err != nil {
			discard(staged[i:])
			return fmt.Errorf("realigning %s: %w — %s already carry the release being cut; restore them from HEAD before cutting again", w.file, err, alreadyMoved(writes[:i]))
		}
	}
	for _, w := range writes {
		for _, detail := range w.details {
			fmt.Fprintf(out, "release-floors: %s\n", detail)
		}
	}
	return nil
}

// discard removes the stages a refused run leaves behind. A stage that
// cannot be removed is reported beside the failure it follows, never
// instead of it.
func discard(stages []string) {
	for _, path := range stages {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "release-floors: %s could not be removed: %v\n", path, err)
		}
	}
}

func alreadyMoved(writes []*pinWrite) string {
	if len(writes) == 0 {
		return "no file"
	}
	files := make([]string, 0, len(writes))
	for _, w := range writes {
		files = append(files, w.file)
	}
	return strings.Join(files, ", ")
}

// pinWrite is one file's realignments, resolved to the bytes they will
// write. A file is staged ONCE and each of its pins applied to what the
// previous one produced: two floors declared in the same file (ImportSince
// and ProfileSince[2] both live in preamble.go) must both survive, where
// re-reading the file per pin would write one and drop the other.
type pinWrite struct {
	file    string // relative to the repo root, for the error
	path    string
	perm    os.FileMode
	content []byte
	details []string
}

// rewrites resolves every realignment the plan carries against the files on
// disk. An unreadable file or a literal the cut cannot locate is a refusal
// like any other, so it joins the same verdict instead of aborting a run
// that has already written.
func rewrites(root string, actions []Action, apply bool) ([]*pinWrite, []string) {
	if !apply {
		return nil, nil
	}
	var writes []*pinWrite
	var refusals []string
	staged := map[string]*pinWrite{}
	for _, a := range actions {
		if a.Kind != Realign {
			continue
		}
		path := filepath.Join(root, a.Pin.File)
		w, ok := staged[path]
		if !ok {
			info, err := os.Stat(path)
			if err != nil {
				refusals = append(refusals, fmt.Sprintf("%s: %v", a.Pin.Name, err))
				continue
			}
			src, err := os.ReadFile(path)
			if err != nil {
				refusals = append(refusals, fmt.Sprintf("%s: %v", a.Pin.Name, err))
				continue
			}
			w = &pinWrite{file: a.Pin.File, path: path, perm: info.Mode().Perm(), content: src}
			staged[path] = w
			writes = append(writes, w)
		}
		rewritten, err := ApplyPin(w.content, a.Pin, a.To)
		if err != nil {
			refusals = append(refusals, err.Error())
			continue
		}
		w.content = rewritten
		w.details = append(w.details, a.Detail)
	}
	return writes, refusals
}

func packageVersion(root string) (string, error) {
	src, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return "", err
	}
	return versionOf(src, filepath.Join(root, "package.json"))
}

// versionOf reads the top-level "version" of a package.json. The document is
// parsed rather than matched: a nested "version" above the top-level one
// would otherwise answer for it, and both readers below would then agree on
// the wrong number and refuse every cut.
func versionOf(src []byte, where string) (string, error) {
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(src, &doc); err != nil {
		return "", fmt.Errorf("%s is not readable JSON: %w", where, err)
	}
	if doc.Version == "" {
		return "", fmt.Errorf("%s carries no version", where)
	}
	return doc.Version, nil
}

// committedVersion is the version HEAD's package.json carries. A cut is the
// one state where it differs from the working tree's: release-it has bumped
// the number and not yet committed it. The witness is the VERSION and not
// the file, because any other uncommitted edit to package.json — a
// dependency added, a script renamed — would otherwise read as a cut, and
// --apply would then move a pending pin onto a release already cut without
// the syntax.
func committedVersion(root string) (string, error) {
	cmd := exec.Command("git", "-C", root, "show", "HEAD:package.json")
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git show HEAD:package.json in %s: %v: %s", root, err, strings.TrimSpace(stderr.String()))
	}
	return versionOf([]byte(stdout.String()), "the package.json HEAD carries in "+root)
}
