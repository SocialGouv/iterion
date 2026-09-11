package connection

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
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

// LocalCatalogs builds the package tiers of a local install, most specific
// first — the one place that decides which roots are catalogs, so `iterion
// connections add` refuses exactly what a run would refuse.
//
// A root that is ABSENT is skipped; a root that is there and cannot be read
// (EACCES on its parent, EIO, a name that is not a directory's to hold) is an
// ERROR. Collapsing the two is how validation becomes selection — the defect
// layeredCatalog.Package refuses by name one file over: with the project tier
// silently dropped, the home tier serves a DIFFERENT package for the same
// connector id, with different operations and a different auth placement, and
// nothing in the run says so. The rule has to hold where the tiers are BUILT
// too, or the tier that would have won is simply not there to win.
func LocalCatalogs(paths LocalPaths) ([]Catalog, error) {
	var out []Catalog
	for _, root := range []string{paths.Project, paths.Home} {
		if root == "" {
			continue
		}
		fi, err := os.Stat(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("connector catalog %s cannot be read: %w", root, err)
		}
		if !fi.IsDir() {
			// Not a catalog at all — a directory is the only thing that can
			// hold one, so this is an absence rather than a refusal.
			continue
		}
		out = append(out, NewFSCatalog(root))
	}
	return out, nil
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
	catalogs, err := LocalCatalogs(paths)
	if err != nil {
		return nil, err
	}
	storePath := DefaultPath(storeDir)
	if _, err := os.Stat(storePath); err != nil {
		// Same rule as the tiers: ABSENT is "nothing wired", anything else is
		// a store that is there and cannot be read, which must not be
		// reported as a feature nobody configured.
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("connection store %s cannot be read: %w", storePath, err)
		}
		if len(catalogs) == 0 {
			return nil, nil
		}
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

// AllowPrivateHostsEnv is the deployment-controlled exception that lets a
// local connector call reach a private, loopback or link-local address.
//
// ADR-098 owes it: "a tenant-supplied URL must not be its own justification
// for reaching a private address; a self-hosted endpoint needs a
// deployment-controlled exception". Without one the guard below is not a
// load-bearing limit but an artificial one — the single connector this
// catalog ships is Forgejo, which is overwhelmingly self-hosted, so
// `--base-url http://localhost:3000` was accepted by `connections add` and
// then refused at every call with no way to permit it.
//
// An ENV var rather than a per-connection field on purpose: the decision
// belongs to whoever runs the process, not to whoever adds the connection —
// the same reasoning, and the same spelling, as
// ITERION_RUNNER_CLONE_ALLOW_PRIVATE for on-prem forge clones.
const AllowPrivateHostsEnv = "ITERION_CONNECTOR_ALLOW_PRIVATE"

// allowPrivateHosts reports whether this process permits it. Read at client
// construction, which is once per run — an operator flipping it mid-run is not
// a case worth re-reading the environment for.
func allowPrivateHosts() bool {
	return os.Getenv(AllowPrivateHostsEnv) == "1"
}

// LocalHTTPClient is the client a local connector call goes out on.
//
// The GUARDED one by default, with no exception for a local run. A connector
// reaches a host an operator configured, and "it is only my laptop" is exactly
// the reasoning that makes a workflow able to fetch
// http://169.254.169.254/latest/meta-data/ on the machine where the developer
// is signed into everything. AllowPrivateHostsEnv is the deliberate,
// greppable way out for a self-hosted instance. The timeout is generous
// because a vendor's pagination can be slow; the node's own `timeout:` is the
// shorter bound an author sets.
//
// LOCAL tier only. A cloud tier builds its own client and must NOT read the
// env var: there the base URL is tenant-supplied, so relaxing the guard would
// hand one tenant the pod's own network — which is the case the guard exists
// for, not an ergonomic papercut.
func LocalHTTPClient() *http.Client {
	strict := !allowPrivateHosts()
	c := httpdial.SafeClient(strict, 2*time.Minute)
	if strict {
		// Only under the guard: with the hatch open there is no refusal to
		// explain, and a hint on an ordinary "connection refused" to localhost
		// would name a variable that is already set.
		c.Transport = &privateHostHint{base: c.Transport}
	}
	return c
}

// privateHostHint turns the guard's refusal into one an operator can act on.
//
// httpdial answers "resolved address 10.0.0.5 is not a public unicast IP",
// which is true and useless: it names neither the workflow's connection nor a
// way to permit the host. The remedy is added here rather than in httpdial
// because the env var is this package's, and the guard is shared with callers
// (webhooks, OIDC, the preview proxy) for whom it is not a way out at all.
type privateHostHint struct{ base http.RoundTripper }

func (t *privateHostHint) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	ip, ok := refusedAsPrivate(req.Context(), req.URL.Hostname())
	if !ok {
		return nil, err
	}
	return nil, fmt.Errorf("%w (%s is a private or loopback address; set %s=1 to let connector calls reach a self-hosted instance)",
		err, ip, AllowPrivateHostsEnv)
}

// refusedAsPrivate reports whether host is one the guard refuses FOR WHAT IT
// RESOLVED TO, and the address that settles it.
//
// The two-step is the whole point, and both callers need exactly it: resolving
// with the policy OFF separates two failures that look alike from the outside.
// A host that resolves and is refused for its address is the hatch's case; a
// host that does not resolve at all is a typo or a dead DNS, and telling its
// author to open a security guard would be advice about the wrong problem. A
// cancelled or timed-out context lands in the second branch, so the answer is
// "no" rather than a guess.
func refusedAsPrivate(ctx context.Context, host string) (net.IP, bool) {
	if host == "" {
		return nil, false
	}
	ip, err := httpdial.ResolvePublicHost(ctx, host, false)
	if err != nil || httpdial.IsPublicUnicast(ip) {
		return nil, false
	}
	return ip, true
}

// UnreachableBaseURL says why this process will refuse to call baseURL, or ""
// when it will reach it.
//
// Told at the moment a connection is ADDED, because that is where the mistake
// is made. The refusal otherwise arrives a layer away — in a run, from a
// workflow that names an operation rather than a URL — where it reads as a
// broken connector rather than as a host this deployment does not permit.
// Advice only: the answer can change with the environment, so it never
// refuses the record.
func UnreachableBaseURL(ctx context.Context, baseURL string) string {
	if baseURL == "" || allowPrivateHosts() {
		return ""
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	ip, ok := refusedAsPrivate(ctx, u.Hostname())
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s resolves to %s, a private or loopback address that connector calls refuse by default — set %s=1 to permit a self-hosted instance",
		u.Hostname(), ip, AllowPrivateHostsEnv)
}
