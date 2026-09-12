package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
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
	// The SAME shape gate `iterion secret set` applies, for the reason it
	// states: "a value that could not possibly authenticate is refused at the
	// paste, not discovered as a provider 401 in the middle of a run hours
	// later". A credential arriving through the environment is the case that
	// needs it most — a k8s `envFrom`, a `.env` line, a CI variable and a
	// `$(cat file)` all bring a trailing newline, which this command sealed
	// happily. The connection then listed as active and failed at its first
	// call with a 401, or with Go's opaque "invalid header field value for
	// Authorization" — a layer away from the command that created it, which is
	// the same failure checkSchemeIsWritable and checkBaseURL were added here
	// to close.
	if err := secrets.ValidateTokenShape(fmt.Sprintf("$%s", opts.TokenEnv), token); err != nil {
		return fmt.Errorf("connections add: %w", err)
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
	if err := checkSchemeIsWritable(pkg, scheme); err != nil {
		return err
	}
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if err := checkBaseURL(pkg, baseURL); err != nil {
		return err
	}
	// The origin is RESOLVED and PINNED here, not left to be re-derived at
	// call time.
	//
	// An empty BaseURL meant "whatever the package's default is when the call
	// happens" — so replacing the package, or shadowing it with a project-tier
	// one (which a granted `ITERION_CONNECTOR_PROJECT_CATALOG` lets any
	// repository do), redirected an
	// existing credential to a different host. The token an operator bound to
	// a vendor would have been sent to whoever the new package named, and
	// nothing in the run would have said so. The SSRF guard does not help: the
	// new origin is a perfectly ordinary public host. What a credential may be
	// sent to is decided when it is entrusted to iterion.
	//
	// checkBaseURL refuses the half where NEITHER the flag nor the package
	// names an instance; this freezes the other half, where the package does
	// name one and that answer must stop being re-asked.
	if baseURL == "" {
		baseURL = strings.TrimRight(pkg.Connector.BaseURL.Default, "/")
	}
	if baseURL == "" {
		return fmt.Errorf("connections add: connector %q names no default instance, so --base-url is required", opts.Connector)
	}
	caps, err := parseCapabilities(opts.Capabilities)
	if err != nil {
		return err
	}

	// The warning sink every other caller passes (`secret set`,
	// `localConnectorsForRun`). Opening the sealer can MINT the master key and
	// write it base64 to ~/.iterion/secrets.key when the OS keychain is
	// unavailable — a headless host, a CI container, SSH with no D-Bus session
	// — and LoadOrCreateMasterKey no-ops a nil logf. So the one command that
	// first creates that key for an operator who only uses connectors was the
	// one command that did not say where it landed.
	sealer, err := secrets.NewLocalSealer(store.GlobalIterionDataDir(), warnTo(out))
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
	// The PLACEMENT is pinned beside the origin, and for the same reason: the
	// package is resolved again at every call, and a granted
	// `<workspace>/connectors` directory outranks every other tier. Pinning
	// only the host leaves a
	// shadowing package free to keep the scheme id while moving the credential
	// from a header into a query string — where the vendor's logs, proxies and
	// referrers keep it. What a credential may be sent to, AND how, is decided
	// when it is entrusted.
	authScheme, ok := pkg.Connector.AuthScheme(scheme)
	if !ok {
		return fmt.Errorf("connections add: connector %q declares no auth scheme %q", opts.Connector, scheme)
	}
	placement := connection.PlacementOf(authScheme)

	conn := connection.Connection{
		ID: id, TenantID: connection.LocalTenant,
		Connector: opts.Connector, Alias: alias,
		DisplayName: opts.DisplayName, BaseURL: baseURL,
		SchemeID: scheme, AuthPlacement: placement, Capabilities: caps,
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

	// The pinned origin, always: it is what this credential may now be sent to,
	// and the operator is the only one who can say it is wrong.
	fmt.Fprintf(out, "connected %s as %q → %s\n", opts.Connector, alias, conn.BaseURL)
	// The RESOLVED capabilities, not the flag. An operator who named none gets
	// `action` by default, and echoing the empty flag reported a grant that
	// differs from the record just written — `connections list` then says
	// `action` for the same connection, which is the shape of a bug report
	// rather than of a default.
	// The placement is printed with the scheme, because "token" alone does not
	// tell an operator whether their PAT rides a header or a query string.
	fmt.Fprintf(out, "  scheme: %s (%s) · capabilities: %s\n", scheme, placement.Describe(), capabilityNames(caps))
	fmt.Fprintf(out, "  a .bot reaches it with `connection: %s`\n", alias)
	// A self-hosted instance is the common case for the connectors this
	// catalog ships, and the guarded dialer refuses one by default. Said here
	// rather than left for the first run to discover: the record is still
	// created — the environment can change, and this command is not the place
	// to decide a deployment's network policy.
	if advice := connection.UnreachableBaseURL(context.Background(), conn.BaseURL); advice != "" {
		fmt.Fprintf(out, "  warning: %s\n", advice)
	}
	if advice := connection.CleartextOrigin(conn.BaseURL); advice != "" {
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
		if advice := connection.CleartextOrigin(c.BaseURL); advice != "" {
			fmt.Fprintf(out, "  warning: %s\n", advice)
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
	// The SAME tier construction a run uses, not a second copy of it: the two
	// had drifted into one shared defect (an unreadable root read as an absent
	// one), and a command whose whole job is to refuse what a run would refuse
	// cannot answer that question from its own reading of the disk.
	tiers, err := connection.LocalCatalogs(paths)
	if err != nil {
		return nil, err
	}
	if len(tiers) == 0 {
		// The project root is only half an expectation while the grant is
		// closed: naming it unconditionally sent an operator to create a
		// directory this process would then not consult.
		if paths.Project != "" && connection.ProjectCatalogGranted() {
			return nil, fmt.Errorf("no connector catalog found — expected %s or %s", paths.Project, paths.Home)
		}
		return nil, fmt.Errorf("no connector catalog found — expected %s (a project catalog at %s is consulted only with %s=1)",
			paths.Home, paths.Project, connection.ProjectCatalogEnv)
	}
	return connection.NewLayeredCatalog(tiers...), nil
}

// resolveScheme picks the auth scheme, refusing to guess when the package
// declares several: placement differs between them, so a wrong guess is a 401
// that reads like a bad credential and sends an operator to rotate a token
// that is fine.
// checkBaseURL refuses an instance URL this command would store and no call
// could ever use.
//
// Both cases are a connection that is written, listed and reported as
// connected, and then fails at the first action node with a message a layer
// away from the mistake:
//
//   - The package is OPERATOR-SUPPLIED and declares no default — the shipped
//     Forgejo package is exactly this — so an omitted --base-url stored "" and
//     `add` printed "→  (the package default)", naming a default that does not
//     exist. `exec.resolveURL` then refuses every call with "connector
//     %q is operator-supplied and the connection names no instance URL".
//   - A URL with no scheme ("git.example.com") parses without error and has an
//     empty Hostname, so `UnreachableBaseURL` says nothing about it either.
//     The transport then fails with `unsupported protocol scheme ""`.
//
// Refused HERE because this is where the operator typed it, which is the same
// reason `UnreachableBaseURL` gives its advice at this moment rather than at
// call time.
func checkBaseURL(pkg *spec.Package, baseURL string) error {
	if baseURL == "" {
		if pkg.Connector.BaseURL.OperatorSupplied && pkg.Connector.BaseURL.Default == "" {
			return fmt.Errorf("connections add: connector %q is self-hosted and declares no default instance — pass --base-url (e.g. --base-url https://git.example.com)", pkg.Connector.ID)
		}
		return nil
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("connections add: --base-url %q does not parse: %w", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("connections add: --base-url %q needs an http or https scheme (e.g. https://%s)", baseURL, strings.TrimPrefix(baseURL, "//"))
	}
	if u.Host == "" {
		return fmt.Errorf("connections add: --base-url %q names no host", baseURL)
	}
	return nil
}

// checkSchemeIsWritable refuses a scheme this command cannot supply the
// material for.
//
// `add` seals a TOKEN (there is no --username-env, and `connection.SealBasic`
// has no caller), so choosing a basic scheme produced a record whose username
// and password are empty: `connections list` showed it, `checkUsable` passed
// it, and every call was refused locally with "basic auth needs a username" —
// a message pointing nowhere near the command that created the record. Forgejo
// makes this easy to hit: it declares several schemes, so --scheme is
// mandatory, and the disambiguation error lists `basic` first.
func checkSchemeIsWritable(pkg *spec.Package, schemeID string) error {
	s, ok := pkg.Connector.AuthScheme(schemeID)
	if !ok || s.Kind != spec.AuthBasic {
		return nil
	}
	return fmt.Errorf("connections add: scheme %q of connector %q is HTTP basic, which needs a username and a password — this command seals a token only; name a token-shaped scheme with --scheme", schemeID, pkg.Connector.ID)
}

// warnTo sends the sealer's warnings where the command's own output goes, so a
// master key written to disk is reported on the surface the operator is
// reading.
func warnTo(out io.Writer) func(string, ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(out, "warning: "+format+"\n", args...)
	}
}

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
