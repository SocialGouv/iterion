package connection

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/connector/overlay"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// errNoSuchConnector marks the ONE catalog failure a layered lookup may fall
// through on: the tier simply does not hold this connector.
//
// Every other failure — a schema version this build cannot read, a malformed
// overlay, an unreadable directory — means the tier HAS the package and could
// not load it, and falling through there turns validation into selection: the
// package an operator pinned is correctly refused, and a different one with
// different operations is served in its place. A run then succeeds against the
// wrong connector, which is worse than failing.
var errNoSuchConnector = errors.New("connection: no such connector in this catalog")

// TierKind says what KIND of catalog answered. A plain name rather than an
// int, because it is printed: the one reader of a tier is a person debugging
// an operation they did not expect.
type TierKind string

const (
	// TierDirectory is a catalog root nobody labelled further — a test
	// fixture, an operator's own tree handed to NewFSCatalog.
	TierDirectory TierKind = "directory"
	// TierProject is `<workspace>/connectors`, the tier a repository ships
	// and that ITERION_CONNECTOR_PROJECT_CATALOG gates.
	TierProject TierKind = "project"
	// TierHome is `<iterion home>/connectors`, installed for every project.
	TierHome TierKind = "home"
	// TierMemory is a catalog held in memory: tests, and the tier that
	// rebuilds a stored package.
	TierMemory TierKind = "memory"
)

// Tier names the catalog that SERVED a package, and where that copy came from.
//
// It travels WITH the answer rather than being reconstructed by whoever reads
// it, because an observer cannot reconstruct it at all: a layered lookup asks
// its tiers in order and only the one that answered knows it did. Two tiers
// can hold the same connector id, the same scheme id and the same placement,
// so "which one won" is not derivable from the package.
//
// For TELLING, never for choosing. The resolver must not acquire a branch on
// the tier that served it — a rule that reads a tier would put the decision
// back on the untrusted side of the question LocalCatalogs answers once (a
// tier that must not win is not BUILT for that run). Naming the winner is for
// the operator reading an error; it is not a licence.
type Tier struct {
	// Kind is WHICH tier this is. It does not carry rank: precedence is the
	// ORDER the tiers were composed in (NewLayeredCatalog), so a rule that
	// read a kind to decide anything would be a second, disagreeing answer.
	Kind TierKind
	// Source is where this tier's copy lives — a root directory, or empty
	// for a tier that is not a path.
	Source string
}

// String is the phrase a surface prints, with no leading article so a caller
// can write "from the %s" or "the %s now asks for".
func (t Tier) String() string {
	if t.Kind == "" {
		// The zero value means NOTHING served, which only reaches a reader
		// through a bug: every error path pairs it with the error that says
		// what happened.
		return "no tier"
	}
	if t.Source == "" {
		return string(t.Kind) + " catalog"
	}
	return string(t.Kind) + " catalog " + t.Source
}

// Catalog resolves a connector id to its package.
//
// An interface because the tiers differ and the resolver must not DEPEND on
// which one answered: a directory on a laptop, a team-authored package in
// Mongo, a platform override, a marketplace entry. The three-tier resolution
// lands later; what matters now is that nothing downstream is written against
// a filesystem.
type Catalog interface {
	// Package returns the connector's package and the tier that served it,
	// or an error naming what was looked for. Implementations must be safe
	// for concurrent use, and must name themselves: a zero Tier with a nil
	// error is a defect, and layeredCatalog refuses it by name.
	Package(connectorID string) (*spec.Package, Tier, error)
	// Connectors lists the ids this catalog can serve, sorted. Used by the
	// studio and by a diagnostic that suggests a near miss.
	Connectors() []string
}

// FSCatalog serves packages from a directory of them — `connectors/<id>/` in
// a checkout, or an operator's own tree.
//
// It CACHES a package after the first load, which matters more than it looks:
// the shipped Forgejo package is 651 KiB of YAML across ten files, and parsing
// it per node would put a measurable cost on every call in a loop.
type FSCatalog struct {
	kind TierKind
	root string

	mu     sync.RWMutex
	loaded map[string]*spec.Package
}

// NewFSCatalog serves packages from the directories under root, as a tier
// nobody labelled further — a test fixture, an operator's own tree.
func NewFSCatalog(root string) *FSCatalog {
	return NewFSCatalogAs("", root)
}

// NewFSCatalogAs is NewFSCatalog for a root whose tier is KNOWN.
//
// The kind is the BUILDER's knowledge, not the catalog's: the same type serves
// `<workspace>/connectors` and `<iterion home>/connectors`, and only the
// function that decides which root is which can say (LocalCatalogs). Reading
// it off the path would be a second rule, wrong the first time an operator
// points a tier somewhere unusual.
//
// An EMPTY kind is the unlabelled door above rather than a mistake to recover
// from: it means "a directory, and nothing more is known about it", said here
// so the two constructors cannot disagree about what unlabelled serves as.
func NewFSCatalogAs(kind TierKind, root string) *FSCatalog {
	if kind == "" {
		kind = TierDirectory
	}
	return &FSCatalog{kind: kind, root: root, loaded: map[string]*spec.Package{}}
}

// tier says which tier this catalog is, for the answers it serves. Internal:
// the tier travels in the ANSWER, so no caller has to ask a catalog what it
// is — and an accessor nobody needs is surface a later tier would have to
// honour.
func (c *FSCatalog) tier() Tier { return Tier{Kind: c.kind, Source: c.root} }

// Package loads (once) and returns a connector package, with this tier.
func (c *FSCatalog) Package(connectorID string) (*spec.Package, Tier, error) {
	if err := CheckConnectorID(connectorID); err != nil {
		return nil, Tier{}, err
	}
	c.mu.RLock()
	pkg, ok := c.loaded[connectorID]
	c.mu.RUnlock()
	if ok {
		return pkg, c.tier(), nil
	}

	dir := filepath.Join(c.root, connectorID)
	// The EFFECTIVE package — generated half plus overlay, then the complete
	// check. Not `spec.Load`, which reads only the generated half: calling it
	// here served execution a package the authored corrections had never
	// touched, so the shipped Forgejo connector validated with two auth
	// schemes and ran with five, answered to its pinned operation ids in
	// validation and to none of them at run time, and lost every declared
	// pagination. The same loader backs `iterion connectors validate`, so what
	// an operator checks is what a run gets.
	pkg, err := overlay.LoadPackage(dir)
	if err != nil {
		// errors.Is, not os.IsNotExist: the loader wraps with %w and the
		// legacy helper does not unwrap, so the absent case was never
		// recognised — every missing connector reported as a load failure,
		// and (once layering arrived) stopped the search instead of falling
		// through to the tier that has it.
		if errors.Is(err, os.ErrNotExist) {
			// ABSENT — the only case a layered lookup may fall through on, so
			// it carries the sentinel. Anything else below means the tier has
			// the package and cannot load it.
			return nil, Tier{}, fmt.Errorf("no connector %q in %s%s: %w", connectorID, c.root, c.suggest(connectorID), errNoSuchConnector)
		}
		return nil, Tier{}, fmt.Errorf("connector %q: %w", connectorID, err)
	}
	if pkg.Connector.ID != connectorID {
		// The directory name is what a `.bot` addresses, so a package whose
		// own id differs would be reachable under a name it does not answer
		// to — and its operation ids, which all start with its own id, would
		// never match what was asked for.
		return nil, Tier{}, fmt.Errorf("connector directory %q holds a package whose id is %q; the directory name is what a `.bot` addresses", connectorID, pkg.Connector.ID)
	}

	c.mu.Lock()
	c.loaded[connectorID] = pkg
	c.mu.Unlock()
	return pkg, c.tier(), nil
}

// Connectors lists the directories that look like packages.
func (c *FSCatalog) Connectors() []string {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(c.root, e.Name(), "connector.yaml")); err != nil {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// suggest names the closest connector the catalog does hold, when there is an
// obvious one. A "not found" that lists nothing sends the reader to the
// filesystem; naming the near miss usually ends the search.
func (c *FSCatalog) suggest(want string) string {
	have := c.Connectors()
	if len(have) == 0 {
		return ""
	}
	for _, got := range have {
		if strings.EqualFold(got, want) {
			return " (did you mean " + got + "? — the id is case-sensitive)"
		}
	}
	return " (it holds: " + strings.Join(have, ", ") + ")"
}

// CheckConnectorID refuses an id that could escape the catalog root, or that
// no `.bot` could address.
//
// The id reaches here from a `.bot`'s `action:` — authored text, and on a
// multi-tenant deployment authored by someone who is not the operator. A
// `..` in it would read a package from anywhere the process can reach.
//
// Exported because it is also the rule the GENERATOR must hold to: `iterion
// connectors gen --id` names a directory to write (and whose `ops/` it
// removes first), and it seeds the first segment of every operation id. One
// rule, so what can be written is exactly what can later be resolved.
func CheckConnectorID(id string) error {
	if id == "" {
		return fmt.Errorf("connector id is empty")
	}
	if id != filepath.Base(id) || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("connector id %q is not a plain name", id)
	}
	// A DOT makes the package unaddressable: ResolveAction cuts an action id
	// at its first one, so `google.drive` is asked of the catalog as `google`
	// and nothing ever answers. A green generation for something no workflow
	// can call is the worst of the three outcomes.
	if strings.Contains(id, ".") {
		return fmt.Errorf("connector id %q contains a dot, which separates an operation id's segments — a `.bot` naming it would resolve %q instead",
			id, id[:strings.Index(id, ".")])
	}
	return nil
}

// MemoryCatalog serves packages held in memory: tests, and the tier that
// rebuilds a team-authored package from its stored content.
type MemoryCatalog struct {
	mu  sync.RWMutex
	pkg map[string]*spec.Package
}

// NewMemoryCatalog builds a catalog over the given packages, keyed by their
// own connector ids.
func NewMemoryCatalog(pkgs ...*spec.Package) *MemoryCatalog {
	c := &MemoryCatalog{pkg: map[string]*spec.Package{}}
	for _, p := range pkgs {
		if p != nil {
			c.pkg[p.Connector.ID] = p
		}
	}
	return c
}

// tier says which tier this catalog is, for the answers it serves.
func (c *MemoryCatalog) tier() Tier { return Tier{Kind: TierMemory} }

func (c *MemoryCatalog) Package(connectorID string) (*spec.Package, Tier, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.pkg[connectorID]
	if !ok {
		return nil, Tier{}, fmt.Errorf("no connector %q in this catalog: %w", connectorID, errNoSuchConnector)
	}
	return p, c.tier(), nil
}

func (c *MemoryCatalog) Connectors() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.pkg))
	for id := range c.pkg {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
