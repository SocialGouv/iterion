package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/overlay"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

// pinsDirName holds the install records, beside the packages rather than
// inside them: a record written into a package would change the very content
// hash it certifies, and `FSCatalog.Connectors` would list the directory as a
// connector id. A dot name for the same reason `.origins` is one for bots.
const pinsDirName = ".pins"

// ConnectorsInstallOptions drives `iterion connectors install`.
type ConnectorsInstallOptions struct {
	// Source is a git URL, or a path to a repository on this machine.
	Source string
	// Ref is the PIN: the commit the package is read from. A branch or a tag
	// is refused — see ConnectorsInstall.
	Ref string
	// Path is the package directory inside the repository. Empty means the
	// repository root is itself the package.
	Path string
	// Dest is the catalog ROOT to install into, default the operator's own
	// tier. The package lands under the id it declares.
	Dest string
	// Force replaces a package already installed under that id.
	Force bool
}

// ConnectorPin is the install record of a git-sourced package: what was
// installed, from where, at which commit, and the content hash of the bytes
// that passed validation.
//
// It is provenance an operator reads, never an input a resolution branches on:
// the installed directory IS the package a run gets, and a record that could
// redirect or re-verify at resolution time would put a remote host back in the
// path of every action node.
type ConnectorPin struct {
	Connector     string    `json:"connector"`
	Version       string    `json:"version"`
	Source        string    `json:"source"`
	Ref           string    `json:"ref"`
	SourcePath    string    `json:"source_path,omitempty"`
	ContentSHA256 string    `json:"content_sha256"`
	InstalledAt   time.Time `json:"installed_at"`
}

// ConnectorsInstall installs a connector package from a git source at a pinned
// commit, into the catalog tier a run reads.
//
// Three things happen at INSTALL time, and none of them at resolution time:
// the commit is fetched, the package is validated with the loader a run uses,
// and the bytes that passed are hashed. A tier that fetched while a run
// resolved an operation would make every action node depend on a remote host
// answering — and the fetched package is what decides where a connection's
// credential goes on the wire, so "what serves" must be bytes an operator
// installed, not bytes a source serves today.
//
// The ref must be a commit pin. A branch is not a pin by any amount of
// convention: the same command run twice would install different operations
// under the same connector id, and the only word on which ones a run got would
// be the time it ran. A tag is refused for the same reason — it is a pointer
// its publisher can move.
func ConnectorsInstall(ctx context.Context, opts ConnectorsInstallOptions, out io.Writer) error {
	staging, err := os.MkdirTemp("", "iterion-connector-pin-*")
	if err != nil {
		return fmt.Errorf("connectors: stage the fetch: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	// The fetch FIRST, and with it the only place that says what a pin is
	// (git.MaterializePin, which refuses a ref that can move): a second
	// "looks like a sha" test here would be a guard covering a guard —
	// each one looking tested while neither is — and it would also create
	// the catalog root on the way to refusing the command.
	checkout := filepath.Join(staging, "checkout")
	if err := gitlib.MaterializePin(ctx, opts.Source, opts.Ref, checkout); err != nil {
		return fmt.Errorf("connectors: fetch %s at %s: %w", sourceDescription(opts), opts.Ref, err)
	}
	pkgDir, err := packageDirWithin(checkout, opts.Path)
	if err != nil {
		return err
	}
	root, err := installCatalogRoot(opts.Dest)
	if err != nil {
		return err
	}

	// The SAME loader a run uses — generated half plus overlay, then the
	// complete check (pkg/connection.FSCatalog.Package, `iterion connectors
	// validate`). Reading the generated half alone here would install a
	// package whose authored corrections nobody checked, which is the defect
	// ADR-098 records: five auth schemes at run time against two in
	// validation.
	pkg, err := overlay.LoadPackage(pkgDir)
	if err != nil {
		return fmt.Errorf("connectors: %s at %s does not load, so it is not installed: %w", sourceDescription(opts), opts.Ref, err)
	}
	// The CATALOG's own rule on the id the PACKAGE declares, because that id
	// is the directory name this function writes: a package calling itself
	// `../evil` would otherwise land outside the tier, and one calling itself
	// `google.drive` would install cleanly and never resolve.
	if err := connection.CheckConnectorID(pkg.Connector.ID); err != nil {
		return fmt.Errorf("connectors: the package at %s declares an id this catalog cannot serve: %w", opts.Ref, err)
	}

	// Hashed BEFORE anything is copied, which is also what refuses an entry
	// no package may carry: bundle.ContentHashDir rejects a symlink, and
	// CopyFS would otherwise install the bytes of whatever it points at —
	// so the ORDER of these two steps is the guard, not a second check
	// inside the copy.
	fetchedHash, err := bundle.ContentHashDir(pkgDir)
	if err != nil {
		return fmt.Errorf("connectors: hash the fetched package %s at %s: %w", sourceDescription(opts), opts.Ref, err)
	}

	target := filepath.Join(root, pkg.Connector.ID)
	if _, statErr := os.Stat(target); statErr == nil && !opts.Force {
		return fmt.Errorf("connectors: %s is already installed at %s%s; pass --force to replace it", pkg.Connector.ID, target, installedPinNote(root, pkg.Connector.ID))
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("connectors: read %s: %w", target, statErr)
	}

	if err := dropConnectorPin(root, pkg.Connector.ID); err != nil {
		return err
	}
	if err := publishPackage(pkgDir, root, pkg.Connector.ID, fetchedHash); err != nil {
		return err
	}
	pin := ConnectorPin{
		Connector: pkg.Connector.ID,
		Version:   pkg.Connector.Version,
		// REDACTED, like the generated half's provenance (ADR-098: a fetch
		// URL went into connector.yaml verbatim, in a directory whose point
		// is to be committed). A record in the operator's home is read by
		// whoever can read the catalog, and the only credential a git URL
		// carries is in its userinfo or its query. What identifies the
		// source for a human — host and path — survives; re-fetching takes
		// the operator's own command, not this file.
		Source:        sourceDescription(opts),
		Ref:           opts.Ref,
		SourcePath:    opts.Path,
		ContentSHA256: fetchedHash,
		InstalledAt:   time.Now().UTC(),
	}
	if err := writeConnectorPin(root, pin); err != nil {
		return err
	}

	fmt.Fprintf(out, "installed %s v%s — %d operations, from %s at %s\n",
		pkg.Connector.ID, pkg.Connector.Version, len(pkg.Operations()), sourceDescription(opts), opts.Ref)
	fmt.Fprintf(out, "  package: %s\n", target)
	fmt.Fprintf(out, "  sha256:  %s\n", fetchedHash)
	fmt.Fprintf(out, "  pin:     %s\n", connectorPinPath(root, pkg.Connector.ID))
	// The same note `connectors gen` gives: a package written where this
	// process would not read it ends in a `connections add` that cannot find
	// its connector, which reads as a broken install rather than as a grant
	// nobody made.
	noteUngrantedDestination(out, target)
	return nil
}

// installCatalogRoot resolves the catalog root to install into.
//
// The default comes from LocalCatalogPaths — the function that decides which
// root a run reads — rather than from a second filepath.Join: an install that
// writes where resolution does not read is the one failure this command cannot
// report, because everything about it succeeds.
func installCatalogRoot(dest string) (string, error) {
	root := dest
	if root == "" {
		root = connection.LocalCatalogPaths("", store.GlobalIterionDataDir()).Home
	}
	if root == "" {
		return "", fmt.Errorf("connectors: no catalog root to install into: pass --dest, or set ITERION_HOME")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("connectors: resolve %s: %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("connectors: create the catalog root %s: %w", abs, err)
	}
	return abs, nil
}

// packageDirWithin narrows a checkout to the package directory, refusing a
// path that leaves it.
//
// The path is operator input and the TREE is source input, so both are
// resolved before either is read: `--path ../..` names bytes the pin does not
// cover, and a repository may ship `connectors/x -> /etc`, which os.Stat
// follows — the hash would then cover nothing (WalkDir does not follow a
// symlinked root) while CopyFS, which does follow, staged whatever it points
// at. Compared after EvalSymlinks on BOTH sides, because the checkout itself
// sits under a temporary directory that may be a symlink (/tmp on macOS).
func packageDirWithin(checkout, rel string) (string, error) {
	root, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return "", fmt.Errorf("connectors: resolve the fetched tree: %w", err)
	}
	if rel == "" {
		return root, nil
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(checkout, rel))
	if err != nil {
		return "", fmt.Errorf("connectors: --path %q is not in the source at this commit: %w", rel, err)
	}
	within, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", fmt.Errorf("connectors: --path %q: %w", rel, err)
	}
	if !filepath.IsLocal(within) {
		return "", fmt.Errorf("connectors: --path %q resolves to %s, outside the fetched repository, so it names bytes the pin does not cover", rel, resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("connectors: --path %q is not in the source at this commit: %w", rel, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("connectors: --path %q is not a directory, and a connector package is one", rel)
	}
	return resolved, nil
}

// publishPackage installs the fetched tree under root/<id>, verifying that
// what landed hashes to what was validated.
//
// Staged in a sibling and moved with renames, so the catalog only ever holds a
// complete package: a copy made in place would publish the directory as a run
// resolving a connector reads it, and half a package is a package whose
// operations silently differ from the one that was checked.
func publishPackage(pkgDir, root, id, wantHash string) error {
	stage, err := os.MkdirTemp(root, "."+id+".staging-")
	if err != nil {
		return fmt.Errorf("connectors: stage %s under %s: %w", id, root, err)
	}
	staged := filepath.Join(stage, id)
	defer func() { _ = os.RemoveAll(stage) }()
	if err := os.CopyFS(staged, os.DirFS(pkgDir)); err != nil {
		return fmt.Errorf("connectors: copy the fetched package: %w", err)
	}
	stagedHash, err := bundle.ContentHashDir(staged)
	if err != nil {
		return fmt.Errorf("connectors: hash the staged package: %w", err)
	}
	if stagedHash != wantHash {
		return fmt.Errorf("connectors: the staged copy of %s hashes to %s, want the validated %s", id, stagedHash, wantHash)
	}

	target := filepath.Join(root, id)
	retired := ""
	if _, err := os.Stat(target); err == nil {
		retired = filepath.Join(stage, "replaced")
		if err := os.Rename(target, retired); err != nil {
			return fmt.Errorf("connectors: move the installed %s aside: %w", id, err)
		}
	}
	if err := os.Rename(staged, target); err != nil {
		if retired != "" {
			// The previous install is the only package there is now; leaving
			// the tier EMPTY on a failed swap would uninstall a connector the
			// operator never asked to remove.
			if restoreErr := os.Rename(retired, target); restoreErr != nil {
				return fmt.Errorf("connectors: install %s failed (%w) and the previous install could not be restored from %s: %w", id, err, retired, restoreErr)
			}
		}
		return fmt.Errorf("connectors: install %s into %s: %w", id, root, err)
	}

	// Re-read what is IN PLACE, not what was staged: the hash that matters is
	// the one covering the bytes a run will load.
	installedHash, err := bundle.ContentHashDir(target)
	if err != nil {
		return fmt.Errorf("connectors: hash the installed %s: %w", id, err)
	}
	if installedHash != wantHash {
		return fmt.Errorf("connectors: the installed %s hashes to %s, want the validated %s", id, installedHash, wantHash)
	}
	return nil
}

func connectorPinPath(root, id string) string {
	return filepath.Join(root, pinsDirName, id+".json")
}

// dropConnectorPin removes the record of whatever was installed under id.
//
// Called BEFORE the package is replaced: a record that outlives the bytes it
// certifies is worse than no record — it names a commit the directory no
// longer holds, which is exactly the question an operator reads it to answer.
// No record at all reads as "installed by hand", which is true.
func dropConnectorPin(root, id string) error {
	if err := os.Remove(connectorPinPath(root, id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("connectors: drop the previous install record of %s: %w", id, err)
	}
	return nil
}

func writeConnectorPin(root string, pin ConnectorPin) error {
	path := connectorPinPath(root, pin.Connector)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("connectors: create %s: %w", dir, err)
	}
	body, err := json.MarshalIndent(pin, "", "  ")
	if err != nil {
		return fmt.Errorf("connectors: encode the install record of %s: %w", pin.Connector, err)
	}
	body = append(body, '\n')
	// Written whole and renamed into place, like a bot's origin: a reader
	// that catches a half-written record would read a truncated commit as
	// the pin.
	tmp, err := os.CreateTemp(dir, "."+pin.Connector+".pin-*")
	if err != nil {
		return fmt.Errorf("connectors: stage %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("connectors: stage %s: %w", path, err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("connectors: stage %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("connectors: stage %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("connectors: write %s: %w", path, err)
	}
	return nil
}

// ReadConnectorPin returns the install record of a package installed under id
// in a catalog root, or an error naming the file it looked for.
func ReadConnectorPin(root, id string) (*ConnectorPin, error) {
	path := connectorPinPath(root, id)
	body, err := os.ReadFile(path) // #nosec G304 -- a catalog root and a checked connector id
	if err != nil {
		return nil, err
	}
	var pin ConnectorPin
	if err := json.Unmarshal(body, &pin); err != nil {
		return nil, fmt.Errorf("connectors: decode %s: %w", path, err)
	}
	return &pin, nil
}

// installedPinNote names what the refused replacement would have replaced. A
// package installed by hand has no record, which is not an error here: the
// refusal stands on the directory, and the pin only makes it more useful.
func installedPinNote(root, id string) string {
	pin, err := ReadConnectorPin(root, id)
	if err != nil {
		return ""
	}
	return fmt.Sprintf(" (v%s, from %s at %s)", pin.Version, pin.Source, pin.Ref)
}

// sourceDescription names the source without its credential material: a git
// URL's userinfo and query are the only places a fetch URL can carry one, and
// this string is printed and wrapped into errors.
func sourceDescription(opts ConnectorsInstallOptions) string {
	src := redactedSpecURL(opts.Source)
	if src == "" {
		src = opts.Source
	}
	if opts.Path != "" {
		return src + " (" + opts.Path + ")"
	}
	return src
}
