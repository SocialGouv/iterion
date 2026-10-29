package devcontainer

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// dispositionCase is one variable name and what the class answers when an
// IMAGE bakes it in.
type dispositionCase struct {
	key  string
	want imageEnvDisposition
}

// dispositionCases name, per PATTERN guard of the class, one variable that
// guard alone catches — the row that reddens when its flag is flipped or its
// entry removed. The exact names are held by
// [TestTheImageDispositionOfEveryExactNameIsDeclared] instead: there, the
// name IS the guard.
//
// Every neutralised row is a name whose consumer reads a VALUE (a
// truthiness, a URL, a key, an identity), so an empty one is inert; every
// reported row is a name where an empty value would break an image doing
// its job, or is read as presence.
var dispositionCases = []dispositionCase{
	// The suffix family is the credential/routing sub-class by definition.
	{"MYTOOL_BASE_URL", neutralizeInImage},
	{"MYTOOL_API_KEY", neutralizeInImage},
	{"GITHUB_TOKEN", neutralizeInImage},
	{"NEXTAUTH_SECRET", neutralizeInImage},
	{"HTTPS_PROXY", neutralizeInImage},

	// The agent and provider namespaces: a route and a credential each, and
	// no image iterion publishes bakes one of them.
	{"CLAW_USER_AGENT", neutralizeInImage},
	{"CLAWD_OTLP_GRPC_ENDPOINT", neutralizeInImage},
	{"HOOK_USER_PROMPT", neutralizeInImage},
	{"CLAUDE_CODE_USE_BEDROCK", neutralizeInImage},
	{"CCR_OAUTH_TOKEN_FILE", neutralizeInImage},
	{"ANT_OTEL_EXPORTER_OTLP_ENDPOINT", neutralizeInImage},
	{"ANT_CLAUDE_CODE_METRICS_ENDPOINT", neutralizeInImage},
	{"ANTHROPIC_UNIX_SOCKET", neutralizeInImage},
	{"OPENCODE_CONFIG", neutralizeInImage},
	{"AWS_ACCESS_KEY_ID", neutralizeInImage},
	{"AZURE_CLIENT_ID", neutralizeInImage},
	{"GOOGLE_APPLICATION_CREDENTIALS", neutralizeInImage},
	{"GCE_METADATA_HOST", neutralizeInImage},
	{"CLOUDSDK_CONFIG", neutralizeInImage},
	{"VERTEX_REGION_CLAUDE_3_5_HAIKU", neutralizeInImage},
	{"OTEL_EXPORTER_OTLP_ENDPOINT", neutralizeInImage},
	{"SENTRY_DSN", neutralizeInImage},
	{"GH_CONFIG_DIR", neutralizeInImage},
	{"GLAB_CONFIG_DIR", neutralizeInImage},

	// The three names that RESTORE what the rest of the sub-class is
	// emptied for: bash reads BASH_ENV for every non-interactive shell —
	// which is what a tool node and an agent's Bash tool are — and node
	// takes a module from NODE_OPTIONS=--require and resolves a bare one
	// through NODE_PATH, inside the agent CLI's own process. A channel
	// that re-exports a neutralised name belongs to the sub-class, and
	// empty is their default (measured: bash and node run normally with
	// all three empty).
	{"BASH_ENV", neutralizeInImage},
	{"NODE_OPTIONS", neutralizeInImage},
	{"NODE_PATH", neutralizeInImage},

	// Case is not a membership rule: the exact-name half answers the same
	// for any spelling, which is why the table carries no lower-case twin
	// of its own rows.
	{"bash_env", neutralizeInImage},
	{"gcloud_project", neutralizeInImage},
	{"Node_Options", neutralizeInImage},
	{"path", reportedOnly},

	// The loader, toolchain and home namespaces: reported, because an empty
	// value breaks the image. ITERION_ is the measured one — iterion's own
	// images bake ITERION_VERSION, ITERION_COMMIT and ITERION_BOTS_PATH.
	{"ITERION_VERSION", reportedOnly},
	{"NIX_CONFIG", reportedOnly},
	{"GIT_SSH_COMMAND", reportedOnly},
	{"LD_PRELOAD", reportedOnly},
	{"OPENSSL_CONF", reportedOnly},
	{"BASH_FUNC_git%%", reportedOnly},
	{"BUN_OPTIONS", reportedOnly},
	{"NPM_CONFIG_NODE_OPTIONS", reportedOnly},
	{"XDG_CONFIG_HOME", reportedOnly},

	// Outside the class: no disposition to give.
	{"NODE_ENV", notDenied},
	{"JAVA_HOME", notDenied},
	{"CHROMIUM_FLAGS", notDenied},
	{"ULTRA11Y_BROWSER_PATH", notDenied},
}

// TestScreenImageEnvNameAnswersItsRows holds the public screen to the table
// above, through the exported seam the runtime calls.
func TestScreenImageEnvNameAnswersItsRows(t *testing.T) {
	for _, c := range dispositionCases {
		t.Run(c.key, func(t *testing.T) {
			denied, neutralize := ScreenImageEnvName(c.key)
			wantDenied := c.want != notDenied
			wantNeutralize := c.want == neutralizeInImage
			if denied != wantDenied || neutralize != wantNeutralize {
				t.Errorf("ScreenImageEnvName(%q) = (denied %v, neutralize %v), want (%v, %v)",
					c.key, denied, neutralize, wantDenied, wantNeutralize)
			}
			// One walk, one answer: the membership the devcontainer screen
			// reads can never disagree with the one the image screen reads.
			if got := DeniedEnvKey(c.key); got != wantDenied {
				t.Errorf("DeniedEnvKey(%q) = %v but ScreenImageEnvName says denied=%v — two readings of one table",
					c.key, got, wantDenied)
			}
		})
	}
}

// TestEveryPatternGuardHasADispositionRowOnlyItCatches is two of the three
// loops: for each suffix and each namespace of the class, some row above is
// caught by that guard and by no other, and the class answers that guard's
// own disposition for it. A guard whose flag is flipped reddens its row; a
// guard added without a row of its own reddens here.
func TestEveryPatternGuardHasADispositionRowOnlyItCatches(t *testing.T) {
	for _, guard := range deniedEnvSuffixes {
		assertGuardHasItsOwnDispositionRow(t, "suffix "+guard.pattern, guard.image, func(key string) bool {
			return strings.HasSuffix(strings.ToUpper(key), guard.pattern)
		})
	}
	for _, guard := range deniedEnvNamespaces {
		assertGuardHasItsOwnDispositionRow(t, "namespace "+guard.pattern, guard.image, func(key string) bool {
			return strings.HasPrefix(strings.ToUpper(key), guard.pattern)
		})
	}
}

// assertGuardHasItsOwnDispositionRow fails unless some row of
// [dispositionCases] is caught by this guard alone — no other pattern, no
// exact entry — and the class answers want for it.
func assertGuardHasItsOwnDispositionRow(t *testing.T, guard string, want imageEnvDisposition, caughtByGuard func(string) bool) {
	t.Helper()
	for _, c := range dispositionCases {
		if c.want == notDenied || !caughtByGuard(c.key) {
			continue
		}
		upper := strings.ToUpper(c.key)
		others := 0
		for _, g := range deniedEnvSuffixes {
			if strings.HasSuffix(upper, g.pattern) {
				others++
			}
		}
		for _, g := range deniedEnvNamespaces {
			if strings.HasPrefix(upper, g.pattern) {
				others++
			}
		}
		if _, exact := deniedEnvNames[upper]; exact {
			others++
		}
		if others != 1 {
			continue // some other guard would answer for this row too
		}
		if got := screenEnvName(c.key); got != want {
			t.Errorf("%s: its own row %q answers %v, want the guard's %v", guard, c.key, got, want)
		}
		return
	}
	t.Errorf("%s has no disposition row only it catches — add one to dispositionCases", guard)
}

// TestTheImageDispositionOfEveryExactNameIsDeclared is the third loop: the
// exact names are the guards that ARE their own row, so the partition is
// declared here, name by name, independently of the production table. A flag
// flipped, a name added or a name dropped reddens with the name printed.
func TestTheImageDispositionOfEveryExactNameIsDeclared(t *testing.T) {
	// The credential/routing sub-class among the exact names: the endpoint,
	// region, project and managed-identity names, whose consumers read a
	// value an empty string makes inert.
	want := map[string]imageEnvDisposition{
		"OPENAI_API_BASE":                neutralizeInImage,
		"BEDROCK_REGION":                 neutralizeInImage,
		"CLOUD_ML_REGION":                neutralizeInImage,
		"GCLOUD_PROJECT":                 neutralizeInImage,
		"AGENT_PROXY_URL":                neutralizeInImage,
		"BETA_TRACING_ENDPOINT":          neutralizeInImage,
		"ENABLE_BETA_TRACING_DETAILED":   neutralizeInImage,
		"ENABLE_ENHANCED_TELEMETRY_BETA": neutralizeInImage,
		"SESSION_INGRESS_URL":            neutralizeInImage,
		"IDENTITY_ENDPOINT":              neutralizeInImage,
		"IDENTITY_HEADER":                neutralizeInImage,
		"IDENTITY_SERVER_THUMBPRINT":     neutralizeInImage,
		"IMDS_ENDPOINT":                  neutralizeInImage,
		"MSI_ENDPOINT":                   neutralizeInImage,
		"DEFAULT_IDENTITY_CLIENT_ID":     neutralizeInImage,
		"MSAL_FORCE_REGION":              neutralizeInImage,

		// Reported: an empty value breaks the container outright.
		"PATH": reportedOnly, "HOME": reportedOnly, "SHELL": reportedOnly,
		"GOROOT": reportedOnly, "GOFLAGS": reportedOnly,

		// Neutralised: the three channels that RESTORE a name the screen
		// emptied — bash's non-interactive startup file, and the two node
		// variables read inside the agent CLI's own process. Empty is
		// their default.
		"BASH_ENV": neutralizeInImage, "NODE_OPTIONS": neutralizeInImage,
		"NODE_PATH": neutralizeInImage,

		// Reported: code loaders, shells and their startup files,
		// toolchains, editors — the execution half of the class, where an
		// empty value is read as presence or breaks the image's own build.
		"GCONV_PATH": reportedOnly,
		"SHELLOPTS":  reportedOnly, "BASHOPTS": reportedOnly,
		"PS4": reportedOnly, "ENV": reportedOnly, "ZDOTDIR": reportedOnly,
		"SSH_ASKPASS": reportedOnly, "SSH_ASKPASS_REQUIRE": reportedOnly,
		"EDITOR": reportedOnly, "VISUAL": reportedOnly,
		"TAR_OPTIONS": reportedOnly, "GOENV": reportedOnly,
		"GOTOOLCHAIN": reportedOnly, "GOAUTH": reportedOnly,

		// Reported: emptying these sends the client to its DEFAULT, which
		// under host_state is the OPERATOR's own kubeconfig, docker socket
		// and agent config home — worse than the planted path.
		"KUBECONFIG": reportedOnly, "DOCKER_HOST": reportedOnly,
		"DOCKER_CONFIG": reportedOnly, "CODEX_HOME": reportedOnly,
		"KIMI_CODE_HOME": reportedOnly, "PI_CODING_AGENT_DIR": reportedOnly,

		// Reported: the names of binaries a CLI spawns, and the TLS bundles
		// an image may legally bake for its own tooling.
		"VITALS_EMITTER_BIN": reportedOnly, "SDK_NATIVE_BIN": reportedOnly,
		"DS_CHROMIUM_PATH":    reportedOnly,
		"NODE_EXTRA_CA_CERTS": reportedOnly, "NODE_TLS_REJECT_UNAUTHORIZED": reportedOnly,
		"SSL_CERT_FILE": reportedOnly, "SSL_CERT_DIR": reportedOnly,
		"CURL_CA_BUNDLE": reportedOnly, "REQUESTS_CA_BUNDLE": reportedOnly,
		"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH": reportedOnly,
		"GRPC_SSL_CIPHER_SUITES":           reportedOnly,
	}
	if !maps.Equal(deniedEnvNames, want) {
		for _, name := range slices.Sorted(maps.Keys(deniedEnvNames)) {
			if got, declared := deniedEnvNames[name], want[name]; got != declared {
				t.Errorf("deniedEnvNames[%q] = %v, declared %v", name, got, declared)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(want)) {
			if _, in := deniedEnvNames[name]; !in {
				t.Errorf("%q is declared here and gone from deniedEnvNames", name)
			}
		}
	}
	// notDenied is the zero value, so an entry written with it would read
	// as "outside the class" at every lookup — a name silently un-denied.
	for name, d := range deniedEnvNames {
		if d == notDenied {
			t.Errorf("deniedEnvNames[%q] is notDenied — an entry of the class must declare a disposition", name)
		}
		// The lookup upper-cases what it is asked, so an entry spelled any
		// other way is unreachable: dead code that reads like a guard.
		if upper := strings.ToUpper(name); name != upper {
			t.Errorf("deniedEnvNames[%q] is never consulted — the lookup asks for %q; one entry answers for every spelling", name, upper)
		}
	}
}

// TestEveryExactNameAnswersTheSameInAnyCase holds the rule the suffix and
// namespace halves already followed, at the class rather than at a row: an
// exact name is matched case-INSENSITIVELY, so no spelling of it escapes
// its disposition.
//
// Read from the production table, never from a copy, and it is the reason
// the table carries no lower-case twin: `gcloud_project` used to be its own
// entry, which applied the rule at ONE row and left every sibling
// (`openai_api_base`, `imds_endpoint`, `msi_endpoint`, `bedrock_region`,
// `bash_env`) answering "outside the class".
func TestEveryExactNameAnswersTheSameInAnyCase(t *testing.T) {
	for _, name := range slices.Sorted(maps.Keys(deniedEnvNames)) {
		want := deniedEnvNames[name]
		for _, spelling := range []string{strings.ToLower(name), strings.ToUpper(name), mixedCase(name)} {
			if got := screenEnvName(spelling); got != want {
				t.Errorf("screenEnvName(%q) = %v, want the %q row's %v — case is not a membership rule",
					spelling, got, name, want)
			}
		}
	}
}

// mixedCase spells name with every other rune upper-cased, so a reader of
// the failure sees a spelling no row of the table carries.
func mixedCase(name string) string {
	runes := []rune(strings.ToLower(name))
	for i := range runes {
		if i%2 == 0 {
			runes[i] = []rune(strings.ToUpper(string(runes[i])))[0]
		}
	}
	return string(runes)
}

// TestTheDispositionPrecedenceTiltsTowardNeutralising holds the documented
// order of [ScreenImageEnvName]: exact names first, then suffixes, then
// namespaces. The case that matters is a name both a reported-only
// namespace and a neutralised suffix catch — the suffix must answer, or a
// repository's image could ship a credential under a reported prefix.
func TestTheDispositionPrecedenceTiltsTowardNeutralising(t *testing.T) {
	for _, name := range []string{"ITERION_SOMETHING_TOKEN", "XDG_SOMETHING_API_KEY", "GIT_SOMETHING_SECRET"} {
		t.Run(name, func(t *testing.T) {
			upper := strings.ToUpper(name)
			namespaced := false
			for _, g := range deniedEnvNamespaces {
				if strings.HasPrefix(upper, g.pattern) && g.image == reportedOnly {
					namespaced = true
				}
			}
			if !namespaced {
				t.Fatalf("%q no longer rides a reported-only namespace — the row proves nothing", name)
			}
			denied, neutralize := ScreenImageEnvName(name)
			if !denied || !neutralize {
				t.Errorf("ScreenImageEnvName(%q) = (%v, %v), want the suffix to answer (true, true)", name, denied, neutralize)
			}
		})
	}
	// And the other direction: a reported-only namespace with no suffix of
	// the class stays reported, so the precedence is not "neutralise
	// everything".
	if denied, neutralize := ScreenImageEnvName("ITERION_SOMETHING_ELSE"); !denied || neutralize {
		t.Errorf("ScreenImageEnvName(ITERION_SOMETHING_ELSE) = (%v, %v), want (true, false)", denied, neutralize)
	}
}
