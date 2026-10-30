package devcontainer

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// devcontainerOnly lists the parts of this deny class that the dotenv
// plant gate (envtrust.DotenvPlantRefused) deliberately does NOT carry,
// with the reason the dotenv question differs. The dotenv gate adopts the
// execution portion of this table (envtrust.ExecutionGradeName — ARB-122
// doctrine A extended by the H-10 round-6 finding: the same children the
// container gate shields, one more planter), and its own routing shapes,
// operator grade and credential shape answer the rest. A name or namespace
// added to this table arrives HERE red until it is classified: answered by
// one of the dotenv classes, or exempted below with its reason.
var devcontainerOnly = map[string]string{
	// The provider credential and switch families: a provider credential
	// (AWS_ACCESS_KEY_ID, ANTHROPIC_API_KEY) in a `.env` next to the
	// project is what the file is FOR, and the operator's own export wins
	// over it; a cloud switch only bites through those credentials or a
	// route, and the routes are the routing class's.
	"ITERION_PROBE":         "the dotenv operator grade carries its own ITERION_ inventory, held by pkg/platformcfg's classification tests",
	"CLAUDE_PROBE":          "the agent's own switches and settings sources: they bite through a credential or a route this gate does not answer either (CLAUDE_CONFIG_DIR alone is execution-grade, spelled exact in envtrust)",
	"CLAWD_PROBE":           "claw-code-go's own namespace: the runner's payload knobs, not generic host env",
	"CLAW_PROBE":            "the runner's namespace, same reason",
	"HOOK_PROBE":            "the hooks' payload namespace: settings-hook input, not host env",
	"CCR_PROBE":             "the claude CLI's router namespace: provider routing through credentials the file may carry",
	"ANT_OTEL_PROBE":        "the telemetry twins: an OTEL destination is routing-shaped when shaped (_ENDPOINT), otherwise the operator's telemetry posture",
	"ANT_CLAUDE_CODE_PROBE": "the CLI's own telemetry namespace, same reason",
	"ANTHROPIC_PROBE":       "the provider's namespace: what the file is FOR",
	"OPENCODE_PROBE":        "the agent's own namespace: its config bits are HOME-shaped (covered) or provider switches",
	"AWS_PROBE":             "the AWS SDK chain: credentials the file exists to carry",
	"AZURE_PROBE":           "azidentity's chain: same reason",
	"GOOGLE_PROBE":          "Google ADC: same reason",
	"GCE_METADATA_PROBE":    "the metadata server family: rides the ADC credentials",
	"CLOUDSDK_PROBE":        "gcloud's credential dir: same reason",
	"VERTEX_REGION_PROBE":   "the per-model region switch: provider selection, not execution",
	"OTEL_PROBE":            "telemetry destinations: routing-shaped when shaped (_ENDPOINT), otherwise the operator's reporting posture",
	"SENTRY_PROBE":          "SENTRY_DSN is routing (_DSN); the namespace's remainder is the operator's reporting posture",
	"GH_PROBE":              "the forge CLIs' namespace: GH_HOST routes (_HOST), GH_TOKEN is credential-shaped, the config dir is HOME-shaped in spirit but the dotenv question leaves the namespace to this gate",
	"GLAB_PROBE":            "same reason as GH_",
	"GIT_PROBE":             "the transport family (GIT_SSH, GIT_PROXY_COMMAND, GIT_CONFIG_) is the routing class's; GIT_ASKPASS and GIT_EXEC_PATH are execution-grade by exact name; GIT_SSL_* are git's own TLS trust, left to this gate",
	// The exact names that are provider switches, regions or identity
	// chains rather than execution or trust: they bite through provider
	// credentials the file may carry, or route (the *_ENDPOINT/_URL ones
	// the routing shapes already answer).
	"BEDROCK_REGION":                 "provider region switch",
	"CLOUD_ML_REGION":                "provider region switch",
	"GCLOUD_PROJECT":                 "provider project switch",
	"gcloud_project":                 "provider project switch (the case-sensitive twin)",
	"MSAL_FORCE_REGION":              "Azure identity-chain region",
	"DEFAULT_IDENTITY_CLIENT_ID":     "Azure managed-identity chain, riding IDENTITY_ENDPOINT (_ENDPOINT, routed)",
	"IDENTITY_SERVER_THUMBPRINT":     "Azure managed-identity chain, riding IDENTITY_ENDPOINT",
	"ENABLE_BETA_TRACING_DETAILED":   "the CLI's telemetry switches: reporting posture",
	"ENABLE_ENHANCED_TELEMETRY_BETA": "the CLI's telemetry switches: reporting posture",
}

// TestEveryDeniedEnvKeyReachesADotenvPlantClassOrAnExemption holds the
// partition between this table and the dotenv plant classes: every exact
// name and every namespace of the deny class must be answered by one of
// envtrust's dotenv classes (routing shapes, operator grade, credential
// shape, execution grade) or sit in devcontainerOnly with its reason. A
// new entry lands red here before it can be a name only the container
// gate refuses.
func TestEveryDeniedEnvKeyReachesADotenvPlantClassOrAnExemption(t *testing.T) {
	answered := func(name string) bool {
		return envtrust.RoutingShapedName(name) ||
			envtrust.OperatorGradeRefused(name) ||
			envtrust.CredentialShapedName(name) ||
			envtrust.ExecutionGradeName(name)
	}
	for name := range deniedEnvNames {
		if answered(name) {
			continue
		}
		reason, exempt := devcontainerOnly[name]
		if !exempt {
			t.Errorf("%s is denied here but reaches no dotenv plant class and no exemption: classify it (envtrust) or exempt it with the reason the dotenv question differs", name)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is exempt with no reason: the exemption must say why the dotenv question differs", name)
		}
	}
	for _, prefix := range deniedEnvNamespaces {
		probe := strings.ToUpper(prefix[:len(prefix)-1]) + "_PROBE"
		if answered(probe) {
			continue
		}
		reason, exempt := devcontainerOnly[probe]
		if !exempt {
			t.Errorf("the %s namespace reaches no dotenv plant class and no exemption: classify it or exempt it with the reason", prefix)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("the %s namespace is exempt with no reason", prefix)
		}
	}
	// The execution portion the dotenv gate DID adopt keeps answering, both
	// directions: a name this table refuses for execution must be refused
	// at the dotenv plant too (the opt-in aside), and the shared witnesses
	// stay shared. (envtrust may refuse MORE than this table — IFS is its
	// own strengthening, not shared — the guard holds the sharing, not an
	// equality.)
	for _, name := range []string{"BASH_ENV", "LD_PRELOAD", "NODE_OPTIONS", "PATH", "XDG_CONFIG_HOME", "NIX_CONFIG", "SSH_ASKPASS", "PS4", "SHELLOPTS", "EDITOR"} {
		if !DeniedEnvKey(name) {
			t.Errorf("%s left this table's deny class: the execution portion must keep answering the container question", name)
		}
		if !envtrust.ExecutionGradeName(name) {
			t.Errorf("%s left the dotenv execution grade: the partition requires the two gates to name it together", name)
		}
	}
}
