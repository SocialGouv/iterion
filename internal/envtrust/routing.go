package envtrust

import "strings"

// DotenvEndpointsOptIn is the operator's opt-in that lets a project `.env`
// plant routing-shaped names (RoutingShapedName): export it as
// `ITERION_DOTENV_ENDPOINTS=allow` in the environment iterion runs with.
// Anything else — unset, empty, another word — keeps the refusal;
// surrounding whitespace is ignored.
//
// It is read through Inherited, never os.Getenv, so a `.env` line naming it
// cannot vote for itself: the loader marks what it plants, and a planted
// value reads as nothing said here, whatever order the file put its lines
// in. It is refused as a bot variable (pkg/platformcfg), so a stored record
// cannot widen what the repositories a bot reviews may plant either.
const DotenvEndpointsOptIn = "ITERION_DOTENV_ENDPOINTS"

// DotenvEndpointsAllowed reports whether the environment this process
// INHERITED grants DotenvEndpointsOptIn. A planted value is no grant: the
// file that plants the opt-in plants routes beside it, and the operator —
// not the nearest repository — decides what a `.env` may name.
func DotenvEndpointsAllowed() bool {
	return strings.TrimSpace(Inherited(DotenvEndpointsOptIn)) == "allow"
}

// routingSuffixes are the case-insensitive NAME SUFFIXES that put a variable
// in the routing class: a name of one of these shapes decides where a
// connection is dialled — a model call, an outbound HTTP request, a metadata
// or credential lookup, a container or module fetch. Enumerated from the
// readers in this tree (first-party greps and the vendored SDKs), not from
// one reviewer's list:
//
//   - `_BASE_URL` — the model-call hosts: OPENAI_BASE_URL, ANTHROPIC_BASE_URL,
//     XAI_BASE_URL and MOONSHOT_BASE_URL (the vendored claw provider packages
//     and this engine's own tables), OPENAI_COMPATIBLE_BASE_URL
//     (pkg/backend/compatgw), DASHSCOPE_BASE_URL and the ANTHROPIC_*_BASE_URL
//     wire variants (vendored claw runtime and providers), and
//     CLAUDE_CODE_API_BASE_URL (vendored claw). cmd/iterion's own endpoint
//     refusals (credentialsPresentedAt) are a subset of this shape.
//   - `_API_BASE` — the same question under OpenAI's legacy name:
//     OPENAI_API_BASE (pkg/sandbox/devcontainer's deny class).
//   - `_ENDPOINT` — AZURE_OPENAI_ENDPOINT (the vendored foundry provider,
//     which reads it with the operator's key beside it), the OpenTelemetry
//     exporters' OTEL_EXPORTER_OTLP_ENDPOINT and its _TRACES/_LOGS splits
//     (vendored, and pkg/cli/run_telemetry's ITERION_OTLP_GRPC_ENDPOINT),
//     ITERION_S3_ENDPOINT (pkg/config), SEARXNG_ENDPOINT
//     (pkg/backend/tool), AWS_EC2_METADATA_SERVICE_ENDPOINT (vendored
//     aws-sdk-go-v2), the Azure instance-identity names MSI_ENDPOINT,
//     IDENTITY_ENDPOINT and IMDS_ENDPOINT (vendored azidentity), and the
//     engine's own ITERION_SANDBOX_PROXY_ENDPOINT (pkg/backend/compatgw) —
//     the one name only the engine may set, so a `.env` naming it would
//     forge the engine's voice on the gateway's proxy.
//   - `_HOST` — DOCKER_HOST (pkg/sandbox/devcontainer), KUBERNETES_SERVICE_HOST
//     (pkg/sandbox/factory, and the vendored mongo driver's Kerberos),
//     GCE_METADATA_HOST (vendored cloud.google.com), ITERION_SMTP_HOST
//     (cmd/iterion/server), GH_HOST (pkg/dispatcher/tracker), and the Azure
//     authority hosts the ARB names as their own shape — AZURE_AUTHORITY_HOST
//     and AZURE_POD_IDENTITY_AUTHORITY_HOST (vendored azidentity) — which
//     this suffix already answers for.
//   - `_PROXY` — the outbound proxies: HTTP_PROXY, HTTPS_PROXY, NO_PROXY and
//     their lowercase forms (vendored claw's apikit, and Go's own
//     net/http for every request this process or a child sends), ALL_PROXY
//     and its lowercase (vendored gorilla/websocket). One suffix covers the
//     four names the doctrine lists, both cases, plus any future `*_PROXY`.
//
// The shape, not a list of names: the next SDK's endpoint is covered the day
// it is read, not the day someone remembers to add it — the same rule the
// model-path class guard (pkg/backend/model hostShapedName) states for the
// narrower set it walks.
var routingSuffixes = []string{
	"_BASE_URL",
	"_API_BASE",
	"_ENDPOINT",
	"_HOST",
	"_PROXY",
}

// RoutingShapedName reports whether a variable's NAME puts it in the routing
// class — the names cmd/iterion's dotenv loader refuses to plant by default
// (ARB-122: the loader is the one site that knows it is planting, so it is
// the one that closes the class; the per-name credential tables are the belt
// that stays at the reads).
//
// The match is case-insensitive: on Windows the case is not even meaningful
// (CanonicalName), and a lowercase spelling of a routing name asks the same
// question as its uppercase. A name must carry more than the suffix —
// `_HOST` alone names nothing.
func RoutingShapedName(name string) bool {
	upper := strings.ToUpper(name)
	for _, suffix := range routingSuffixes {
		if len(upper) > len(suffix) && strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}
