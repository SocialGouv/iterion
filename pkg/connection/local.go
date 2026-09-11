package connection

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// The LOCAL tier: how a laptop, a desktop app or a CLI run gets a resolver.
//
// The cloud tier resolves the same seam from Mongo and the team's stored
// packages. Both build a *Resolver; only where the packages and the records
// come from differs, which is what keeps the executor from ever knowing which
// deployment it is running in.

// LocalPaths says where a local install looks for connector packages.
//
// Two roots, most specific first, mirroring how skills and secrets already
// layer: a project may ship or pin a connector without touching the operator's
// home, and an operator may install one for every project.
type LocalPaths struct {
	// Project is <workspace>/connectors, checked first.
	Project string
	// Home is <iterion home>/connectors.
	Home string
}

// LocalCatalogPaths derives the two roots from a workspace and an iterion home.
func LocalCatalogPaths(workspace, iterionHome string) LocalPaths {
	var p LocalPaths
	if workspace != "" {
		p.Project = filepath.Join(workspace, "connectors")
	}
	if iterionHome != "" {
		p.Home = filepath.Join(iterionHome, "connectors")
	}
	return p
}

// layeredCatalog resolves through its tiers in order.
type layeredCatalog struct{ tiers []Catalog }

// NewLayeredCatalog serves the first tier that holds the connector. Order is
// most-specific-first: a project's own package wins, which is what makes a
// pinned or patched connector possible without an install.
func NewLayeredCatalog(tiers ...Catalog) Catalog {
	kept := make([]Catalog, 0, len(tiers))
	for _, t := range tiers {
		if t != nil {
			kept = append(kept, t)
		}
	}
	return &layeredCatalog{tiers: kept}
}

func (c *layeredCatalog) Package(connectorID string) (*spec.Package, error) {
	if len(c.tiers) == 0 {
		return nil, fmt.Errorf("no connector catalog is configured, so %q cannot be resolved", connectorID)
	}
	var reasons []string
	for _, t := range c.tiers {
		pkg, err := t.Package(connectorID)
		if err == nil {
			return pkg, nil
		}
		// A tier that HAS the package and could not load it stops the search.
		//
		// Falling through on every error turned validation into selection: a
		// project pinning a package this build cannot read — a newer schema
		// version, a malformed overlay — had it correctly refused, and then the
		// operator's home tier silently served a DIFFERENT package with
		// different operations and a different policy. The run succeeded
		// against the wrong connector, which is worse than failing.
		if !errors.Is(err, errNoSuchConnector) {
			return nil, err
		}
		reasons = append(reasons, err.Error())
	}
	// Every tier's reason, not just the last: two tiers can be absent for
	// different reasons, and reporting only the last sends the reader to the
	// wrong directory.
	return nil, fmt.Errorf("%s", strings.Join(reasons, "; "))
}

// Connectors is the union of every tier's, deduplicated and sorted.
func (c *layeredCatalog) Connectors() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range c.tiers {
		for _, id := range t.Connectors() {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}

// LocalResolver builds the resolver a local run uses: the layered package
// catalog, the file-backed connection store, and the local sealer.
//
// Returns (nil, nil) when nothing is wired — no catalog root exists and no
// store file is present. Nil is the honest answer rather than an empty
// resolver: it leaves the executor's Connectors unset, so a `.bot` declaring
// an action fails with "this process has no connector catalog wired" instead
// of a resolution error that reads as though the connector were at fault.
func LocalResolver(paths LocalPaths, storeDir string, sealer secrets.Sealer) (*Resolver, error) {
	var catalogs []Catalog
	for _, root := range []string{paths.Project, paths.Home} {
		if root == "" {
			continue
		}
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			continue
		}
		catalogs = append(catalogs, NewFSCatalog(root))
	}
	storePath := DefaultPath(storeDir)
	_, statErr := os.Stat(storePath)
	if len(catalogs) == 0 && statErr != nil {
		return nil, nil
	}
	st, err := NewFileStore(storePath)
	if err != nil {
		return nil, err
	}
	return &Resolver{
		Catalog:  NewLayeredCatalog(catalogs...),
		Store:    st,
		Sealer:   sealer,
		TenantID: LocalTenant,
	}, nil
}

// LocalHTTPClient is the client a local connector call goes out on.
//
// The GUARDED one, with no exception for a local run. A connector reaches a
// host an operator configured, and "it is only my laptop" is exactly the
// reasoning that makes a workflow able to fetch
// http://169.254.169.254/latest/meta-data/ on the machine where the developer
// is signed into everything. The timeout is generous because a vendor's
// pagination can be slow; the node's own `timeout:` is the shorter bound an
// author sets.
func LocalHTTPClient() *http.Client {
	return httpdial.SafeClient(true, 2*time.Minute)
}
