package clawfold

import (
	"bufio"
	"bytes"
	"crypto/sha1" // #nosec G505 -- git's object id, not a security primitive
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/internal/envtrust"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// Entry is one file of a git tree.
type Entry struct {
	Path string // slash-separated, relative to the tree root
	Mode string // git mode: 100644, 100755 or 120000
	Blob string // object id
}

// Tree is the content of one commit of the source repository: every file,
// its mode, its blob id and its bytes.
type Tree struct {
	Commit  string
	Entries map[string]Entry
	content map[string][]byte // by object id
}

// Content returns the bytes of the file at path.
func (t *Tree) Content(path string) ([]byte, bool) {
	e, ok := t.Entries[path]
	if !ok {
		return nil, false
	}
	data, ok := t.content[e.Blob]
	return data, ok
}

// BlobID is the git object id of data stored as a blob, in the hash the
// object id's length names: 40 hex digits for SHA-1, 64 for SHA-256.
func BlobID(data []byte, idLen int) string {
	header := []byte("blob " + strconv.Itoa(len(data)) + "\x00")
	if idLen == 64 {
		h := sha256.New()
		h.Write(header)
		h.Write(data)
		return hex.EncodeToString(h.Sum(nil))
	}
	h := sha1.New() // #nosec G401 -- git's object id, not a security primitive
	h.Write(header)
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// git runs git read-only against repositories this tool names itself: the
// redirection variables are dropped (the command answers about the
// directory given, not about one the environment points at) and no hook or
// fsmonitor of that repository runs.
type git struct{ path string }

func newGit() (git, error) {
	path, err := envtrust.LookPath("git")
	if err != nil {
		return git{}, fmt.Errorf("locate git: %w", err)
	}
	return git{path: path}, nil
}

func (g git) output(dir string, stdin io.Reader, args ...string) ([]byte, error) {
	// --no-replace-objects: a `refs/replace` entry (or GIT_REPLACE_REF_BASE)
	// would make cat-file answer the requested id with another object's
	// bytes, so the tree read would not be the pin it names.
	cmd := exec.Command(g.path, gitlib.NoRunHooks(append([]string{"--no-replace-objects"}, args...)...)...) // #nosec G204 -- argv built by this package
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	cmd.Dir = dir
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s in %s: %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ErrPinNotInSource is returned when the source repository does not hold
// the pinned commit.
var ErrPinNotInSource = errors.New("pin not found in the source repository")

// ReadTree loads every file of rev in the repository at repo.
func ReadTree(repo, rev string) (*Tree, error) {
	g, err := newGit()
	if err != nil {
		return nil, err
	}
	commitOut, err := g.output(repo, nil, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("%w: %s in %s (fetch it: git -C %s fetch origin — a shallow clone needs --unshallow): %v", ErrPinNotInSource, rev, repo, repo, err)
	}
	commit := strings.TrimSpace(string(commitOut))
	listing, err := g.output(repo, nil, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	tree := &Tree{Commit: commit, Entries: map[string]Entry{}, content: map[string][]byte{}}
	var ids []string
	for _, record := range bytes.Split(listing, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		meta, path, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("ls-tree record without a path: %q", record)
		}
		fields := strings.Fields(string(meta))
		if len(fields) != 3 {
			return nil, fmt.Errorf("ls-tree record %q: want mode, type and id", meta)
		}
		if fields[1] != "blob" {
			continue // a submodule commit carries no content of this tree
		}
		e := Entry{Path: string(path), Mode: fields[0], Blob: fields[2]}
		tree.Entries[e.Path] = e
		if _, seen := tree.content[e.Blob]; !seen {
			tree.content[e.Blob] = nil
			ids = append(ids, e.Blob)
		}
	}
	if err := g.readBlobs(repo, ids, tree.content); err != nil {
		return nil, err
	}
	return tree, nil
}

func (g git) readBlobs(repo string, ids []string, into map[string][]byte) error {
	if len(ids) == 0 {
		return nil
	}
	raw, err := g.output(repo, strings.NewReader(strings.Join(ids, "\n")+"\n"), "cat-file", "--batch")
	if err != nil {
		return err
	}
	r := bufio.NewReader(bytes.NewReader(raw))
	for _, want := range ids {
		header, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("cat-file --batch: header for %s: %w", want, err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != want || fields[1] != "blob" {
			return fmt.Errorf("cat-file --batch: want blob %s, got %q", want, strings.TrimSpace(header))
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return fmt.Errorf("cat-file --batch: size of %s: %w", want, err)
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return fmt.Errorf("cat-file --batch: body of %s: %w", want, err)
		}
		if _, err := r.ReadByte(); err != nil { // the newline after each body
			return fmt.Errorf("cat-file --batch: terminator of %s: %w", want, err)
		}
		// What the object store returns is checked, not trusted: a body
		// that does not hash to the id asked for is not that blob.
		if got := BlobID(data, len(want)); got != want {
			return fmt.Errorf("cat-file --batch: object %s holds bytes hashing to %s; the source's object store is corrupt or rewritten", want, got)
		}
		into[want] = data
	}
	return nil
}

// ObjectFormat returns the hash a repository names its objects with.
func ObjectFormat(repo string) (string, error) {
	g, err := newGit()
	if err != nil {
		return "", err
	}
	out, err := g.output(repo, nil, "rev-parse", "--show-object-format")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ModuleFiles returns the files of tree a module zip of it holds — what
// `go mod download` hashes into go.sum's h1: — sorted. The rules are
// golang.org/x/mod/zip's own (checkFiles), as the toolchain vendors them:
// regular files only; none in a nested module (a directory holding a
// regular file named go.mod, in any case); none of a vendored package, under
// the rule of the go version the root go.mod names; no .hg_archival.txt.
func ModuleFiles(tree *Tree) []string {
	nested := map[string]bool{}
	for p, e := range tree.Entries {
		if d := path.Dir(p); strings.EqualFold(path.Base(p), "go.mod") && d != "." && regular(e) {
			nested[d] = true
		}
	}
	vers := goLangVersion(tree)
	var files []string
	for p, e := range tree.Entries {
		if !regular(e) || isVendoredPackage(p, vers) || inNested(p, nested) || p == ".hg_archival.txt" {
			continue
		}
		files = append(files, p)
	}
	sort.Strings(files)
	return files
}

func regular(e Entry) bool { return e.Mode == "100644" || e.Mode == "100755" }

// droppedFiles names, with the reason, every file of tree the module zip
// leaves out.
func droppedFiles(tree *Tree) map[string]string {
	kept := map[string]bool{}
	for _, p := range ModuleFiles(tree) {
		kept[p] = true
	}
	nested := map[string]bool{}
	for p, e := range tree.Entries {
		if d := path.Dir(p); strings.EqualFold(path.Base(p), "go.mod") && d != "." && regular(e) {
			nested[d] = true
		}
	}
	vers := goLangVersion(tree)
	dropped := map[string]string{}
	for p, e := range tree.Entries {
		switch {
		case kept[p]:
		case !regular(e):
			dropped[p] = "not a regular file"
		case isVendoredPackage(p, vers):
			dropped[p] = "a vendored path"
		case inNested(p, nested):
			dropped[p] = "in a nested module"
		default:
			dropped[p] = "dropped by the go command"
		}
	}
	return dropped
}

func inNested(p string, nested map[string]bool) bool {
	for d := path.Dir(p); d != "."; d = path.Dir(d) {
		if nested[d] {
			return true
		}
	}
	return false
}

var goDirective = regexp.MustCompile(`(?m)^\s*go\s+(\d+)\.(\d+)`)

// goLangVersion is the language version the root go.mod names, as major and
// minor, or zero when it names none.
func goLangVersion(tree *Tree) [2]int {
	// x/mod reads the version from a regular root go.mod only.
	if e, ok := tree.Entries["go.mod"]; !ok || !regular(e) {
		return [2]int{}
	}
	data, _ := tree.Content("go.mod")
	m := goDirective.FindSubmatch(data)
	if m == nil {
		return [2]int{}
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	return [2]int{major, minor}
}

// isVendoredPackage is golang.org/x/mod/zip's predicate: from go 1.24 on,
// vendor/modules.txt counts as vendored and the "/vendor/" offset is right;
// before, the offset quirk stays, because the published checksums depend on
// it.
func isVendoredPackage(name string, vers [2]int) bool {
	go124 := vers[0] > 1 || (vers[0] == 1 && vers[1] >= 24)
	if go124 && name == "vendor/modules.txt" {
		return true
	}
	var i int
	if strings.HasPrefix(name, "vendor/") {
		i += len("vendor/")
	} else if j := strings.Index(name, "/vendor/"); j >= 0 {
		if go124 {
			i = j + len("/vendor/")
		} else {
			i += len("/vendor/")
		}
	} else {
		return false
	}
	return strings.Contains(name[i:], "/")
}

// archiveRules lists the .gitattributes lines of tree that set an attribute
// through which `git archive` — how the module zip of a git repository is
// built — may write other bytes than the blobs this tool reads, or drop
// files; whether one does depends on the contents, so they are leads.
func archiveRules(tree *Tree) []string {
	var rules []string
	for p := range tree.Entries {
		if path.Base(p) != ".gitattributes" {
			continue
		}
		data, _ := tree.Content(p)
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
				continue
			}
			// The attributes, after the pattern; an unset (-x) or unspecified
			// (!x) attribute transforms nothing.
			for _, f := range fields[1:] {
				name, _, _ := strings.Cut(f, "=")
				switch name {
				case "export-ignore", "export-subst", "eol", "text", "filter", "ident", "working-tree-encoding":
					rules = append(rules, p+": "+strings.TrimSpace(line))
				default:
					continue
				}
				break
			}
		}
	}
	sort.Strings(rules)
	return rules
}

// ModuleHash is go.sum's h1: hash of the module zip of tree published as
// module@version (golang.org/x/mod/sumdb/dirhash.Hash1).
func ModuleHash(tree *Tree, module, version string) (string, error) {
	h := sha256.New()
	for _, p := range ModuleFiles(tree) {
		if strings.Contains(p, "\n") {
			return "", fmt.Errorf("file name %q holds a newline: no module hash covers it", p)
		}
		data, _ := tree.Content(p)
		fmt.Fprintf(h, "%x  %s\n", sha256.Sum256(data), module+"@"+version+"/"+p)
	}
	return "h1:" + base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// moduleView is tree reduced to its module zip: what the fold may carry.
func moduleView(tree *Tree) *Tree {
	view := &Tree{Commit: tree.Commit, Entries: map[string]Entry{}, content: tree.content}
	for _, p := range ModuleFiles(tree) {
		view.Entries[p] = tree.Entries[p]
	}
	return view
}
