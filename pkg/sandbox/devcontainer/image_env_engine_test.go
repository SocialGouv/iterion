package devcontainer

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// engineDispositionCase is one engine name, what the screen must answer for
// it when an IMAGE bakes it in, and which form of the refinement answers.
type engineDispositionCase struct {
	name string
	want imageEnvDisposition
	why  string
}

// engineDispositionCases are the witnesses of the engine namespace's
// partition — one per form of [engineImageEnvDisposition] and per side of
// the doctrine, driven through the exported screen the runtime calls.
var engineDispositionCases = []engineDispositionCase{
	// The shared shape class, which is the half that answers most of the
	// namespace: a switch that lifts the guard on reaching a PRIVATE host,
	// a switch that chooses WHICH credential authenticates a model call,
	// the stores' and wires' URLs, the credentials themselves.
	{"ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", neutralizeInImage, "lifts the guard that keeps a model endpoint off a private host"},
	{"ITERION_OPENAI_USE_OAUTH", neutralizeInImage, "chooses the subscription credential over the operator's key"},
	{"ITERION_FORBID_SUBSCRIPTION_OAUTH", neutralizeInImage, "the same choice, the other way"},
	{"ITERION_RUNNER_CLONE_ALLOW_PRIVATE", neutralizeInImage, "lifts the private-host guard on the runner's clone"},
	{"ITERION_MONGO_URI", neutralizeInImage, "where the store is dialled, credential inside the URI"},
	{"ITERION_REMOTE_URL", neutralizeInImage, "where the CLI's remote instance is reached"},
	{"ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL", neutralizeInImage, "where the stored subscription's refresh token is POSTed"},
	{"ITERION_SECRETS_KEY", neutralizeInImage, "the key the secret store is sealed with"},
	{"ITERION_SECRETS_REDACT", neutralizeInImage, "the guard that keeps the operator's secrets out of a model call"},
	{"ITERION_SANDBOX_MODEL_HOSTS", neutralizeInImage, "the hosts whose request bodies cross the inspecting proxy in the clear"},

	// The exact NEUTRALISED rows: a switch that lifts a guard on a route or
	// on a credential while carrying no credential word and naming no host,
	// which is the half of the shape's imprecision a reported default
	// swallows in silence.
	{"ITERION_DOTENV_ENDPOINTS", neutralizeInImage, "grants a project `.env` the whole routing class (ARB-122)"},
	{"ITERION_DOTENV_FORCE", neutralizeInImage, "makes an engine child read the workspace `.env` at all (#2844)"},
	{"ITERION_MCP_EXPAND_UNTRUSTED_ENV", neutralizeInImage, "hands a workflow-controlled MCP config the launcher's environment"},
	{"ITERION_MCP_SERVER_ENV_PASSTHROUGH", neutralizeInImage, "hands a stdio MCP server child the launcher's variables, `all` for every one of them"},
	{"ITERION_CLOUD_ENV_PASSTHROUGH", neutralizeInImage, "lets workflow text read the platform's credential-shaped variables"},
	{"ITERION_SANDBOX_IMAGE_ENV_ALLOW", neutralizeInImage, "this screen's own escape hatch: an image allowlisting itself"},

	// The FAMILY rows: a member that carries no credential word and names
	// no host, inside a sub-namespace whose every member chooses a
	// credential or a route.
	{"ITERION_PLATFORM_FACADE_DEFAULT", neutralizeInImage, "which credential facade serves a route by default"},
	{"ITERION_PLATFORM_PAIR_ORDER", neutralizeInImage, "the order the platform tries its credential pairs in"},
	{"ITERION_SMTP_USERNAME", neutralizeInImage, "the account the mail credential authenticates as"},
	{"ITERION_REDIS_MASTER_NAME", neutralizeInImage, "the Sentinel master a cache connection fails over to"},
	{"ITERION_OIDC_GENERIC_CLIENT_ID", neutralizeInImage, "the client identity presented at the issuer"},

	// The REPORTED side. The three names iterion's own published images
	// bake, which is the measurement that keeps the namespace off the
	// neutralised side wholesale; the shape's reach, where a credential
	// word sits in a name that chooses nothing; and the namespace's own
	// default, which is the doctrine's residue — a knob that chooses
	// neither a credential nor a route keeps the image's value.
	{"ITERION_VERSION", reportedOnly, "baked by iterion's own image"},
	{"ITERION_COMMIT", reportedOnly, "baked by iterion's own image"},
	{"ITERION_BOTS_PATH", reportedOnly, "baked by iterion's own image and its runner's"},
	{"ITERION_CLOUD_MAX_TOKENS", reportedOnly, "a spend bound, not a token"},
	{"ITERION_ASSISTANT_AUTHORING_MAX_FILES", reportedOnly, "a write bound whose AUTHORING segment carries the AUTH spelling"},
	{"ITERION_OIDC_GENERIC_DISPLAY_NAME", reportedOnly, "a login-page label inside a neutralised family"},
	{"ITERION_PERMISSION", reportedOnly, "the permission gate's mode: the documented residue, not a credential or a route"},
	{"ITERION_LOG_LEVEL", reportedOnly, "the process's log level"},

	// The suffix family still answers BEFORE the namespace, refinement or
	// not: an engine name ending in a credential suffix is neutralised by
	// the suffix, which is the precedence this class promises.
	{"ITERION_REVIEW_BEARER_TOKEN", neutralizeInImage, "the `_TOKEN` suffix, ahead of the namespace"},
}

// TestTheEngineNamespaceAnswersItsSubClasses drives the witnesses above
// through [ScreenImageEnvName] — the seam pkg/runtime's screen calls — so
// each is a claim about the production answer and not about a table.
//
// The plant gate is asserted beside it: refining the namespace must not
// un-deny a name, or a repository's devcontainer.json could plant what the
// image screen was taught to empty.
func TestTheEngineNamespaceAnswersItsSubClasses(t *testing.T) {
	for _, c := range engineDispositionCases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.HasPrefix(c.name, "ITERION_") {
				t.Fatalf("%s is not of the engine namespace: the row proves nothing about the refinement", c.name)
			}
			denied, neutralize := ScreenImageEnvName(c.name)
			wantNeutralize := c.want == neutralizeInImage
			if !denied || neutralize != wantNeutralize {
				verb := "reports it and keeps the image's value"
				if wantNeutralize {
					verb = "overrides it with an empty value"
				}
				t.Errorf("ScreenImageEnvName(%q) = (denied %v, neutralize %v): an image baking it must be one the screen %s — %s",
					c.name, denied, neutralize, verb, c.why)
			}
			if !DeniedEnvKey(c.name) {
				t.Errorf("DeniedEnvKey(%q) = false: the engine namespace's refinement answers the IMAGE question, and must leave the plant gate's refusal untouched", c.name)
			}
		})
	}
}

// TestTheEngineNamespaceAnswersTheSameInAnyCase holds the class's
// case rule at the refinement: [screenEnvName] upper-cases before it asks,
// and [EngineImageEnvReason] upper-cases what it is asked, so no spelling
// of an engine name escapes its disposition or loses its reason.
func TestTheEngineNamespaceAnswersTheSameInAnyCase(t *testing.T) {
	for _, c := range engineDispositionCases {
		for _, spelling := range []string{strings.ToLower(c.name), mixedCase(c.name)} {
			if got := screenEnvName(spelling); got != c.want {
				t.Errorf("screenEnvName(%q) = %v, want the %q row's %v — case is not a membership rule", spelling, got, c.name, c.want)
			}
		}
	}
	for _, table := range []map[string]string{engineEnvReportedNames, engineEnvNeutralisedNames} {
		for _, name := range slices.Sorted(maps.Keys(table)) {
			if got, want := EngineImageEnvReason(strings.ToLower(name)), table[name]; got != want {
				t.Errorf("EngineImageEnvReason(%q) = %q, want the row's own reason — the lookup must upper-case what it is asked", strings.ToLower(name), got)
			}
		}
	}
}

// TestTheEngineRefinementIsReachedByTheWalk: the rows answer nothing unless
// the namespace entry carries the refinement, so the hook itself is held
// here. Without it every member of the namespace reads the family's
// reported default, and the whole partition is dead code that looks like a
// guard.
//
// The row's own `image` is held through the PRODUCTION answer rather than by
// reading the field: an engine name no row and no shape clause catches is
// what the refinement hands back to the row, so the answer for one of those
// is the only reading that proves the fallback is the row's and not a
// literal buried in [engineImageEnvDisposition].
func TestTheEngineRefinementIsReachedByTheWalk(t *testing.T) {
	var found bool
	for _, guard := range deniedEnvNamespaces {
		if guard.pattern != "ITERION_" {
			continue
		}
		found = true
		if guard.refine == nil {
			t.Fatalf("the ITERION_ namespace carries no refinement: its credential/routing members read the family's %v", guard.image)
		}
		if got := guard.refine("ITERION_SOMETHING_ELSE"); got != notDenied {
			t.Errorf("the refinement answers %v for a name no row and no shape clause catches: it must say notDenied there, or the row's own disposition is never the fallback", got)
		}
	}
	if !found {
		t.Fatal("no ITERION_ namespace in the class: the engine's own names are not screened at all")
	}
	// The fallback, read where every caller reads it: an unrowed, unshaped
	// engine name is TOLD and keeps the image's value — the measurement the
	// reported default stands on, since iterion's own images bake three such
	// names.
	if denied, neutralize := ScreenImageEnvName("ITERION_SOMETHING_ELSE"); !denied || neutralize {
		t.Errorf("ScreenImageEnvName(ITERION_SOMETHING_ELSE) = (denied %v, neutralize %v), want (true, false): the namespace row's own disposition is the refinement's fallback", denied, neutralize)
	}
}

// TestEveryEngineRowIsLoadBearingAndCarriesItsReason is the dead-code guard
// of the three tables, both directions:
//
//   - a REPORTED row must be a name the shape (or a neutralised family)
//     would otherwise catch. A row for a name the namespace's default
//     already reports answers nothing — dead code that reads like a
//     decision;
//   - a NEUTRALISED exact row is the mirror: it must be a name neither the
//     shape nor a family already catches, or it decides nothing either —
//     and it must not also sit in the reported table, where the walk's
//     order would decide the answer instead of the tables;
//   - a neutralised FAMILY must decide something the shape does not: a
//     family whose members the shape already catches is dead too. The
//     probe is the family's own prefix, which is how the dotenv parity
//     guard asks the same question of a namespace;
//   - a family must be one of the operator's own infra namespaces
//     (internal/envtrust's inventory, asked through InfraGradeName), never
//     a namespace invented here: the engine's credential and route
//     families are already enumerated for the bot-var gate, and two
//     inventories of the same families drift;
//   - every row carries a reason, because the reason IS the classification
//     the sweep prints.
func TestEveryEngineRowIsLoadBearingAndCarriesItsReason(t *testing.T) {
	for _, name := range slices.Sorted(maps.Keys(engineEnvReportedNames)) {
		reason := engineEnvReportedNames[name]
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is reported with no reason: the row must say which spelling the shape read and what the name decides", name)
		}
		if !strings.HasPrefix(name, "ITERION_") {
			t.Errorf("%s is not of the engine namespace: the refinement never sees it", name)
		}
		if upper := strings.ToUpper(name); name != upper {
			t.Errorf("%s is never consulted — the lookup asks for %q; one entry answers for every spelling", name, upper)
		}
		shaped := envtrust.RoutingShapedName(name) || envtrust.CredentialShapedName(name)
		familied := false
		for prefix := range engineEnvNeutralisedFamilies {
			if strings.HasPrefix(name, prefix) {
				familied = true
			}
		}
		if !shaped && !familied {
			t.Errorf("%s is reported by a row of its own, and neither the shared shape class nor a neutralised family would have caught it: the namespace's default reports it already, so the row decides nothing", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(engineEnvNeutralisedNames)) {
		reason := engineEnvNeutralisedNames[name]
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is neutralised with no reason: the row must name the guard the switch lifts and whose route or credential it opens", name)
		}
		if !strings.HasPrefix(name, "ITERION_") {
			t.Errorf("%s is not of the engine namespace: the refinement never sees it", name)
		}
		if upper := strings.ToUpper(name); name != upper {
			t.Errorf("%s is never consulted — the lookup asks for %q; one entry answers for every spelling", name, upper)
		}
		if _, both := engineEnvReportedNames[name]; both {
			t.Errorf("%s is in both exact tables: the answer would come from the order engineImageEnvRow walks them, not from a decision", name)
		}
		if envtrust.RoutingShapedName(name) || envtrust.CredentialShapedName(name) {
			t.Errorf("%s is already neutralised by the shared shape class: the row neutralises nothing of its own", name)
		}
		for prefix := range engineEnvNeutralisedFamilies {
			if strings.HasPrefix(name, prefix) {
				t.Errorf("%s is already neutralised by the %s family row: the exact row decides nothing", name, prefix)
			}
		}
	}
	for _, prefix := range slices.Sorted(maps.Keys(engineEnvNeutralisedFamilies)) {
		reason := engineEnvNeutralisedFamilies[prefix]
		if strings.TrimSpace(reason) == "" {
			t.Errorf("the %s family neutralises with no reason: the row must say what every member of it chooses", prefix)
		}
		if !strings.HasPrefix(prefix, "ITERION_") || !strings.HasSuffix(prefix, "_") {
			t.Errorf("%q is not an engine sub-namespace: a family row is a `ITERION_`-prefixed namespace ending in `_`", prefix)
		}
		probe := prefix + "PROBE"
		if envtrust.RoutingShapedName(probe) || envtrust.CredentialShapedName(probe) {
			t.Errorf("the %s family is already answered by the shared shape class (%s reads as a credential or a route): the row neutralises nothing of its own", prefix, probe)
		}
		if !envtrust.InfraGradeName(probe) {
			t.Errorf("the %s family is not one of the operator's infra namespaces (internal/envtrust): a credential or route family of the engine belongs to that inventory first, or it is a namespace invented here", prefix)
		}
		if got := screenEnvName(probe); got != neutralizeInImage {
			t.Errorf("screenEnvName(%q) = %v: the %s family does not reach the walk", probe, got, prefix)
		}
		for _, other := range slices.Sorted(maps.Keys(engineEnvNeutralisedFamilies)) {
			if other != prefix && strings.HasPrefix(prefix, other) {
				t.Errorf("the %s family is nested in %s: the disposition would be the same either way, but the REASON a reader is given would depend on which row the range hit first — one family per sub-namespace", prefix, other)
			}
		}
	}
}

// TestTheEngineRefinementNeutralisesNoNameOurImagesBake is the measurement
// the refinement's reported default stands on, held at the class rather
// than at the sweep: the three names iterion's own published images bake
// must keep their value. pkg/platformcfg reads them from the Dockerfiles —
// this is the fast guard beside the rows they constrain.
func TestTheEngineRefinementNeutralisesNoNameOurImagesBake(t *testing.T) {
	for _, name := range []string{"ITERION_VERSION", "ITERION_COMMIT", "ITERION_BOTS_PATH"} {
		denied, neutralize := ScreenImageEnvName(name)
		if !denied {
			t.Errorf("ScreenImageEnvName(%q) says the name is outside the class: our own images bake it, and the operator must still be told when another image does", name)
		}
		if neutralize {
			t.Errorf("ScreenImageEnvName(%q) = neutralise: iterion's own runner image bakes it, so the override would empty a variable our own Dockerfile sets", name)
		}
	}
}
