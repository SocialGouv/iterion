// Package connection is what authenticates a connector call: the binding
// between a tenant, a connector package, an instance of that vendor, and the
// credential to reach it.
//
// It is the generalization `pkg/forge`'s connection model becomes (ADR-098):
// the provider enum opens into a connector id, the auth scheme comes from the
// package rather than a hardcoded list, and the forges become connectors #1-3.
// This package is the NEW half of that expand-and-contract; `pkg/forge` still
// owns the forge connections and keeps its ids, its `forge_conn:` sealing AAD
// and its callback URLs until the contract phase moves them.
//
// # Four properties held by construction, not by discipline
//
// Each one is a defect the existing code demonstrates, so each is designed
// out rather than documented against.
//
//  1. EVERY read is tenant-scoped, in the SIGNATURE. `forge.ConnectionStore.Get`
//     filters by `_id` alone, so any caller holding an id reaches any tenant's
//     connection; the tenant here is a positional argument, so the hole cannot
//     be reintroduced by forgetting a filter.
//  2. Capabilities are EXPLICIT. A connection is eligible for a use only if it
//     says so, which is what keeps a connection that merely shares a host from
//     being picked up by a resolver looking for something else.
//  3. The credential is EXECUTION-ONLY. Nothing exported here returns a token:
//     the sealed blob opens on one path, inside a call, into the value the HTTP
//     executor needs. Making it reachable any other way is a visible edit to
//     this package, not an accident at a call site.
//  4. Granted scopes distinguish UNKNOWN from NONE. An empty list read as "no
//     scopes" refuses every PAT-backed connection, since most providers never
//     say what a token carries; read as "all scopes" it grants everything. It
//     is a third state, and callers are made to handle it.
package connection

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors a store returns. Callers distinguish "no such connection" from "not
// yours": both must read the same to an untrusted caller, which is why the
// store answers ErrNotFound for a connection belonging to another tenant.
var (
	ErrNotFound = errors.New("connection: not found")
	ErrExists   = errors.New("connection: already exists")
)

// Status is a connection's usability.
type Status string

const (
	// StatusActive is usable now.
	StatusActive Status = "active"
	// StatusDegraded is a connection whose credential still works for some
	// operations but which has a problem an operator should see — a scope
	// withdrawn, a refresh that failed once. Still selectable: degrading to
	// unusable on the first hiccup is how a fleet loses a working credential.
	StatusDegraded Status = "degraded"
	// StatusRevoked is unusable and will not recover by itself: the credential
	// was withdrawn at the vendor, or the operator disconnected it.
	StatusRevoked Status = "revoked"
)

// Usable reports whether a connection may serve a call. Degraded counts —
// see StatusDegraded.
func (s Status) Usable() bool { return s == StatusActive || s == StatusDegraded }

// Capability is a use a connection is eligible for.
//
// The set is deliberately small and grows only when something reads it. A
// capability nobody checks is configuration that looks like a guarantee — the
// failure mode this package exists to avoid, and one this branch has already
// paid for once in a DSL field that compiled and was read by nobody.
type Capability string

const (
	// CapAction lets the connection serve a `tool … action:` node — a
	// deterministic call with no model in the path.
	CapAction Capability = "action"
	// CapAgent lets the connection serve an agent through the MCP facade,
	// where the model chooses the operation.
	//
	// Separate from CapAction on purpose: an operator may well want a
	// credential that automation may spend and an agent may not, and the two
	// differ in who decides what is called.
	CapAgent Capability = "agent"
)

// ValidCapability reports whether c is one this build understands. An unknown
// capability on a stored record is REFUSED rather than ignored: ignoring it
// would silently narrow a connection an operator widened, and on a mixed-version
// fleet the older replica would be the one deciding.
func ValidCapability(c Capability) bool {
	switch c {
	case CapAction, CapAgent:
		return true
	}
	return false
}

// Connection binds a tenant's credential to one instance of one connector.
type Connection struct {
	// ID is stable and opaque.
	ID string `bson:"_id" json:"id"`
	// TenantID owns this connection. Every store read takes it separately and
	// matches it — see the package doc.
	TenantID string `bson:"tenant_id" json:"tenant_id"`

	// Connector is the package id ("forgejo", "slack"). It replaces the forge
	// Provider enum, which is what made every new integration an engine change.
	Connector string `bson:"connector" json:"connector"`

	// Alias is the name a `.bot` writes in `connection:`. Unique per tenant
	// and per connector, so a workflow addresses "the production Forgejo"
	// without knowing an opaque id — and so the same `.bot` runs for two
	// tenants whose aliases match and whose credentials do not.
	Alias string `bson:"alias" json:"alias"`

	// DisplayName is what an operator reads in the studio. Cosmetic.
	DisplayName string `bson:"display_name,omitempty" json:"display_name,omitempty"`

	// BaseURL pins the instance this connection authenticates to. Empty means
	// the package's own default, which is right for a SaaS with one host and
	// wrong for everything self-hosted — the same Forgejo package serves
	// codeberg.org and an internal instance, and only the connection knows
	// which.
	BaseURL string `bson:"base_url,omitempty" json:"base_url,omitempty"`

	// SchemeID names which of the package's auth schemes this credential
	// satisfies. Stored rather than guessed: a package may declare several,
	// and placement (header, query, prefix) differs between them, so a wrong
	// guess is a 401 that reads like a bad credential.
	SchemeID string `bson:"scheme_id" json:"scheme_id"`

	// Capabilities is what this connection may be used FOR. Empty means
	// nothing — a connection that declares no use has none, rather than all.
	Capabilities []Capability `bson:"capabilities,omitempty" json:"capabilities,omitempty"`

	// GrantedScopes is what the provider said the credential carries, and
	// ScopesKnown is whether it said anything at all.
	//
	// Two fields rather than one because an empty list is genuinely ambiguous
	// and both readings are wrong: as "no scopes" it refuses every PAT-backed
	// connection, since most providers never enumerate a token's grants; as
	// "all scopes" it authorises whatever the workflow asks. Only the pair can
	// say "this token carries exactly these" versus "nobody told us".
	GrantedScopes []string `bson:"granted_scopes,omitempty" json:"granted_scopes,omitempty"`
	ScopesKnown   bool     `bson:"scopes_known,omitempty" json:"scopes_known,omitempty"`

	Status Status `bson:"status" json:"status"`
	// StatusReason explains a non-active Status in words an operator can act
	// on. Cleared when the connection returns to active.
	StatusReason string `bson:"status_reason,omitempty" json:"status_reason,omitempty"`

	// SealedPayload is the credential, sealed with AAD "connection:<ID>".
	// NEVER serialised to JSON: this struct reaches the studio.
	SealedPayload []byte `bson:"sealed_payload,omitempty" json:"-"`

	// ExpiresAt is when the credential stops working, zero for one that does
	// not expire.
	//
	// Read by the resolver, which refuses a connection whose expiry has passed
	// instead of sending a credential it knows is dead. Kept on the RECORD as
	// well as inside the sealed blob so that selecting what is due does not
	// mean unsealing every credential to find out.
	//
	// No write path sets a non-zero value yet: the OAuth tier that will, and
	// the worker that would renew one, are tracked in ADR-098. The guard is
	// here rather than with them because the field is public on this struct
	// and serialised, so any writer reaching it finds the refusal already in
	// place.
	ExpiresAt time.Time `bson:"expires_at,omitempty" json:"expires_at,omitempty"`

	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// Has reports whether the connection is eligible for c.
func (c Connection) Has(cap Capability) bool {
	for _, got := range c.Capabilities {
		if got == cap {
			return true
		}
	}
	return false
}

// Validate refuses a connection that could not be used coherently.
//
// Called on every write, because the alternative is discovering an incoherent
// record at call time — where the diagnosis is a vendor error that names none
// of this.
func (c Connection) Validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("connection: missing id")
	}
	if strings.TrimSpace(c.TenantID) == "" {
		// Refused rather than defaulted: a connection with no owner is
		// reachable by everyone, which is the exact hole this package's
		// tenant-scoped reads exist to close.
		return fmt.Errorf("connection %q: missing tenant", c.ID)
	}
	if strings.TrimSpace(c.Connector) == "" {
		return fmt.Errorf("connection %q: missing connector id", c.ID)
	}
	if strings.TrimSpace(c.Alias) == "" {
		return fmt.Errorf("connection %q: missing alias — it is the name a .bot writes in `connection:`", c.ID)
	}
	if strings.TrimSpace(c.SchemeID) == "" {
		return fmt.Errorf("connection %q: names no auth scheme; placement differs between a package's schemes, so guessing one is a 401 that reads like a bad credential", c.ID)
	}
	if len(c.Capabilities) == 0 {
		return fmt.Errorf("connection %q: declares no capability, so nothing may use it — say `action`, `agent`, or both", c.ID)
	}
	seen := make(map[Capability]bool, len(c.Capabilities))
	for _, cap := range c.Capabilities {
		if !ValidCapability(cap) {
			return fmt.Errorf("connection %q: unknown capability %q — this build understands %q and %q", c.ID, cap, CapAction, CapAgent)
		}
		if seen[cap] {
			return fmt.Errorf("connection %q: capability %q is listed twice", c.ID, cap)
		}
		seen[cap] = true
	}
	if c.Status == "" {
		return fmt.Errorf("connection %q: missing status", c.ID)
	}
	if !c.Status.Usable() && c.Status != StatusRevoked {
		return fmt.Errorf("connection %q: unknown status %q", c.ID, c.Status)
	}
	if len(c.GrantedScopes) > 0 && !c.ScopesKnown {
		// The pair has to stay coherent, or the ambiguity it exists to
		// resolve comes back through the record itself.
		return fmt.Errorf("connection %q: lists granted scopes but says they are unknown", c.ID)
	}
	return nil
}
