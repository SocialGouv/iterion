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
// Every neutralised row is a name of the credential/routing sub-class —
// its consumer reads a VALUE (a truthiness, a URL, a key, an identity),
// so an empty one is inert. A reported row is everything else: a name
// where an empty value would break an image doing its job or is read as
// presence, AND a name that leads to code the image itself ships, which
// is told rather than emptied (ARB-121).
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

	// The three channels that RESTORE a neutralised name from inside the
	// container: bash reads BASH_ENV for every non-interactive shell —
	// which is what a tool node and an agent's Bash tool are — and node
	// takes a module from NODE_OPTIONS=--require and resolves a bare one
	// through NODE_PATH, inside the agent CLI's own process. Measured to
	// FIRE, and REPORTED: the image that bakes them ships the script and
	// the module they would run, so the empty value buys no promise
	// (ARB-121, amending ARB-119).
	{"BASH_ENV", reportedOnly},
	{"NODE_OPTIONS", reportedOnly},
	{"NODE_PATH", reportedOnly},

	// Case is not a membership rule: the exact-name half answers the same
	// for any spelling, which is why the table carries no lower-case twin
	// of its own rows.
	{"bash_env", reportedOnly},
	{"gcloud_project", neutralizeInImage},
	{"Node_Options", reportedOnly},
	{"path", reportedOnly},

	// Every member of this namespace is a shell function definition bash
	// imports into every non-interactive shell: a code channel of the
	// image, told and kept like the rest.
	{"BASH_FUNC_git%%", reportedOnly},
	// git's config with no file and no HOME: `git -c` spelled in the
	// environment, and one answer for the whole `GIT_` family — no
	// narrower namespace refines it, because a refinement that answered
	// the same thing would be dead code.
	{"GIT_CONFIG_KEY_0", reportedOnly},
	{"GIT_CONFIG_VALUE_0", reportedOnly},
	{"GIT_CONFIG_PARAMETERS", reportedOnly},

	// The loader, toolchain and home namespaces: reported, because an empty
	// value breaks the image or nothing a process iterion starts reads the
	// name. ITERION_ is the measured one — iterion's own images bake
	// ITERION_VERSION, ITERION_COMMIT and ITERION_BOTS_PATH.
	{"ITERION_VERSION", reportedOnly},
	{"NIX_CONFIG", reportedOnly},
	{"GIT_SSH_COMMAND", reportedOnly},
	{"LD_LIBRARY_PATH", reportedOnly},
	{"OPENSSL_MODULES", reportedOnly},
	{"BUN_INSTALL", reportedOnly},
	{"NPM_CONFIG_PREFIX", reportedOnly},
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
		assertGuardHasItsOwnDispositionRow(t, "suffix "+guard.pattern, "suffix", guard.pattern, guard.image, func(key string) bool {
			return strings.HasSuffix(strings.ToUpper(key), guard.pattern)
		})
	}
	for _, guard := range deniedEnvNamespaces {
		assertGuardHasItsOwnDispositionRow(t, "namespace "+guard.pattern, "namespace", guard.pattern, guard.image, func(key string) bool {
			return strings.HasPrefix(strings.ToUpper(key), guard.pattern)
		})
	}
	// The exact names need no loop here: every one of them is declared,
	// name by name, in [TestTheImageDispositionOfEveryExactNameIsDeclared],
	// which reddens on a row deleted, added or flipped.
}

// assertGuardHasItsOwnDispositionRow fails unless some row of
// [dispositionCases] is LOAD-BEARING for this guard: the class answers the
// guard's own disposition for it, and removing the guard from the walk
// changes that answer.
//
// "Removing it changes the answer" rather than "no other guard catches it":
// a NARROWER guard refines a family that already answers — `GIT_CONFIG_KEY_`
// inside `GIT_`, the exact `LD_PRELOAD` inside `LD_` — so counting the
// guards that match would call every refinement un-witnessed while a
// deleted refinement silently downgrades a name from neutralised to
// reported. The measurement is the mutation this guard would suffer.
func assertGuardHasItsOwnDispositionRow(t *testing.T, guard string, kind, pattern string, want imageEnvDisposition, caughtByGuard func(string) bool) {
	t.Helper()
	for _, c := range dispositionCases {
		if c.want == notDenied || !caughtByGuard(c.key) {
			continue
		}
		if got := screenEnvName(c.key); got != want {
			continue // another guard answers for this row, with another disposition
		}
		if screenEnvNameSkipping(c.key, kind, pattern) == want {
			continue // the answer survives without this guard: not its own row
		}
		return
	}
	t.Errorf("%s has no row whose answer it alone decides — add one to dispositionCases, or drop the guard", guard)
}

// screenEnvNameSkipping answers [screenEnvName]'s walk with ONE guard left
// out — kind is "suffix", "namespace" or "exact" — so a test can measure
// whether a guard decides anything.
//
// A copy of the walk, and [TestTheGuardSkippingWalkIsTheRealWalk] is what
// keeps it from drifting: with nothing skipped it must answer exactly what
// the production walk answers, for every row of the table.
func screenEnvNameSkipping(name, kind, pattern string) imageEnvDisposition {
	upper := strings.ToUpper(name)
	if kind != "exact" || !strings.EqualFold(pattern, upper) {
		if d, exact := deniedEnvNames[upper]; exact {
			return d
		}
	}
	for _, guard := range deniedEnvSuffixes {
		if kind == "suffix" && guard.pattern == pattern {
			continue
		}
		if strings.HasSuffix(upper, guard.pattern) {
			return guard.image
		}
	}
	for _, guard := range deniedEnvNamespaces {
		if kind == "namespace" && guard.pattern == pattern {
			continue
		}
		if strings.HasPrefix(upper, guard.pattern) {
			return guard.image
		}
	}
	return notDenied
}

// TestTheGuardSkippingWalkIsTheRealWalk holds the copy above to the
// production walk: skipping nothing must answer exactly [screenEnvName],
// or every "this guard is load-bearing" verdict is about another function.
func TestTheGuardSkippingWalkIsTheRealWalk(t *testing.T) {
	for _, c := range dispositionCases {
		if got, want := screenEnvNameSkipping(c.key, "", ""), screenEnvName(c.key); got != want {
			t.Errorf("screenEnvNameSkipping(%q, nothing) = %v, screenEnvName = %v — the copy has drifted from the walk it measures", c.key, got, want)
		}
	}
	for name := range deniedEnvNames {
		if got, want := screenEnvNameSkipping(name, "", ""), screenEnvName(name); got != want {
			t.Errorf("screenEnvNameSkipping(%q, nothing) = %v, screenEnvName = %v", name, got, want)
		}
	}
}

// TestNoNamespaceIsShadowedByABroaderOneBeforeIt is the order the
// precedence doc promises: within the namespaces the first match answers,
// so a namespace that REFINES another must be listed before it. Reversed,
// the refinement would be dead code that reads like a guard.
func TestNoNamespaceIsShadowedByABroaderOneBeforeIt(t *testing.T) {
	for i, narrow := range deniedEnvNamespaces {
		for j, broad := range deniedEnvNamespaces {
			if i == j || !strings.HasPrefix(narrow.pattern, broad.pattern) {
				continue
			}
			if j < i {
				t.Errorf("namespace %q is listed after %q, which prefixes it: the first match answers, so %q is never consulted — a guard the walk cannot reach",
					narrow.pattern, broad.pattern, narrow.pattern)
			}
		}
	}
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
		"PATH": reportedOnly, "HOME": reportedOnly,
		"GOROOT": reportedOnly, "GOFLAGS": reportedOnly,

		// Reported, and the measurement is that the channel FIRES: bash's
		// non-interactive startup file, the two node variables read
		// inside the agent CLI's own process, glibc's gconv modules,
		// bash's xtrace pair and GNU tar's own options. Each ran the
		// planter's code on a disposable image through that program's own
		// startup path, and each is still only told: an image a
		// repository chooses is the repository's code (ARB-121, amending
		// ARB-119). The names whose FAMILY answers the same thing own no
		// row here — LD_PRELOAD/LD_AUDIT under `LD_`, OPENSSL_CONF under
		// `OPENSSL_`, BUN_OPTIONS under `BUN_`, the NPM_CONFIG_* pair
		// under `NPM_CONFIG_`, GIT_CONFIG_COUNT under `GIT_` — because a
		// refinement that answers its family's answer is dead code.
		"BASH_ENV": reportedOnly, "NODE_OPTIONS": reportedOnly,
		"NODE_PATH": reportedOnly, "GCONV_PATH": reportedOnly,
		"SHELLOPTS": reportedOnly, "PS4": reportedOnly,
		"TAR_OPTIONS": reportedOnly,

		// Reported: shells and the startup files they DO read, toolchains,
		// editors — where an empty value is read as presence, breaks the
		// image's own build, or nothing a process iterion starts reads the
		// name (`ENV`, measured on dash and on bash-as-sh).
		"BASHOPTS": reportedOnly,
		"ENV":      reportedOnly, "ZDOTDIR": reportedOnly,
		"SSH_ASKPASS": reportedOnly, "SSH_ASKPASS_REQUIRE": reportedOnly,
		"EDITOR": reportedOnly, "VISUAL": reportedOnly,
		"GOENV": reportedOnly, "SHELL": reportedOnly,
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
