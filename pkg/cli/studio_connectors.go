package cli

import (
	"github.com/SocialGouv/iterion/pkg/connection"
)

// LocalConnectorPlane is what a LOCAL studio wires for the connector plane's
// MCP facade (#2758): the process-wide catalog and the per-run builder the
// facade resolves through.
//
// The per-run builder is the shape the node path already resolves through
// (runview.LocalConnectors): each call's catalog is built from THAT run's
// own workspace and trust. Pinned at startup it read as "trusted" in hard
// code and rooted at the studio's own cwd — a fork run was then served the
// project tier its trust withdraws (the workspace tier is exactly where a
// repository under review can plant a package), and two runs over different
// checkouts resolved the same packages — the daemon's — while their own
// nodes resolved their own.
//
// The fallback catalog is therefore the trust-independent floor: the
// operator's home tier and the baked packages, with NO workspace tier rooted
// anywhere — the shape safe for whichever run reaches it (the workspace
// directory named at this call is deliberately NOT consulted for it).
//
// workspace is the studio's launch directory, which is NOT the workspace of
// any particular run: a run launched over a per-issue worktree has its own,
// and its grant carries it (boardMCPGrant.WorkDir) to the builder below.
func LocalConnectorPlane(workspace, homeRoot string) (connection.Catalog, func(workDir string, trust connection.ActionTrust) (connection.Catalog, error)) {
	home := connection.LocalCatalogPaths("", homeRoot)
	var floor connection.Catalog = connection.BakedCatalog()
	// An unreadable home root disables the plane loudly at the caller (the
	// studio logs it); a PRESENT-but-unreadable one is LocalCatalogs' own
	// error, which must not be swallowed into a silent narrower catalog.
	if tiers, err := connection.LocalCatalogs(home, connection.TrustUnknown()); err == nil {
		floor = connection.NewLayeredCatalog(append(tiers, connection.BakedCatalog())...)
	}
	return floor, func(workDir string, trust connection.ActionTrust) (connection.Catalog, error) {
		tiers, err := connection.LocalCatalogs(connection.LocalCatalogPaths(workDir, homeRoot), trust)
		if err != nil {
			return nil, err
		}
		return connection.NewLayeredCatalog(append(tiers, connection.BakedCatalog())...), nil
	}
}
