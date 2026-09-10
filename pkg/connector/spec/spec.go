// Package spec is the connector catalog's declarative model: what a
// connector package IS, independent of where it came from (a generated
// vendor OpenAPI description, a hand-authored overlay, or both) and of who
// executes it (the deterministic node path on the runner, or the MCP facade
// on the server).
//
// A connector package is a directory:
//
//	connector.yaml   identity, provenance/licence, base URL, auth schemes, maturity
//	ops/*.yaml       operations, one file per domain
//	overlay.yaml     the curation a machine cannot derive
//	triggers.yaml    inbound webhooks / polling
//
// Two properties are load-bearing and shape every type below.
//
// GENERATED IS NOT AUTHORED. Everything under ops/ is derivable from the
// vendor's description, so it is disposable and re-derivable; everything in
// overlay.yaml is iterion's own work. Provenance records which is which,
// because it decides what may be REDISTRIBUTED: a vendor spec under a
// non-commercial licence may be generated from locally by an operator, but
// its derived ops may not ship in iterion's own catalog.
//
// DECLARATIVE MEANS NO CODE. An operation is data — a method, a path, typed
// parameters, a typed result, error classes — so the same package serves the
// deterministic node path (no LLM anywhere) and the MCP facade. Anything
// needing logic is an explicitly declared escape hatch, never an implicit one.
package spec

// SchemaVersion is the current connector.yaml / ops schema. It is read by a
// tolerant PRE-PASS before the strict decode, never after: a package written
// for a newer iterion must fail with "upgrade iterion", not with an opaque
// "unknown field" from the strict decoder. (pkg/plugin's manifest has the
// inverse order and its version diagnostic is consequently unreachable.)
const SchemaVersion = 1

// Connector is a parsed connector.yaml — the package's identity card. It
// carries no operation: those live in ops/ so a large catalog entry stays
// readable and diffable per domain.
type Connector struct {
	SchemaVersion int `yaml:"schema_version" json:"schema_version"`

	// ID is the package's stable slug (kebab-case), unique in a catalog and
	// the first segment of every operation id it declares.
	ID string `yaml:"id" json:"id"`
	// DisplayName is the operator-facing label; Description is one line.
	DisplayName string `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Version is the PACKAGE's semver, bumped by whoever publishes it. It is
	// not the upstream API's version — Provenance.SpecVersion carries that.
	Version string `yaml:"version" json:"version"`

	Provenance Provenance `yaml:"provenance" json:"provenance"`
	BaseURL    BaseURL    `yaml:"base_url" json:"base_url"`
	// Auth lists every scheme the API accepts, in the order a connection
	// wizard should offer them. A connection names one by its ID.
	Auth []AuthScheme `yaml:"auth" json:"auth"`
	// Maturity is the package-wide floor. A capability may declare its own,
	// never HIGHER than this one — a package nobody has qualified cannot
	// contain a qualified operation.
	Maturity Maturity `yaml:"maturity" json:"maturity"`

	// Outcome describes an API that signals failure inside a successful HTTP
	// status. It sits here rather than on each operation because an API that
	// does this does it everywhere — Slack's entire Web API answers 200 with
	// `{"ok": false}` — and an operation may still override.
	Outcome *OutcomePolicy `yaml:"outcome,omitempty" json:"outcome,omitempty"`

	// DefaultSecurity applies to every operation that declares none of its
	// own, mirroring both formats' root-level `security`.
	DefaultSecurity []SecurityRequirement `yaml:"default_security,omitempty" json:"default_security,omitempty"`
}

// Provenance records where the package's content came from and what may be
// done with it. It exists because "we generated this from their spec" and "we
// may ship this" are different questions, and only the second one is answered
// by a licence.
type Provenance struct {
	// SpecURL is the vendor description the ops were generated from; empty
	// for a hand-authored package.
	SpecURL string `yaml:"spec_url,omitempty" json:"spec_url,omitempty"`
	// SpecFormat is "openapi3" | "swagger2" — recorded because the generator
	// accepts both and the two express security and bodies differently.
	SpecFormat string `yaml:"spec_format,omitempty" json:"spec_format,omitempty"`
	// SpecVersion is the upstream API version from the description's own info
	// block, so a regeneration can be compared with what a package was built
	// from.
	SpecVersion string `yaml:"spec_version,omitempty" json:"spec_version,omitempty"`
	// SpecLicense is the licence the DESCRIPTION carries (an SPDX id when it
	// maps cleanly, else the vendor's own wording). Verified, never assumed:
	// of the five specs surveyed for the pilot, four are permissive and one
	// (Mattermost) is CC BY-NC-SA — which is invisible from the endpoint list.
	SpecLicense string `yaml:"spec_license,omitempty" json:"spec_license,omitempty"`
	// Redistributable states whether the GENERATED ops may ship inside
	// iterion's own catalog. When false, iterion publishes the overlay alone
	// and the operator's instance generates ops locally from SpecURL — licit
	// (private use), and it keeps the ops fresh besides.
	//
	// It is a deliberate assertion by whoever publishes the package, not a
	// guess derived from SpecLicense: an unrecognised licence string must
	// read as "no", and a false here is what keeps a non-commercial spec out
	// of a commercial distribution.
	Redistributable bool `yaml:"redistributable" json:"redistributable"`
	// GeneratedAt is an RFC3339 stamp; GeneratedBy names the iterion build.
	GeneratedAt string `yaml:"generated_at,omitempty" json:"generated_at,omitempty"`
	GeneratedBy string `yaml:"generated_by,omitempty" json:"generated_by,omitempty"`
}

// BaseURL is where a connection's calls go. It is split from the connection
// because the two answer different questions: the PACKAGE knows the API's
// path prefix and whether the product is self-hostable; the CONNECTION knows
// which host this team actually talks to.
type BaseURL struct {
	// Default is the vendor's SaaS origin, empty for a product that has none.
	Default string `yaml:"default,omitempty" json:"default,omitempty"`
	// PathPrefix is the API root under the origin (Swagger 2.0's basePath,
	// e.g. "/api/v1"). Kept apart from the host so a self-hosted connection
	// supplies only the origin and cannot accidentally drop the prefix.
	PathPrefix string `yaml:"path_prefix,omitempty" json:"path_prefix,omitempty"`
	// OperatorSupplied marks a product that is commonly self-hosted, so the
	// connection wizard asks for an instance URL instead of assuming Default.
	OperatorSupplied bool `yaml:"operator_supplied,omitempty" json:"operator_supplied,omitempty"`
}

// AuthKind is how a credential is presented on the wire.
type AuthKind string

const (
	// AuthAPIKey sends a stored value in a header or query parameter,
	// optionally behind a fixed prefix (Forgejo wants "token <value>").
	AuthAPIKey AuthKind = "api_key"
	// AuthBearer is the RFC 6750 "Authorization: Bearer <value>" special case
	// of AuthAPIKey, named separately because every OAuth flow lands on it.
	AuthBearer AuthKind = "bearer"
	// AuthBasic sends username:password.
	AuthBasic AuthKind = "basic"
	// AuthOAuth2 is the authorization-code flow: confidential (a client
	// secret, an iterion-managed or operator-registered app) or public
	// (PKCE). The connection holds the tokens; the package holds the URLs.
	AuthOAuth2 AuthKind = "oauth2"
)

// AuthScheme is one way to authenticate against the API. A package may
// declare several; a connection picks exactly one.
type AuthScheme struct {
	// ID is the scheme's slug within the package ("token", "oauth"), which a
	// connection stores. It is NOT the vendor's securityDefinitions key —
	// that one changes between spec releases and would break stored records.
	ID          string   `yaml:"id" json:"id"`
	Kind        AuthKind `yaml:"kind" json:"kind"`
	DisplayName string   `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`

	// In ("header" | "query") and Name locate an api_key on the wire.
	In   string `yaml:"in,omitempty" json:"in,omitempty"`
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// ValuePrefix is prepended to the stored value ("token ", "Bearer ").
	// Carried here rather than baked into the stored credential so a
	// re-pasted token cannot end up double-prefixed.
	ValuePrefix string `yaml:"value_prefix,omitempty" json:"value_prefix,omitempty"`

	// OAuth2 endpoints, when Kind is AuthOAuth2.
	AuthURL   string `yaml:"auth_url,omitempty" json:"auth_url,omitempty"`
	TokenURL  string `yaml:"token_url,omitempty" json:"token_url,omitempty"`
	RevokeURL string `yaml:"revoke_url,omitempty" json:"revoke_url,omitempty"`

	// SupportedScopes is everything the vendor ADVERTISES for this scheme.
	// DefaultScopes is what a connection asks for when the operator picks no
	// narrower set. They are separate, and neither is "the scopes granted" —
	// which only the provider's answer says. Collapsing the three is how an
	// integration that reads one channel ends up requesting every scope a
	// vendor offers, so a package that cannot tell them apart is a package
	// that cannot be least-privilege.
	SupportedScopes []string `yaml:"supported_scopes,omitempty" json:"supported_scopes,omitempty"`
	DefaultScopes   []string `yaml:"default_scopes,omitempty" json:"default_scopes,omitempty"`
	// PKCE marks a public client (no secret), so a self-hosted deployment can
	// connect without registering a confidential app.
	PKCE bool `yaml:"pkce,omitempty" json:"pkce,omitempty"`
}

// Maturity is a capability's readiness. It is DATA THE SERVER ENFORCES, not a
// display category: MaturitySpotted is inert — visible in the index, refused
// at attachment — so a large generated index never promises what nobody has
// qualified.
type Maturity string

const (
	// MaturitySpotted is a known source with no package anyone can run.
	MaturitySpotted Maturity = "spotted"
	// MaturityExperimental is runnable by explicit operator choice, limits published.
	MaturityExperimental Maturity = "experimental"
	// MaturityQualified is ready-to-use for the capabilities and environments tested.
	MaturityQualified Maturity = "qualified"
	// MaturityDeprecated still resolves for existing bindings, with a reason.
	MaturityDeprecated Maturity = "deprecated"
)

// Attachable reports whether a capability at this maturity may be bound to a
// bot or used by a node. Spotted is the inert level; the empty value reads as
// spotted, so an entry that declares nothing promises nothing.
func (m Maturity) Attachable() bool {
	switch m {
	case MaturityExperimental, MaturityQualified, MaturityDeprecated:
		return true
	}
	return false
}
