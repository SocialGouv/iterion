package envtrust

import (
	"fmt"
	"strings"
)

// DotenvEndpointsOptIn is the operator's opt-in that lets a project `.env`
// plant the opt-in-able classes — routing-shaped names (RoutingShapedName)
// and execution-grade names (ExecutionGradeName): export it as
// `ITERION_DOTENV_ENDPOINTS=allow` in the environment iterion runs with.
// Anything else — unset, empty, another word — keeps the refusal;
// surrounding whitespace is ignored.
//
// It is read through Inherited, never os.Getenv, so a `.env` line naming it
// cannot vote for itself: the loader marks what it plants, and a planted
// value reads as nothing said here, whatever order the file put its lines
// in. It is refused as a bot variable (pkg/platformcfg), so a stored record
// cannot widen what the repositories a bot reviews may plant either. On the
// studio lane it never crosses into a run's environment at all
// (pkg/projectenv): the runs launched there answer their own opt-in where
// the run's iterion itself is launched, not in a file the workspace holds.
const DotenvEndpointsOptIn = "ITERION_DOTENV_ENDPOINTS"

// DotenvEndpointsAllowed reports whether the environment this process
// INHERITED grants DotenvEndpointsOptIn. A planted value is no grant: the
// file that plants the opt-in plants routes beside it, and the operator —
// not the nearest repository — decides what a `.env` may name.
func DotenvEndpointsAllowed() bool {
	return strings.TrimSpace(Inherited(DotenvEndpointsOptIn)) == "allow"
}

// DotenvPlantRefused answers the ONE question every loader that plants a
// dotenv file asks about a name it is about to set: the CLI's applyDotEnv,
// the studio's projectenv.Snapshot and the desktop's env-file loader. Three
// classes close at the plant, one entry point, so the answer cannot drift
// between the lanes. A ROUTING-SHAPED name and an EXECUTION-GRADE name
// (ExecutionGradeName — the generic host-env families whose plant makes a
// child of iterion run or trust what the file chose) are not applied unless
// the environment this process INHERITED granted the opt-in — whatever
// namespace it lives in, the operator's explicit grant answers it, as it
// has since the routing class closed (a deployment wire the operator
// exported a grant for keeps working). The two share the branch so a name
// of both classes reads one answer; the refusal line names the routing
// reason first, so a name in the overlap is once refused, once explained.
// Every OTHER operator-grade name (OperatorGradeRefused — the same
// inventory the bot-var gate asks, widened to the server's credential
// spellings) is closed OUTRIGHT: the sandbox lifts, the auth and origin
// guards, the server's secrets are the operator's posture, and nothing a
// file may say — the opt-in included — lets the file set them.
func DotenvPlantRefused(name string) bool {
	if RoutingShapedName(name) || ExecutionGradeName(name) {
		return !DotenvEndpointsAllowed()
	}
	return OperatorGradeRefused(name)
}

// RoutingRefusalLine renders the single-line refusal for a name a dotenv
// file plants that the opt-in-able classes answer: the name, the file it
// came from, why it is not applied, the credential it would have
// redirected (when the operator's own is inherited beside it) and the
// operator's way past the refusal. One message for every loader that
// plants — the CLI's, the studio snapshot's, the desktop's — so a refusal
// reads the same wherever a file tried to plant. A routing-shaped name
// reads the routing reason ("where a connection goes"); an execution-grade
// name outside the routing shapes reads the execution reason ("what
// iterion's children execute or trust"); the union never duplicates — a
// name of both classes reads the routing line alone. source names the kind
// of file ("the project `.env`", "the dotenv file") and path its location;
// both are said because the next reader needs to find the line to delete
// or the name to export. The caller puts the line on stderr directly: a
// refusal is not a log level, and the level a logger honours is itself
// something the file may have planted a line earlier.
func RoutingRefusalLine(name, source, path string) string {
	credHalf := ""
	if redirected := InheritedCredentialsAt(name); len(redirected) > 0 {
		credHalf = ": " + strings.Join(redirected, " / ") + " is the operator's own and would be presented at the host it names"
	}
	// The grade names that no opt-in answers get the other half of the
	// question: their opt-in half would lie, because ITERION_DOTENV_
	// ENDPOINTS widens what a file may plant as a ROUTE or a host-env
	// execution name and answers nothing for the operator's own posture.
	// What remains is the operator's channel: the environment iterion runs
	// with.
	tail := fmt.Sprintf(" — export %s in the environment iterion runs with to answer it yourself, or export %s=allow to let this file plant its routes", name, DotenvEndpointsOptIn)
	if !RoutingShapedName(name) && ExecutionGradeName(name) {
		tail = fmt.Sprintf(" — export %s in the environment iterion runs with to answer it yourself, or export %s=allow to let this file plant it", name, DotenvEndpointsOptIn)
	}
	if !RoutingShapedName(name) && OperatorGradeRefused(name) {
		tail = fmt.Sprintf(" — export %s in the environment iterion runs with if you mean it; the file a project holds does not set the operator's posture", name)
	}
	reason := "a dotenv file does not choose where a connection goes"
	if !RoutingShapedName(name) && ExecutionGradeName(name) {
		reason = "a dotenv file does not choose what iterion's children execute or trust"
	}
	if !RoutingShapedName(name) && OperatorGradeRefused(name) {
		reason = "a dotenv file does not speak for the operator"
	}
	return fmt.Sprintf("%s comes from %s %s, so it is not applied: %s%s%s",
		name, source, path, reason, credHalf, tail)
}

// gitTransportPrefixes are the git transport names the git BINARY reads
// itself: GIT_SSH and GIT_SSH_COMMAND name the program that carries
// ssh:// and git:// transport; GIT_PROXY_COMMAND the program that carries
// every proxied connection; GIT_CONFIG_COUNT / GIT_CONFIG_KEY_n /
// GIT_CONFIG_VALUE_n and GIT_CONFIG_PARAMETERS inject config — which is
// where core.sshCommand and friends hide. The reader is not a Go
// os.Getenv (the walk that derives the routing class sees none) but every
// clone, fetch and push pkg/git runs: the H-10 probe measured a planted
// GIT_SSH_COMMAND EXECUTED by the git binary before it even dialled. The
// family is refused at the plant like the rest of the class, and a planted
// name of it is additionally dropped at pkg/git's own env assembly.
var gitTransportPrefixes = []string{
	"GIT_SSH",
	"GIT_PROXY_COMMAND",
	"GIT_CONFIG_",
}

// GitTransportName reports whether name is one of the git transport names
// the git binary itself reads (gitTransportPrefixes). pkg/git consults it
// beside Planted to keep a project `.env`'s transport out of the engine's
// own git gestures.
func GitTransportName(name string) bool {
	upper := strings.ToUpper(name)
	for _, prefix := range gitTransportPrefixes {
		if upper == prefix || strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

// routingPrefixes are the case-insensitive NAME PREFIXES that put a variable
// in the routing class — a family the service suffix composes per service,
// so no suffix can answer for it:
//
//   - `AWS_ENDPOINT_URL` — the vendored aws-sdk-go-v2's endpoint overrides.
//     AWS_ENDPOINT_URL itself is the global override every service's
//     endpoints.go reads (and the `_URL` suffix would answer for it alone);
//     AWS_ENDPOINT_URL_BEDROCK_RUNTIME, AWS_ENDPOINT_URL_STS and their kin
//     are the per-service spellings, which the SDK composes from the service
//     ID at run time (config/env_config.go builds the name with
//     fmt.Sprintf("%s_%s", ...) and each service's endpoints.go reads its
//     own literal). The bedrock runtime dials the planted host with the
//     operator's AWS identity — Authorization header and session token — so
//     the prefix answers for the whole family, the next service included.
//
//   - the git transport family (gitTransportPrefixes) — the programs and
//     config the git binary reads for every network gesture, spelled above.
var routingPrefixes = []string{
	"AWS_ENDPOINT_URL",
	"GIT_CONFIG_",
	"GIT_PROXY_COMMAND",
	"GIT_SSH",
}

// routingSuffixes are the case-insensitive NAME SUFFIXES that put a variable
// in the routing class: a name of one of these shapes decides where a
// connection is dialled, what is presented or sent there, or which hosts are
// trusted — a model call, an outbound HTTP request, a metadata or credential
// lookup, a container or module fetch, an error report. Enumerated from the
// readers in this tree (first-party greps and the vendored SDKs), not from
// one reviewer's list; the completeness guard
// (routing_completeness_test.go) re-walks the same sources, so a reader
// that outgrows this inventory arrives red rather than planting silently.
//
//   - `_URL` — where a wire goes, the tree's most common spelling. The
//     model-call hosts (OPENAI_BASE_URL, ANTHROPIC_BASE_URL, XAI_BASE_URL,
//     MOONSHOT_BASE_URL — the vendored claw provider packages and this
//     engine's own tables — OPENAI_COMPATIBLE_BASE_URL of
//     pkg/backend/compatgw, DASHSCOPE_BASE_URL, the ANTHROPIC_*_BASE_URL
//     wire variants and CLAUDE_CODE_API_BASE_URL, all vendored claw — the
//     `_BASE_URL` shape this suffix answers for together with the rest);
//     the OAuth forfait wires, ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL
//     being the URL the stored Claude subscription's refresh token is
//     POSTed to (pkg/secrets reads them through envOr: oauth_refresh.go,
//     oauth_authcode.go, oauth_account.go, plus the Codex token URL);
//     the deployment's own wires ITERION_REMOTE_URL (pkg/cli/remote.go,
//     read with ITERION_REMOTE_TOKEN beside it), ITERION_NATS_URL,
//     ITERION_REDIS_URL and ITERION_ALERTS_WEBHOOK_URL (pkg/config/env.go),
//     ITERION_QUEUE_NATS_URL and ITERION_SERVER_URL
//     (cmd/iterion/scan_shards.go), ITERION_ALERTS_BASE_URL
//     (pkg/cli/studio.go) and ITERION_PUBLIC_URL (pkg/deeplink — the origin
//     deep links are built on); the search backends SEARXNG_URL (read FIRST,
//     before its classed `_ENDPOINT` sibling, by the vendored claw
//     web_search.go and by pkg/backend/tool), CLAW_WEB_SEARCH_BRAVE_URL and
//     CLAW_WEB_SEARCH_DDG_URL (vendored claw); the wires that fetch
//     iterion's own data, ITERION_OIDC_GENERIC_ISSUER_URL
//     (pkg/config/env.go), ITERION_UPDATE_MANIFEST_URL
//     (cmd/iterion-desktop/updater.go) and ITERION_MODEL_SPECS_URL
//     (pkg/backend/modelspecs — where the price table is fetched from).
//   - `_URI` — the same question under a second spelling: ITERION_MONGO_URI
//     (pkg/config/env.go), and the vendored aws-sdk-go-v2's container
//     credential sources AWS_CONTAINER_CREDENTIALS_FULL_URI and
//     AWS_CONTAINER_CREDENTIALS_RELATIVE_URI (config/env_config.go — where
//     the SDK fetches credentials from).
//   - `_ADDRS` — a host list dialled as one: ITERION_REDIS_SENTINEL_ADDRS
//     (pkg/config/env.go), the Sentinel peers a cache connection fails over
//     across.
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
//   - `_HOSTS` — a list that decides which hosts are trusted or left
//     unguarded: ITERION_SANDBOX_MODEL_HOSTS (pkg/runtime/sandbox.go — the
//     hosts whose request bodies the inspecting proxy leaves in placeholder
//     form, so a planted list is a host the run's prompts and code cross in
//     the clear) and ITERION_WEBHOOK_FORGE_HOSTS
//     (pkg/server/webhooks_gitlab.go — the hosts allowed to forge webhooks,
//     where a planted entry admits the forger the list exists to stop).
//   - `_PROXY` — the outbound proxies: HTTP_PROXY, HTTPS_PROXY, NO_PROXY and
//     their lowercase forms (vendored claw's apikit, and Go's own
//     net/http for every request this process or a child sends), ALL_PROXY
//     and its lowercase (vendored gorilla/websocket).
//   - `_DSN` — where error reports are delivered: SENTRY_DSN (vendored
//     sentry-go reads it itself, pkg/errtrack boots on it) — a crash
//     envelope carries the operator's stack, paths and machine context, so
//     a planted DSN delivers them at a host the file chose. This closes the
//     #498 review note that a project `.env` could aim errtrack: a project
//     that legitimately reports to its own DSN now asks the opt-in like any
//     other wire.
//
// Examined and excluded, with the reason: DBUS_SESSION_BUS_ADDRESS
// (vendored godbus — the desktop's session-bus discovery, dialled by the
// GUI stack of the process the operator launched, which reads its own
// operator-owned env files and never a project `.env`; nothing a project
// `.env` reaches dials it); GOPROXY (no production reader in the tree);
// GIT_ASKPASS (a program git runs for credentials, but only when a
// transport needs an interactive answer — every engine gesture
// authenticates out of band or fails rather than prompting, and the
// transport family above already closes the gestures the reviewer
// measured); GIT_EDITOR (git runs it on an interactive commit — the
// engine's commits always pass -m) — and the routing-shaped names with no
// reader at all — SSH_AUTH_SOCK,
// ANTHROPIC_UNIX_SOCKET, DATABASE_URL, ITERION_MONGO_URL,
// ITERION_SLACK_WEBHOOK_URL, SESSION_INGRESS_URL, AGENT_PROXY_URL,
// FIRECRAWL_API_URL, GITHUB_API_URL — grepped for across pkg/, internal/,
// cmd/ and vendor/ in the read forms the tree uses (os.Getenv,
// os.LookupEnv, envOr, lookupString, lookup, readEnv, envOperator) and
// found only in lists and comments. GIT_PROXY_COMMAND is NOT among them
// any more: the H-10 review measured its reader — the git BINARY, run as a
// subprocess with the process env, which the Go-form grep cannot see — so
// it and GIT_SSH and GIT_CONFIG_ are in the class above. The AWS booleans
// that end in `_ENDPOINT`
// (AWS_USE_FIPS_ENDPOINT, AWS_USE_DUALSTACK_ENDPOINT) are the class's
// documented over-refusal price: they pick which VARIANT of Amazon's own
// endpoints answers, the refusal is loud, and the opt-in answers it.
//
// The shape, not a list of names: the next SDK's endpoint is covered the day
// it is read, not the day someone remembers to add it — the same rule the
// model-path class guard (pkg/backend/model hostShapedName) states for the
// narrower set it walks.
var routingSuffixes = []string{
	"_ADDRS",
	"_API_BASE",
	"_DSN",
	"_ENDPOINT",
	"_HOST",
	"_HOSTS",
	"_PROXY",
	"_URI",
	"_URL",
}

// RoutingShapedName reports whether a variable's NAME puts it in the routing
// class — the names every dotenv loader refuses to plant by default
// (ARB-122: the loader is the site that knows it is planting, so the class
// closes at the plant; the per-name credential tables are the belt that
// stays at the reads). The match is case-insensitive: on Windows the case is
// not even meaningful (CanonicalName), and a lowercase spelling of a routing
// name asks the same question as its uppercase. A name must carry more than
// the shape it matches — `_HOST` alone names nothing.
func RoutingShapedName(name string) bool {
	upper := strings.ToUpper(name)
	for _, prefix := range routingPrefixes {
		// The prefix matches its own exact spelling too: GIT_SSH alone is
		// the legacy transport-program name, and AWS_ENDPOINT_URL answers
		// through this list as well as through the _URL suffix.
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	for _, suffix := range routingSuffixes {
		if len(upper) > len(suffix) && strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}
