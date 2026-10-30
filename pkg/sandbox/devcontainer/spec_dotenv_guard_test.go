package devcontainer

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// classed records which dotenv classes answer a name — the four questions
// the partition asks, so a member walk can report the answer it got, not
// just yes/no.
type classed struct {
	routing, operator, credential, execution bool
}

func (c classed) any() bool { return c.routing || c.operator || c.credential || c.execution }

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
	"OPENCODE_PROBE":        "the namespace's remainder is provider switches and knobs: the config file, dir, inline content and permission names are execution-grade by exact name (envtrust's config-homes group — opencode is one of the engine's own backends, delegate CLILive), OPENCODE_API_KEY is credential-shaped",
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
	"GIT_PROBE":             "the transport family (GIT_SSH, GIT_PROXY_COMMAND, GIT_CONFIG_) is the routing class's; GIT_ASKPASS, GIT_EXEC_PATH and the editor chain with the pager (GIT_EDITOR, GIT_SEQUENCE_EDITOR, GIT_PAGER) are execution-grade by exact name; GIT_SSL_* are git's own TLS trust, left to this gate",
	// The exact names that are provider switches, regions or identity
	// chains rather than execution or trust: they bite through provider
	// credentials the file may carry, or route (the *_ENDPOINT/_URL ones
	// the routing shapes already answer). The walk over the real members
	// below lands here too: a member whose answer is the namespace line's
	// answer, spelled exact, carries that line's reason.
	"BEDROCK_REGION":                 "provider region switch",
	"CLOUD_ML_REGION":                "provider region switch",
	"GCLOUD_PROJECT":                 "provider project switch",
	"gcloud_project":                 "provider project switch (the case-sensitive twin)",
	"MSAL_FORCE_REGION":              "Azure identity-chain region",
	"DEFAULT_IDENTITY_CLIENT_ID":     "Azure managed-identity chain, riding IDENTITY_ENDPOINT (_ENDPOINT, routed)",
	"IDENTITY_SERVER_THUMBPRINT":     "Azure managed-identity chain, riding IDENTITY_ENDPOINT",
	"ENABLE_BETA_TRACING_DETAILED":   "the CLI's telemetry switches: reporting posture",
	"ENABLE_ENHANCED_TELEMETRY_BETA": "the CLI's telemetry switches: reporting posture",
	// Real members of the namespaces whose answer is the namespace line's,
	// spelled exact for the member walk below.
	"GIT_SSL_CAINFO":    "git's own TLS trust for git's own fetches (the GIT_PROBE line's answer): the bundle configures the git binary alone, the transport programs the engine's gestures ride are the routing class's",
	"AWS_ACCESS_KEY_ID": "the provider credential the file exists to carry (the AWS_PROBE line's answer, spelled exact for the walk)",
	"AWS_PROFILE":       "profile selection among the credentials the file may carry (the AWS_PROBE line's answer, spelled exact)",
	"ANTHROPIC_API_KEY": "the provider credential the file exists to carry (the ANTHROPIC_PROBE line's answer, spelled exact)",
	"CLOUDSDK_CONFIG":   "gcloud's credential dir: what the file is FOR (the CLOUDSDK_PROBE line's answer, spelled exact)",
	"OPENCODE_API_KEY":  "the provider credential the file exists to carry (credential-shaped: KEY)",
}

// devcontainerRealMembers are REAL names the covered programs read — one
// witness per family the namespace lines cover, the names a repository's
// dotenv file actually spells, not synthetic probes. The H-10 round-8
// review measured this table's namespace walk staying green while six of
// these were container-denied and dotenv-planted (GIT_EDITOR, GIT_PAGER,
// GIT_SEQUENCE_EDITOR, AWS_CA_BUNDLE, CLOUDSDK_PYTHON, OPENCODE_CONFIG):
// a `<NS>_PROBE` probe answers for the prefix, never for the member. Each
// member must be denied HERE (membership stays true — a member the
// container gate does not deny is a table bug), and answered by a dotenv
// class or exempted in devcontainerOnly. A new real member lands red
// before it can be a name only the container gate refuses.
var devcontainerRealMembers = []string{
	// git: the programs the binary runs, the transport it dials, its own
	// TLS trust.
	"GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_PAGER", "GIT_ASKPASS",
	"GIT_EXEC_PATH", "GIT_SSH_COMMAND", "GIT_SSL_CAINFO",
	// the AWS SDK chain: the trust root, the credentials, the per-service
	// endpoint.
	"AWS_CA_BUNDLE", "AWS_ACCESS_KEY_ID", "AWS_PROFILE",
	"AWS_ENDPOINT_URL_BEDROCK",
	// the Anthropic client of the claude CLI, and its credentials.
	"ANTHROPIC_UNIX_SOCKET", "ANTHROPIC_API_KEY", "ANTHROPIC_CUSTOM_HEADERS",
	// opencode: the config file, dir, inline content, permission bits, and
	// the provider key.
	"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_CONFIG_CONTENT",
	"OPENCODE_PERMISSION", "OPENCODE_API_KEY",
	// gcloud: the interpreter it runs, the credential dir it reads.
	"CLOUDSDK_PYTHON", "CLOUDSDK_CONFIG",
	// the cluster and daemon an operator-authored deploy step talks to.
	"KUBECONFIG", "DOCKER_CONFIG",
	// the config homes the dotenv gate carries by exact name, and the
	// shared families.
	"CLAUDE_CONFIG_DIR", "NIX_CONFIG", "XDG_CONFIG_HOME", "SSH_ASKPASS",
	// the interpreters a tool node runs: the module path whose
	// sitecustomize executes at interpreter startup, and the options line
	// every JVM picks up with no flag of ours — the execution grade one
	// inventory answers at both gates, the dotenv one first (a project
	// `.env` planting them) and this one beside it (the repository's
	// devcontainer.json doing the same).
	"PYTHONPATH", "JAVA_TOOL_OPTIONS",
}

// TestEveryDeniedEnvKeyReachesADotenvPlantClassOrAnExemption holds the
// partition between this table and the dotenv plant classes: every exact
// name and every namespace of the deny class must be answered by one of
// envtrust's dotenv classes (routing shapes, operator grade, credential
// shape, execution grade) or sit in devcontainerOnly with its reason. A
// new entry lands red here before it can be a name only the container
// gate refuses.
func TestEveryDeniedEnvKeyReachesADotenvPlantClassOrAnExemption(t *testing.T) {
	answered := func(name string) classed {
		return classed{
			routing:    envtrust.RoutingShapedName(name),
			operator:   envtrust.OperatorGradeRefused(name),
			credential: envtrust.CredentialShapedName(name),
			execution:  envtrust.ExecutionGradeName(name),
		}
	}
	for name := range deniedEnvNames {
		got := answered(name)
		if got.any() {
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
	for _, guard := range deniedEnvNamespaces {
		prefix := guard.pattern
		probe := strings.ToUpper(prefix[:len(prefix)-1]) + "_PROBE"
		if answered(probe).any() {
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
	// The REAL members the covered programs read, not only the synthetic
	// probes: each stays denied here and reaches a dotenv class or an
	// exemption — the parity the probe walk above cannot see.
	for _, name := range devcontainerRealMembers {
		if !DeniedEnvKey(name) {
			t.Errorf("%s is named a real member of the deny class but the container gate denies it not: fix the member table or the deny class", name)
		}
		if answered(name).any() {
			continue
		}
		reason, exempt := devcontainerOnly[name]
		if !exempt {
			t.Errorf("%s is a real member the covered programs read, denied here and planted by the dotenv gate: classify it (envtrust) or exempt it with the reason the dotenv question differs", name)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is exempt with no reason: the exemption must say why the dotenv question differs", name)
		}
	}
	// The execution portion the dotenv gate DID adopt keeps answering, both
	// directions: a name this table refuses for execution must be refused
	// at the dotenv plant too (the opt-in aside), and the shared witnesses
	// stay shared. (envtrust may refuse MORE than this table — IFS is its
	// own strengthening, not shared — the guard holds the sharing, not an
	// equality.)
	for _, name := range []string{"BASH_ENV", "LD_PRELOAD", "NODE_OPTIONS", "PATH", "XDG_CONFIG_HOME", "NIX_CONFIG", "SSH_ASKPASS", "PS4", "SHELLOPTS", "EDITOR", "PYTHONPATH", "JAVA_TOOL_OPTIONS"} {
		if !DeniedEnvKey(name) {
			t.Errorf("%s left this table's deny class: the execution portion must keep answering the container question", name)
		}
		if !envtrust.ExecutionGradeName(name) {
			t.Errorf("%s left the dotenv execution grade: the partition requires the two gates to name it together", name)
		}
	}
}
