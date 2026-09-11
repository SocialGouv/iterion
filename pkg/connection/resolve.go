package connection

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Resolver answers the question a `tool … action:` node asks: given an
// operation id and a connection alias, which package, which operation, which
// credential, and against which host.
//
// It implements `model.ConnectorResolver` without importing it — the interface
// is declared where it is CONSUMED, so this package does not depend on the
// backend and the backend does not depend on this one. Neither has to exist
// for the other to compile, which is what let the node ship before the
// connection layer did.
type Resolver struct {
	// Catalog resolves the connector id to its package.
	Catalog Catalog
	// Store resolves the alias to a connection. Reads are tenant-scoped.
	Store Store
	// Sealer opens the credential. Required: without one, a resolver would
	// have to either fail or run unsealed, and the second is worse.
	Sealer secrets.Sealer
	// TenantID is whose connections this resolver may read.
	//
	// A FIELD, not a parameter threaded from the caller. A resolver is built
	// per run, by whoever knows which tenant the run belongs to, and then the
	// answer is fixed: a node cannot name a tenant, so nothing a workflow
	// writes can widen it. Threading it from the call site would put the
	// decision back where the untrusted text is.
	TenantID string
}

// ResolveAction implements the executor's seam.
//
// The order is deliberate. The OPERATION is resolved first, then the
// connection, then the credential — cheapest and least sensitive first, so a
// misspelt operation id never causes a credential to be unsealed. Each step's
// failure names what to fix, because every one of them surfaces to an operator
// as "the node failed" otherwise.
func (r *Resolver) ResolveAction(ctx context.Context, actionID, alias string) (*spec.Package, spec.Operation, exec.Credential, string, error) {
	var zero spec.Operation
	if r == nil || r.Catalog == nil || r.Store == nil {
		return nil, zero, exec.Credential{}, "", errors.New("connection: resolver is not wired (catalog or store missing)")
	}
	if r.TenantID == "" {
		// Refused rather than defaulted to "": an empty tenant matches the
		// records that also have none, which on a shared deployment is
		// whatever a migration left behind.
		return nil, zero, exec.Credential{}, "", errors.New("connection: resolver has no tenant, so it may read nobody's connections")
	}

	connectorID, _, ok := strings.Cut(actionID, ".")
	if !ok || connectorID == "" {
		return nil, zero, exec.Credential{}, "", fmt.Errorf("action %q is not an operation id — it must read `connector.resource.verb`", actionID)
	}

	pkg, err := r.Catalog.Package(connectorID)
	if err != nil {
		return nil, zero, exec.Credential{}, "", err
	}
	op, found := pkg.Operation(actionID)
	if !found {
		return nil, zero, exec.Credential{}, "", fmt.Errorf("connector %q (v%s) has no operation %q%s",
			connectorID, pkg.Connector.Version, actionID, suggestOperation(pkg, actionID))
	}

	conn, err := r.Store.ByAlias(ctx, r.TenantID, connectorID, alias)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, zero, exec.Credential{}, "", fmt.Errorf("no %q connection named %q for this tenant%s",
				connectorID, alias, r.suggestAlias(ctx, connectorID))
		}
		return nil, zero, exec.Credential{}, "", fmt.Errorf("resolve connection %q: %w", alias, err)
	}
	if err := r.checkUsable(conn, pkg, op); err != nil {
		return nil, zero, exec.Credential{}, "", err
	}

	blob, err := openCredential(r.Sealer, conn)
	if err != nil {
		return nil, zero, exec.Credential{}, "", err
	}

	cred := exec.Credential{
		SchemeID: conn.SchemeID,
		Value:    blob.Token,
		Username: blob.Username,
		Password: blob.Password,
	}
	// Granted scopes travel ONLY when the provider actually said them. An
	// empty list on a connection whose scopes are unknown must stay empty, so
	// the executor keeps reading it as "cannot tell" — the third state the
	// record exists to preserve. Filling it in with nothing here would collapse
	// unknown into none at the last step.
	if conn.ScopesKnown {
		cred.Scopes = conn.GrantedScopes
	}

	// The connection's OWN origin, with no fallback to the package's.
	//
	// Falling back meant "whatever the package says when the call happens", so
	// replacing the package — or shadowing it with a project-tier one —
	// redirected an existing credential to a different host, with nothing in
	// the run saying so. `connections add` resolves and pins the origin at
	// creation for exactly this reason; a record without one predates that or
	// was hand-written, and guessing on its behalf is the vector itself.
	if conn.BaseURL == "" {
		return nil, zero, exec.Credential{}, "", fmt.Errorf(
			"connection %q names no instance URL, so there is nothing to say where its credential may be sent; re-create it with --base-url", conn.Alias)
	}
	return pkg, op, cred, conn.BaseURL, nil
}

// checkUsable refuses a connection that must not serve this call.
//
// Checked at the moment of USE rather than trusted from the record's own
// status, because both facts change under the run: a credential is revoked
// while a loop iterates, an operator narrows a capability between two nodes.
func (r *Resolver) checkUsable(conn Connection, pkg *spec.Package, op spec.Operation) error {
	if !conn.Status.Usable() {
		reason := conn.StatusReason
		if reason == "" {
			reason = "no reason recorded"
		}
		return fmt.Errorf("connection %q is %s: %s", conn.Alias, conn.Status, reason)
	}
	// An expiry iterion ALREADY HOLDS is refused here rather than spent. The
	// vendor would answer 401, which reads like a bad token and sends an
	// operator to rotate one that is merely out of date — and on a mutating
	// operation it costs a call whose effect has to be reasoned about. Zero
	// means a credential that does not expire.
	if !conn.ExpiresAt.IsZero() && !conn.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("connection %q expired at %s — reconnect it",
			conn.Alias, conn.ExpiresAt.UTC().Format(time.RFC3339))
	}
	// The PLACEMENT the package now describes must be the one this credential
	// was entrusted under.
	//
	// The package is resolved again at every call and a `<workspace>/connectors`
	// directory outranks every other tier, so the id alone guarantees nothing:
	// a repository can ship a package keeping scheme "token" while moving the
	// value out of an `Authorization` header and into a query string, where it
	// lands in the vendor's logs, its proxies and its referrers. Pinning the
	// origin closed the "another host" half; this is the "same host, somewhere
	// else" half, and the two together are what "where a credential may be
	// sent is decided when it is entrusted" actually means.
	//
	// Refused on MISMATCH rather than on any change to the package, so an
	// ordinary catalog update keeps every connection working and only a moved
	// credential needs re-consenting.
	scheme, found := pkg.Connector.AuthScheme(conn.SchemeID)
	if !found {
		return fmt.Errorf("connection %q names scheme %q, which connector %q no longer declares — reconnect it against the package now installed",
			conn.Alias, conn.SchemeID, pkg.Connector.ID)
	}
	if got := placementOf(scheme); !conn.AuthPlacement.Equal(got) {
		return fmt.Errorf("connection %q was entrusted for %s and the package now asks for %s — refusing rather than sending the credential somewhere it was not granted to go; reconnect it if the change is intended",
			conn.Alias, conn.AuthPlacement.Describe(), got.Describe())
	}
	// The capability is the whole answer to "may this connection be used for
	// this?". A deterministic node and an agent facade are different uses of
	// one credential, and an operator may legitimately allow one and not the
	// other.
	if !conn.Has(CapAction) {
		return fmt.Errorf("connection %q may not serve a deterministic node: it declares %s, not %q — an operator grants that use deliberately",
			conn.Alias, describeCaps(conn.Capabilities), CapAction)
	}
	// The scheme the connection holds must be one the operation accepts, and
	// the grant must cover what the operation needs. Refused locally: the
	// vendor answers 403, which reads like a bad credential and sends an
	// operator to rotate a token that is fine.
	//
	// The two branches are the point. When the provider TOLD us what the
	// credential carries, the scope check is real and names what is missing.
	// When it did not — which is most PATs, since few providers enumerate a
	// token's grants — refusing would make every such connection unusable,
	// and granting would authorise whatever is asked. So the conjunction is
	// still enforced (a requirement naming two schemes cannot be met by a
	// connection holding one) while the scopes are left unjudged.
	// The EFFECTIVE requirements: an operation that declares none inherits the
	// connector's, and reading only its own accepted a connection against a
	// requirement it had never been checked for. A connector-wide `admin`
	// scope, with an operation declaring nothing, was simply not seen here.
	probe := op
	probe.Security = pkg.EffectiveSecurity(op)
	if len(probe.Security) > 0 {
		probe.Anonymous = false
	}
	if conn.ScopesKnown {
		if ok, missing := probe.SatisfiedBy(conn.SchemeID, conn.GrantedScopes); !ok {
			return fmt.Errorf("connection %q cannot perform %q: %s", conn.Alias, op.ID, strings.Join(missing, ", "))
		}
		return nil
	}
	// spec.SatisfiableBy is the ONE definition of "could this scheme do it if
	// its own scopes were granted", shared with the executor's pre-call check.
	// Two copies of that reasoning would drift, and the drift would be a
	// silent authorisation difference between the layer that hands over a
	// credential and the layer that spends it.
	if ok, missing := probe.SatisfiableBy(conn.SchemeID); !ok {
		return fmt.Errorf("connection %q cannot perform %q (its granted scopes are unknown, so only the scheme was checked): %s",
			conn.Alias, op.ID, strings.Join(missing, ", "))
	}
	return nil
}

// suggestAlias names the aliases the tenant does have for this connector. A
// bare "not found" on a name the operator is sure they created usually means a
// different connector, and listing what exists says so in one line.
func (r *Resolver) suggestAlias(ctx context.Context, connector string) string {
	all, err := r.Store.List(ctx, r.TenantID, connector)
	if err != nil || len(all) == 0 {
		return " (this tenant has no " + connector + " connection at all)"
	}
	names := make([]string, 0, len(all))
	for _, c := range all {
		names = append(names, c.Alias)
	}
	return " (it has: " + strings.Join(names, ", ") + ")"
}

// suggestOperation points at the nearest operation id, which for a mistyped
// verb is almost always the one meant.
func suggestOperation(pkg *spec.Package, want string) string {
	var near []string
	for _, f := range pkg.Ops {
		for _, op := range f.Operations {
			if strings.EqualFold(op.ID, want) {
				return " (did you mean " + op.ID + "? — ids are case-sensitive)"
			}
			if len(near) < 4 && sharesPrefix(op.ID, want) {
				near = append(near, op.ID)
			}
		}
	}
	if len(near) == 0 {
		return ""
	}
	return " (near: " + strings.Join(near, ", ") + ")"
}

// sharesPrefix reports whether two operation ids agree up to their last
// segment — same connector and resource, different verb.
func sharesPrefix(a, b string) bool {
	ai := strings.LastIndex(a, ".")
	bi := strings.LastIndex(b, ".")
	return ai > 0 && bi > 0 && a[:ai] == b[:bi]
}

func describeCaps(caps []Capability) string {
	if len(caps) == 0 {
		return "no capability"
	}
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = string(c)
	}
	return strings.Join(out, "+")
}

// placementOf reads a package scheme's placement, and is the ONE definition of
// what gets pinned and what gets compared.
//
// Two copies — one at `connections add`, one here — is how a field added to
// AuthPlacement ends up pinned and never checked, which reads exactly like a
// guarantee and is none.
func placementOf(s spec.AuthScheme) AuthPlacement {
	return AuthPlacement{
		Kind:        string(s.Kind),
		In:          s.In,
		Name:        s.Name,
		ValuePrefix: s.ValuePrefix,
	}
}

// PlacementOf is placementOf for the command that PINS it, so the writer and
// the checker cannot disagree.
func PlacementOf(s spec.AuthScheme) AuthPlacement { return placementOf(s) }
