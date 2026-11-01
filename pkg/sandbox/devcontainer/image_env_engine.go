package devcontainer

import (
	"strings"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// Package-level file: how the image ENV screen answers the engine's OWN
// namespace, `ITERION_`.
//
// Every other namespace of the class answers wholesale — a provider's
// namespace is a route and a credential, the loader's and git's families
// are code the image itself ships. This one is MIXED, and both halves are
// measured rather than preferred:
//
//   - iterion's own published images bake `ITERION_VERSION`,
//     `ITERION_COMMIT` and `ITERION_BOTS_PATH` into their `ENV` on purpose
//     (the repository's Dockerfiles; pkg/platformcfg's
//     TestTheEngineNamesOurOwnImagesBakeStayReported reads them from there),
//     so neutralising the namespace wholesale would empty three variables
//     our own artefacts set;
//   - some of the names the engine reads are exactly the credential/routing
//     kind this screen exists for: the stores' URLs and passwords, the
//     OAuth wires the operator's subscription credential is refreshed on,
//     the platform policy that decides WHICH credential serves a route, and
//     the switches that LIFT a guard on reaching a private host, on which
//     credential is used, on what a project `.env` may plant, on which
//     package serves a consented connection, or on which of the launcher's
//     variables a child receives — the last of which the shared shape
//     cannot spell at all.
//
// So the namespace is answered name by name (#2859), by the SHARED
// credential-routing shape plus the rows below, and the partition is swept
// over the engine's own inventory — every `ITERION_` name its Go source,
// the vendored claw and the manifests it embeds spell
// (pkg/platformcfg's TestEveryEngineEnvNameTheImageScreenSeesIsClassified).

// engineImageEnvDisposition answers [screenEnvName] for a name of the
// engine's own namespace: the credential/routing sub-class is neutralised,
// everything else is reported and keeps the image's value (ARB-121).
//
// Three forms, in this order:
//
//  1. an exact ROW, which is how a name escapes the shape's reach in
//     EITHER direction — the shape reads a credential word this name does
//     not carry (reported), or the name lifts a guard on a route or on a
//     credential while carrying no credential word and naming no host
//     (neutralised);
//  2. a FAMILY row, which is how a sub-namespace whose every member
//     chooses a credential or a route answers for the member the shape
//     cannot see (and for the next one it gains);
//  3. the shared shape class — [envtrust.RoutingShapedName] (the name
//     decides where a connection is dialled) and
//     [envtrust.CredentialShapedName] (the name reads as a credential, or
//     lifts a guard on one). It is the SAME predicate every dotenv loader
//     refuses a plant by (ARB-122), asked here of the other channel into
//     the same environment: one class, three gates — the bot-var gate
//     (pkg/platformcfg), the dotenv plant (envtrust.DotenvPlantRefused)
//     and this screen — so a credential spelling added to it answers in
//     all three the day it lands.
//
// [notDenied] when no form answers: the namespace's own row carries the
// default, which is reported, and what that default leaves open is stated as
// residue in docs/sandbox.md — a name that chooses neither a credential nor
// a route, and lifts no guard on either (the permission gate's mode, a
// binary a step runs, the isolation tier, a bound on what a run may spend),
// keeps the image's value, because an image a repository chooses is the
// repository's own code and an empty value buys no promise its files and its
// `postCreateCommand` do not owe.
func engineImageEnvDisposition(upper string) imageEnvDisposition {
	if row, answered := engineImageEnvRow(upper); answered {
		return row.image
	}
	if envtrust.RoutingShapedName(upper) || envtrust.CredentialShapedName(upper) {
		return neutralizeInImage
	}
	return notDenied
}

// EngineImageEnvReason returns the reason the ROW that answers an engine
// name gives it, and "" for a name no row answers — there the shared shape
// class and the namespace's reported default answer, and their reason is
// [engineImageEnvDisposition]'s own doc rather than a row.
//
// Exported for the sweep that holds this partition to the engine's
// inventory: that inventory is scanned where it lives (pkg/platformcfg
// walks the Go source, the vendored claw and the embedded manifests for the
// bot-var gate), and the reason must travel from the PRODUCTION row rather
// than from a copy of it in a test — a copy is what drifts the day a row
// changes side.
func EngineImageEnvReason(name string) string {
	row, answered := engineImageEnvRow(strings.ToUpper(name))
	if !answered {
		return ""
	}
	return row.reason
}

// engineImageEnvRow is the one walk of the rows: the exact names first, then
// the families. Both halves of a family and its exceptions therefore read
// one answer, whichever reader asks.
//
// The two exact tables are disjoint — image_env_engine_test.go refuses a
// name in both — so their order is immaterial too. The families are a map
// and their order is immaterial as well: they all answer neutralizeInImage,
// and no family prefixes another, because the REASON this returns would then
// depend on which row a range hit first. An exact row is consulted before
// them, which is how `ITERION_OIDC_GENERIC_DISPLAY_NAME` stays reported
// inside a neutralised family.
func engineImageEnvRow(upper string) (engineEnvRow, bool) {
	if reason, exact := engineEnvNeutralisedNames[upper]; exact {
		return engineEnvRow{image: neutralizeInImage, reason: reason}, true
	}
	if reason, exact := engineEnvReportedNames[upper]; exact {
		return engineEnvRow{image: reportedOnly, reason: reason}, true
	}
	for prefix, reason := range engineEnvNeutralisedFamilies {
		if strings.HasPrefix(upper, prefix) {
			return engineEnvRow{image: neutralizeInImage, reason: reason}, true
		}
	}
	return engineEnvRow{}, false
}

// engineEnvRow is what a row of the two tables below answers: the
// disposition, and the reason it earns — the reason rides on the ROW, so
// the sweep that measures the class prints the production text.
type engineEnvRow struct {
	image  imageEnvDisposition
	reason string
}

// engineEnvNeutralisedNames are the engine names that LIFT a guard on a
// route or on a credential while carrying no credential word and naming no
// host — the sub-class the shared shape cannot spell, and the other
// direction of the shape's imprecision: it catches a guard lift only where
// a credential or host word leaks into the name (`PRIVATE` in
// `*_ALLOW_PRIVATE`, `AUTH` in `*_USE_OAUTH`), so the structurally identical
// ones ride the namespace's reported default.
//
// Each row is keyed where it can be on the constant that OWNS the name, so
// the key cannot drift from the reader; where the owner sits in a package
// this one must not depend on, the literal is pinned to its constant by
// pkg/runtime's TestTheEngineGuardLiftRowsNameTheirOwnConstants, which is
// the one package that imports every owner.
//
// What makes a name a member is measurable: the guard it lifts refuses a
// route or a credential, and the refusal is read through
// [envtrust.Inherited] or os.Getenv — an image's `ENV` is an inherited
// grant, since nothing marks it planted. The members that lift an
// ISOLATION, a LOADING or a browser-facing guard are not here: they are
// ARB-121's documented residue, named in docs/sandbox.md.
var engineEnvNeutralisedNames = map[string]string{
	envtrust.DotenvEndpointsOptIn: "the operator's opt-in that lets a project `.env` plant the routing and execution classes (ARB-122): granted from an image, the repository's own `.env` plants the model endpoints, the proxies and the loader's preload into every iterion the engine starts there",
	envtrust.EnvDotEnvForce:       "makes an engine child read the workspace `.env` at all (#2844): the file is the repository's, and the names nobody set on purpose are the provider endpoints a credential is spent against first among them",

	"ITERION_MCP_EXPAND_UNTRUSTED_ENV":   "opts a workflow-controlled MCP config back into expanding `${VAR}` against the launcher's environment, which holds the operator's credentials (pkg/backend/mcp's EnvExpandUntrustedEnv)",
	"ITERION_MCP_SERVER_ENV_PASSTHROUGH": "names which of the launcher's variables a stdio MCP server child receives — `all` for the whole environment, planted names included (pkg/backend/mcp's EnvServerEnvPassthrough)",
	"ITERION_CLOUD_ENV_PASSTHROUGH":      "names the credential-shaped variables workflow text may read from a cloud process's environment, which holds the platform's credentials rather than the tenant's (pkg/platformcfg's EnvCloudEnvPassthrough)",
	"ITERION_CONNECTOR_PROJECT_CATALOG":  "grants the connector catalog's project tier, which outranks the operator's own: a repository-shipped `connectors/<id>/` package then serves a consented connection id, and nothing pins what the operation does, so the package spends the operator's credential on a call the consent never granted (pkg/connection's ProjectCatalogEnv, read through plain os.Getenv — an inherited value is the grant)",
	"ITERION_SANDBOX_IMAGE_ENV_ALLOW":    "this screen's own escape hatch: it names the variables an image contributes untouched, so a baked value is an image allowlisting itself out of the neutralisation (pkg/runtime's EnvSandboxImageEnvAllow)",
}

// engineEnvNeutralisedFamilies are the sub-namespaces of `ITERION_` whose
// EVERY member chooses a credential or a route, with the reason. They say
// what the shared shape cannot: a family member that carries no credential
// word and names no host — the Sentinel master a connection fails over to,
// the account a mail credential authenticates as, the order the platform
// tries its credential pairs in — still chooses the one or the other.
//
// A family rather than its members, because the next member lands
// classified: `ITERION_PLATFORM_*` is the platform's credential/routing
// policy whatever knob is added to it. The namespaces are the operator's
// own infra families (internal/envtrust's operatorInfraPrefixes names the
// same ones for the bot-var gate), never a namespace invented here —
// pkg/sandbox/devcontainer's image_env_engine_test.go holds that, and holds
// each family to deciding something the shape does not already decide.
var engineEnvNeutralisedFamilies = map[string]string{
	"ITERION_PLATFORM_": "the platform credential/routing policy: which credential serves a route, the order the pairs are tried in, what a refused pinned key falls back to and whether routing is strict (pkg/llmroute, pkg/platformcfg's keys-first and facade defaults)",
	"ITERION_OIDC_":     "the identity chain the server mints its own sessions from: the issuer a token is fetched at, the client identity presented there, the scopes asked for, and whether a provider answers at all",
	"ITERION_SMTP_":     "the mail route and the credential presented on it: the host and port dialled, the STARTTLS posture of that dial, and the account it authenticates as",
	"ITERION_REDIS_":    "the cache route and its credential: the URL, the Sentinel peers and the master a connection fails over to, and the passwords presented there",
}

// engineEnvReportedNames are the engine names the shared shape class reads
// as a credential or a route while the name chooses NEITHER — its reach,
// documented name by name, because ARB-121 keeps everything outside the
// credential/routing sub-class reported.
//
// Each row says which spelling the shape read and what the name actually
// decides. A row whose name the shape does not catch — and which no
// neutralised family covers either — would be a reported-only answer the
// namespace's own default already gives: dead code, refused by
// image_env_engine_test.go.
var engineEnvReportedNames = map[string]string{
	"ITERION_SECURITY_HEADERS":          "the browser-facing response headers of the server's own pages (CSP, HSTS, frame options): the HEADER spelling is the shape's reach, and the name names no host and carries no credential",
	"ITERION_PAT_MAX_TTL":               "a ceiling on the lifetime of the tokens the server mints: the PAT segment is the shape's reach, and a ceiling chooses no credential",
	"ITERION_CLOUD_MAX_TOKENS":          "a spend ceiling counted in model tokens: the TOKEN spelling is the shape's reach, and the value is a bound, not a token",
	"ITERION_WEBHOOK_MAX_ATTEMPTS":      "how many times a webhook delivery is retried: the WEBHOOK segment is the shape's reach — a webhook URL carries its own bearer, this bound carries none",
	"ITERION_WEBHOOK_SYNC_DEBOUNCE":     "the debounce window of the webhook-driven sync: the same reach, the same answer",
	"ITERION_OIDC_GENERIC_DISPLAY_NAME": "the label the login page shows for the generic provider: it names neither the issuer nor the credential presented there, so the ITERION_OIDC_ family's reason does not hold for it",

	// The studio assistant's write bounds, whose AUTHORING segment carries
	// the AUTH spelling the shape reads. They bound what untrusted input
	// may cost; none of them chooses a credential or a route.
	"ITERION_ASSISTANT_AUTHORING_MAX_BLOCK_BYTES":           engineEnvAuthoringBound,
	"ITERION_ASSISTANT_AUTHORING_MAX_FILES":                 engineEnvAuthoringBound,
	"ITERION_ASSISTANT_AUTHORING_MAX_REPLACEMENTS":          engineEnvAuthoringBound,
	"ITERION_ASSISTANT_AUTHORING_MAX_REPLACEMENTS_PER_FILE": engineEnvAuthoringBound,
	"ITERION_ASSISTANT_AUTHORING_MAX_TOTAL_BYTES":           engineEnvAuthoringBound,
}

// engineEnvAuthoringBound is the five authoring bounds' shared reason: one
// text for one answer, so a reader never wonders whether two of them differ
// by anything but their unit.
const engineEnvAuthoringBound = "a bound on what the studio assistant may write in one authoring call: the AUTHORING segment carries the AUTH spelling the shape reads, and a bound chooses neither a credential nor a route"
