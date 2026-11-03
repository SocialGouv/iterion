package connection

import (
	"context"
	"errors"
	"fmt"
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
		return nil, Tier{}, fmt.Errorf("connector %q in the %s tier does not load: %w", connectorID, c.kind, err)
	}
	return pkg, c.tier(), nil
}

// Connectors lists the ids this tier can serve — the rows that ARE the tier.
// A row that no longer loads does not advertise (the rule a tier that cannot
// serve holds): it is skipped here and refused by name at Package.
func (c *StoreCatalog) Connectors() []string {
	rows, err := c.store.ListByTenant(context.Background(), c.tenant)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, p := range rows {
		out = append(out, p.ConnectorID)
	}
	return out
}

// CloudCatalogs composes the tiers a CLOUD resolution walks, most specific
// first: the team's own packages, then the platform's, then the baked floor
// compiled into the binary. The local tiers (project, home) are NOT here —
// they are a local install's, composed by LocalCatalogs with their own trust
// rule; a cloud resolution never reads a workspace.
func CloudCatalogs(store PackageStore, tenantID string) []Catalog {
	return []Catalog{
		TeamCatalog(store, tenantID),
		PlatformCatalog(store),
		BakedCatalog(),
	}
}
