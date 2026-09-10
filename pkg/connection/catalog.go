package connection

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// Catalog resolves a connector id to its package.
//
// An interface because the tiers differ and the resolver must not know which
// one answered: a directory on a laptop, a team-authored package in Mongo, a
// platform override, a marketplace entry. The three-tier resolution lands
// later; what matters now is that nothing downstream is written against a
// filesystem.
type Catalog interface {
	// Package returns the connector's package, or an error naming what was
	// looked for. Implementations must be safe for concurrent use.
	Package(connectorID string) (*spec.Package, error)
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
	root string

	mu     sync.RWMutex
	loaded map[string]*spec.Package
}

// NewFSCatalog serves packages from the directories under root.
func NewFSCatalog(root string) *FSCatalog {
	return &FSCatalog{root: root, loaded: map[string]*spec.Package{}}
}

// Package loads (once) and returns a connector package.
func (c *FSCatalog) Package(connectorID string) (*spec.Package, error) {
	if err := checkConnectorID(connectorID); err != nil {
		return nil, err
	}
	c.mu.RLock()
	pkg, ok := c.loaded[connectorID]
	c.mu.RUnlock()
	if ok {
		return pkg, nil
	}

	dir := filepath.Join(c.root, connectorID)
	// spec.Load runs the COMPLETE validation, not the generator's: a package
	// reached from a node has to be one a call can be built from, and finding
	// out otherwise mid-run means finding out after a credential was resolved.
	pkg, err := spec.Load(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no connector %q in %s%s", connectorID, c.root, c.suggest(connectorID))
		}
		return nil, fmt.Errorf("connector %q: %w", connectorID, err)
	}
	if pkg.Connector.ID != connectorID {
		// The directory name is what a `.bot` addresses, so a package whose
		// own id differs would be reachable under a name it does not answer
		// to — and its operation ids, which all start with its own id, would
		// never match what was asked for.
		return nil, fmt.Errorf("connector directory %q holds a package whose id is %q; the directory name is what a `.bot` addresses", connectorID, pkg.Connector.ID)
	}

	c.mu.Lock()
	c.loaded[connectorID] = pkg
	c.mu.Unlock()
	return pkg, nil
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

// checkConnectorID refuses an id that could escape the catalog root.
//
// The id reaches here from a `.bot`'s `action:` — authored text, and on a
// multi-tenant deployment authored by someone who is not the operator. A
// `..` in it would read a package from anywhere the process can reach.
func checkConnectorID(id string) error {
	if id == "" {
		return fmt.Errorf("connector id is empty")
	}
	if id != filepath.Base(id) || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("connector id %q is not a plain name", id)
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

func (c *MemoryCatalog) Package(connectorID string) (*spec.Package, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.pkg[connectorID]
	if !ok {
		return nil, fmt.Errorf("no connector %q in this catalog", connectorID)
	}
	return p, nil
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
