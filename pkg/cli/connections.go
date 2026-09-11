package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The local connection commands: what an operator runs once so a `.bot`'s
// `connection: main` resolves to a credential.

// ConnectionAddOptions is `iterion connections add`.
type ConnectionAddOptions struct {
	// Connector is the package id ("forgejo").
	Connector string
	// Alias is the name a `.bot` writes. Defaults to "main" — one connection
	// per connector is the common case, and making the operator invent a name
	// for it is a papercut on the very first command.
	Alias string
	// BaseURL pins the instance. Empty uses the package's default.
	BaseURL string
	// Scheme is which auth scheme the credential satisfies. Empty picks the
	// package's only one, and refuses when there are several.
	Scheme string
	// TokenEnv names the environment variable holding the credential.
	//
	// An ENV VAR rather than a flag, deliberately: a token passed as an
	// argument lands in the shell history and in `ps` output for the life of
	// the process. There is no --token flag for the same reason.
	TokenEnv string
	// Capabilities is what the connection may be used for. Empty grants
	// `action` only — the narrower of the two, since an agent choosing its own
	// calls is a decision an operator should make deliberately.
	Capabilities []string
	// StoreDir is the --store-dir override, resolved through
	// store.ResolveStoreDir like every other local store's — so connections
	// land beside the secrets of the same project rather than in a second
	// place the operator has to learn.
	StoreDir string
	// DisplayName is cosmetic.
	DisplayName string
}

// ConnectionsAdd stores a credential for a connector.
func ConnectionsAdd(opts ConnectionAddOptions, out io.Writer) error {
	if strings.TrimSpace(opts.Connector) == "" {
		return errors.New("connections add: --connector is required")
	}
	if strings.TrimSpace(opts.TokenEnv) == "" {
		return errors.New("connections add: --token-env is required (the name of an environment variable holding the credential; a token passed as a flag would land in your shell history)")
	}
	token := os.Getenv(opts.TokenEnv)
	if token == "" {
		return fmt.Errorf("connections add: $%s is empty", opts.TokenEnv)
	}
	alias := strings.TrimSpace(opts.Alias)
	if alias == "" {
		alias = "main"
	}

	// The package is loaded FIRST, so a typo in the connector id or an
	// unknown scheme is refused before a credential is sealed anywhere.
	cat, err := localCatalog(opts.StoreDir)
	if err != nil {
		return err
	}
	pkg, err := cat.Package(opts.Connector)
	if err != nil {
		return err
	}
	scheme, err := resolveScheme(pkg, opts.Scheme)
	if err != nil {
		return err
	}
	caps, err := parseCapabilities(opts.Capabilities)
	if err != nil {
		return err
	}

	sealer, err := secrets.NewLocalSealer(store.GlobalIterionDataDir(), nil)
	if err != nil {
		return err
	}
	id, err := store.GenerateRunID()
	if err != nil {
		return fmt.Errorf("connections add: generate id: %w", err)
	}
	id = "conn_" + id
	sealed, err := connection.SealToken(sealer, id, token, time.Time{})
	if err != nil {
		return err
	}

	st, err := connection.NewFileStore(connection.DefaultPath(store.ResolveStoreDir(cwd(), opts.StoreDir)))
	if err != nil {
		return err
	}
	conn := connection.Connection{
		ID: id, TenantID: connection.LocalTenant,
		Connector: opts.Connector, Alias: alias,
		DisplayName: opts.DisplayName, BaseURL: strings.TrimRight(opts.BaseURL, "/"),
		SchemeID: scheme, Capabilities: caps,
		Status: connection.StatusActive, SealedPayload: sealed,
		// Deliberately NOT claiming to know the grant. A provider that never
		// enumerated a PAT's scopes has told us nothing, and recording an
		// empty list as fact would turn "unknown" into "none" — refusing every
		// operation with a scope requirement.
		ScopesKnown: false,
	}
	if err := st.Create(context.Background(), conn); err != nil {
		if errors.Is(err, connection.ErrExists) {
			return fmt.Errorf("connections add: a %q connection named %q already exists — remove it first, or use a different --alias", opts.Connector, alias)
		}
		return err
	}

	host := conn.BaseURL
	if host == "" {
		host = pkg.Connector.BaseURL.Default + " (the package default)"
	}
	fmt.Fprintf(out, "connected %s as %q → %s\n", opts.Connector, alias, host)
	// The RESOLVED capabilities, not the flag. An operator who named none gets
	// `action` by default, and echoing the empty flag reported a grant that
	// differs from the record just written — `connections list` then says
	// `action` for the same connection, which is the shape of a bug report
	// rather than of a default.
	fmt.Fprintf(out, "  scheme: %s · capabilities: %s\n", scheme, capabilityNames(caps))
	fmt.Fprintf(out, "  a .bot reaches it with `connection: %s`\n", alias)
	// A self-hosted instance is the common case for the connectors this
	// catalog ships, and the guarded dialer refuses one by default. Said here
	// rather than left for the first run to discover: the record is still
	// created — the environment can change, and this command is not the place
	// to decide a deployment's network policy.
	if advice := connection.UnreachableBaseURL(context.Background(), conn.BaseURL); advice != "" {
		fmt.Fprintf(out, "  warning: %s\n", advice)
	}
	return nil
}

// ConnectionsList prints the local connections. It never prints a credential,
// nor anything derived from one: a fingerprint would still be a fact about a
// secret this command has no reason to reveal.
func ConnectionsList(storeDir string, out io.Writer) error {
	st, err := connection.NewFileStore(connection.DefaultPath(store.ResolveStoreDir(cwd(), storeDir)))
	if err != nil {
		return err
	}
	all, err := st.List(context.Background(), connection.LocalTenant, "")
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Fprintln(out, "no connections (add one with `iterion connections add --connector <id> --token-env <VAR>`)")
		return nil
	}
	for _, c := range all {
		host := c.BaseURL
		if host == "" {
			host = "(package default)"
		}
		fmt.Fprintf(out, "%-16s %-12s %-10s %s\n", c.Alias, c.Connector, c.Status, host)
		fmt.Fprintf(out, "  %s · scheme %s · %s\n", c.ID, c.SchemeID, capabilityNames(c.Capabilities))
		if c.StatusReason != "" {
			fmt.Fprintf(out, "  %s\n", c.StatusReason)
		}
	}
	return nil
}

// ConnectionsRemove deletes a connection by alias.
func ConnectionsRemove(storeDir, connector, alias string, out io.Writer) error {
	st, err := connection.NewFileStore(connection.DefaultPath(store.ResolveStoreDir(cwd(), storeDir)))
	if err != nil {
		return err
	}
	ctx := context.Background()
	c, err := st.ByAlias(ctx, connection.LocalTenant, connector, alias)
	if err != nil {
		if errors.Is(err, connection.ErrNotFound) {
			return fmt.Errorf("no %q connection named %q", connector, alias)
		}
		return err
	}
	if err := st.Delete(ctx, connection.LocalTenant, c.ID); err != nil {
		return err
	}
	fmt.Fprintf(out, "removed %s connection %q\n", connector, alias)
	return nil
}

// localCatalog builds the same layered catalog a run uses, so `connections
// add` refuses exactly what a run would refuse — a connector the run could not
// have resolved either.
func localCatalog(storeDir string) (connection.Catalog, error) {
	wd, _ := os.Getwd()
	paths := connection.LocalCatalogPaths(wd, store.GlobalIterionDataDir())
	var tiers []connection.Catalog
	for _, root := range []string{paths.Project, paths.Home} {
		if root == "" {
			continue
		}
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			continue
		}
		tiers = append(tiers, connection.NewFSCatalog(root))
	}
	if len(tiers) == 0 {
		return nil, fmt.Errorf("no connector catalog found — expected %s or %s", paths.Project, paths.Home)
	}
	return connection.NewLayeredCatalog(tiers...), nil
}

// resolveScheme picks the auth scheme, refusing to guess when the package
// declares several: placement differs between them, so a wrong guess is a 401
// that reads like a bad credential and sends an operator to rotate a token
// that is fine.
func resolveScheme(pkg *spec.Package, want string) (string, error) {
	ids := make([]string, 0, len(pkg.Connector.Auth))
	for _, a := range pkg.Connector.Auth {
		ids = append(ids, a.ID)
	}
	sort.Strings(ids)
	if want != "" {
		for _, id := range ids {
			if id == want {
				return id, nil
			}
		}
		return "", fmt.Errorf("connector %q has no auth scheme %q (it declares: %s)", pkg.Connector.ID, want, strings.Join(ids, ", "))
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("connector %q declares no auth scheme", pkg.Connector.ID)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("connector %q declares several auth schemes (%s) — name one with --scheme", pkg.Connector.ID, strings.Join(ids, ", "))
	}
}

// parseCapabilities turns the flag values into capabilities, defaulting to
// `action` alone.
// capabilityNames renders a grant for a human. One rendering shared by `add`
// and `list`, so the two commands cannot describe the same record
// differently — which is exactly what they did while `add` echoed the flag
// the operator typed instead of the grant it stored.
func capabilityNames(caps []connection.Capability) string {
	if len(caps) == 0 {
		return "none"
	}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, string(c))
	}
	return strings.Join(out, "+")
}

func parseCapabilities(vals []string) ([]connection.Capability, error) {
	if len(vals) == 0 {
		return []connection.Capability{connection.CapAction}, nil
	}
	var out []connection.Capability
	for _, v := range vals {
		c := connection.Capability(strings.TrimSpace(v))
		if !connection.ValidCapability(c) {
			return nil, fmt.Errorf("unknown capability %q (want %q or %q)", v, connection.CapAction, connection.CapAgent)
		}
		out = append(out, c)
	}
	return out, nil
}
