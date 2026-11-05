package connection

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing/fstest"

	"github.com/SocialGouv/iterion/pkg/connector/overlay"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// The team and platform TIERS (#2757): the durable tiers of the connector
// distribution, served from a PackageStore. Precedence, composed by
// CloudCatalogs, mirrors the bot distribution (team → platform → baked): a
// tenant's own package wins over the platform's, the platform's over the
// shipped floor.

// The tier kinds the two store-backed tiers answer with. TierBaked (the
// floor) and the local kinds are declared in catalog.go.
const (
	// TierTeam is a package the TENANT published — the most specific tier of
	// a cloud resolution.
	TierTeam TierKind = "team"
	// TierPlatform is a package the DEPLOYMENT published for every tenant —
	// the same trust level as the image, which is why writing it is a
	// super-admin act at the route that owns it.
	TierPlatform TierKind = "platform"
)

// StoreCatalog is a Catalog over one tenant's rows of a PackageStore: the
// team tier when built with a team id, the platform tier with
// PlatformTenantID.
//
// It reads THROUGH on every call — no cache. The FSCatalog's cache is safe
// because a local resolver lives for one run; a server's store tier lives for
// the process, and a republish must be served on the next resolution, not
// after a restart. The cost is one document read per resolution; making it
// cheaper belongs to the content-addressed lot, which can key a cache on the
// hash rather than on time.
type StoreCatalog struct {
	kind   TierKind
	tenant string
	store  PackageStore
}

// TeamCatalog serves the connector packages tenantID published.
func TeamCatalog(store PackageStore, tenantID string) *StoreCatalog {
	return &StoreCatalog{kind: TierTeam, tenant: tenantID, store: store}
}

// PlatformCatalog serves the packages the deployment published for every
// tenant.
func PlatformCatalog(store PackageStore) *StoreCatalog {
	return &StoreCatalog{kind: TierPlatform, tenant: PlatformTenantID, store: store}
}

func (c *StoreCatalog) tier() Tier { return Tier{Kind: c.kind} }

func (c *StoreCatalog) Package(connectorID string) (*spec.Package, Tier, error) {
	if err := CheckConnectorID(connectorID); err != nil {
		return nil, Tier{}, err
	}
	p, err := c.store.Get(context.Background(), c.tenant, connectorID)
	if err != nil {
		if errors.Is(err, ErrPackageNotFound) {
			// ABSENT — the one case a layered lookup may fall through on.
			return nil, Tier{}, fmt.Errorf("no connector %q in the %s tier: %w", connectorID, c.kind, errNoSuchConnector)
		}
		return nil, Tier{}, err
	}
	// Rebuild through the SAME loader every tier resolves with — the publish
	// gate ran it too, so a failure here means the row changed after the gate
	// (a hand edit, a migration), and THAT stops the search: falling through
	// to the platform or baked tier would serve a DIFFERENT package for the
	// id the team published, with no sign the team's one was refused.
	fsys := fstest.MapFS{}
	for path, content := range p.Files {
		fsys[path] = &fstest.MapFile{Data: []byte(content)}
	}
	pkg, err := overlay.LoadPackageFS(fsys, ".")
	if err != nil {
		// Name the ROW, not only the tier: the operator repairing it deletes
		// or republishes a (tenant, connector) record, and "the team tier"
		// alone does not say which.
		return nil, Tier{}, fmt.Errorf("connector %q in the %s tier (tenant %s) does not load: %w", connectorID, c.kind, c.tenant, err)
	}
	return pkg, c.tier(), nil
}

// Connectors lists the tier's rows WITHOUT loading them — loading per id
// here would pay a package build for a listing. The "must not advertise what
// it cannot serve" rule is held at Package (a row that no longer loads is
// refused by name there, stopping the search), not pre-computed here. A
// store that cannot list is the listing's error, naming the tier and tenant.
func (c *StoreCatalog) Connectors() ([]string, error) {
	rows, err := c.store.ListByTenant(context.Background(), c.tenant)
	if err != nil {
		return nil, fmt.Errorf("the %s tier (tenant %s) cannot list its connectors: %w", c.kind, c.tenant, err)
	}
	out := make([]string, 0, len(rows))
	for _, p := range rows {
		out = append(out, p.ConnectorID)
	}
	return out, nil
}

// sharedFloor is the ONE baked catalog per process. The floor's packages are
// compiled into the binary and cannot change at runtime, so rebuilding it per
// resolution re-parsed ~790 KiB of YAML per facade call for nothing — and
// defeated the FSCatalog's own load cache, which is per instance. The
// store-backed tiers above it are rebuilt per call ON PURPOSE (a republish
// must serve on the next resolution); the floor is the immutable part.
var sharedFloor = sync.OnceValue(func() *FSCatalog { return BakedCatalog() })

// CloudCatalogs composes the tiers a CLOUD resolution walks, most specific
// first: the team's own packages, then the platform's, then the baked floor
// compiled into the binary. The local tiers (project, home) are NOT here —
// they are a local install's, composed by LocalCatalogs with their own trust
// rule; a cloud resolution never reads a workspace.
func CloudCatalogs(store PackageStore, tenantID string) []Catalog {
	return []Catalog{
		TeamCatalog(store, tenantID),
		PlatformCatalog(store),
		sharedFloor(),
	}
}
