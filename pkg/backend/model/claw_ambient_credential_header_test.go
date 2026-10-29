package model

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/credheader"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// #2244: the crossing forwarded ANTHROPIC_CUSTOM_HEADERS whole, and the CLI
// applies that block LAST on every request it sends, for every provider
// (claw identity.go Identity.Apply) — so an ambient "Authorization: Bearer …"
// on the launcher authenticated the in-container runner's requests at the
// vendor's gateway the engine chose, in place of the credential the engine
// resolved for the node. #1476 closed that shape for the claude_code delegate
// (facadeRouteEnv clears the variable); it stayed open one backend away.
//
// The composition is the REAL one — forwardableProviderEnv, the single map
// every sandboxed claw node is handed — read for what the container receives,
// not for how this package spells it.
//
// Mutation: hand the host value on unfiltered (env[name] = v for the
// header-block branch) and the credential assertions redden; keep only the
// filtering and drop the explicit crossing (skip an empty kept value) and the
// absent-name assertion of the next test reddens.
func TestTheSandboxCrossingRefusesAnAmbientCredentialHeaderLine(t *testing.T) {
	const ambient = "sk-ambient-operator-gateway"
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "x-tenant: acme\nAuthorization: Bearer "+ambient+"\nUser-Agent: my-tool/1.0")

	env, refused, err := forwardableProviderEnv(context.Background(), "anthropic/claude-haiku-4-5")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	crossed, present := env["ANTHROPIC_CUSTOM_HEADERS"]
	if !present {
		t.Fatal("ANTHROPIC_CUSTOM_HEADERS crossed ABSENT — the workspace .env fills it inside the container and writes the headers of every request the node sends")
	}
	if credheader.CarriesName(crossed, credheader.NameAuthorization) {
		t.Errorf("the forwarded block carries an Authorization line: it replaces this node's own credential at the gateway the engine chose")
	}
	if strings.Contains(crossed, ambient) {
		t.Errorf("the ambient credential's value crossed into the container")
	}
	// The predicate is not "drop everything": the documented use of the
	// channel is client identity, and those lines stay the operator's.
	for _, line := range []string{"x-tenant: acme", "User-Agent: my-tool/1.0"} {
		if !strings.Contains(crossed, line) {
			t.Errorf("the forwarded block = %q, want it to keep %q — the channel's documented use", crossed, line)
		}
	}
	// The refusal is said out loud, naming the variable and the header, never
	// the value (the value of a credential line IS the credential).
	if want := "ANTHROPIC_CUSTOM_HEADERS/" + credheader.NameAuthorization; !slices.Contains(refused, want) {
		t.Errorf("refused = %v, want it to name %q so the operator is told the line did not cross", refused, want)
	}
	for _, entry := range refused {
		if strings.Contains(entry, ambient) {
			t.Errorf("the refusal %q carries the credential's value", entry)
		}
	}
}

// The same question asked of the SITE the attacker reaches: one claw task
// through the real sandbox dispatch, and the env the runner command actually
// started with — not the map this package builds for itself. The launcher says
// what it would not carry in the same pass, so the operator reads it on the
// node whose header vanished.
//
// Mutation: hand the host value on unfiltered and the Authorization assertion
// reddens; drop the warning at the call site and the log assertion reddens.
func TestTheRunnerStartsWithoutTheAmbientCredentialHeader(t *testing.T) {
	operatorEnv(t)
	const ambient = "sk-ambient-operator-gateway"
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "x-tenant: acme\nAuthorization: Bearer "+ambient)
	box := newDeliverySandbox(t)
	var logs bytes.Buffer
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{}, WithClawLogger(iterlog.New(iterlog.LevelWarn, &logs)))

	env, err := dispatchToSandbox(t, context.Background(), b, box, box, "anthropic/claude-haiku-4-5")
	if env == nil {
		t.Fatalf("the dispatch started no runner: %v", err)
	}
	crossed, present := env["ANTHROPIC_CUSTOM_HEADERS"]
	if !present {
		t.Fatal("the runner started with ANTHROPIC_CUSTOM_HEADERS absent — the workspace .env fills it inside the container")
	}
	if credheader.CarriesName(crossed, credheader.NameAuthorization) || strings.Contains(crossed, ambient) {
		t.Errorf("the runner started with the ambient credential header: %q", crossed)
	}
	if !strings.Contains(crossed, "x-tenant: acme") {
		t.Errorf("the runner lost the operator's identity header: %q", crossed)
	}
	// Told, on this node, by name.
	if got := logs.String(); !strings.Contains(got, "ANTHROPIC_CUSTOM_HEADERS/"+credheader.NameAuthorization) {
		t.Errorf("the launcher said nothing about the line it refused; logs = %q", got)
	}
	if strings.Contains(logs.String(), ambient) {
		t.Errorf("the launcher logged the credential's value; logs = %q", logs.String())
	}
}

// Every header-block name on the forwarding list crosses FILTERED and
// EXPLICIT — derived over the list, so the next such variable is covered the
// day it is added rather than the day someone remembers this rule. Explicit
// matters as much as filtered: the runner auto-loads the workspace .env, which
// fills names MISSING from the process env (applyDotEnv honours LookupEnv), so
// a name that crosses absent lets the repository under review write the very
// line refused here — and a block made only of credential lines is exactly the
// case where the filtered value is empty.
//
// Mutation: skip the empty kept value (keep the `if v != ""` of the other
// names) and the absent-name arm reddens on both inputs.
func TestEveryHeaderBlockForwardCrossesFilteredAndExplicit(t *testing.T) {
	var names []string
	for _, name := range providerCredentialEnvVars {
		if headerBlockEnvName(name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		t.Fatal("no header-block name on the forwarding list: the probe lost its subject, and a green verdict here proves nothing")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				what string
				raw  string
			}{
				{"only a credential line", "x-api-key: sk-ambient-key"},
				{"the host sets nothing", ""},
			} {
				t.Run(tc.what, func(t *testing.T) {
					t.Setenv(name, tc.raw)
					if tc.raw == "" {
						// Truly absent, not empty: that is the shape the
						// workspace .env fills. t.Setenv's cleanup restores it.
						if err := os.Unsetenv(name); err != nil {
							t.Fatalf("os.Unsetenv(%s): %v", name, err)
						}
					}
					env, _, err := forwardableProviderEnv(context.Background(), "anthropic/claude-haiku-4-5")
					if err != nil {
						t.Fatalf("forwardableProviderEnv: %v", err)
					}
					crossed, present := env[name]
					if !present {
						t.Fatalf("%s crossed ABSENT — the repository's own .env fills it inside the container", name)
					}
					if strings.TrimSpace(crossed) != "" {
						t.Errorf("%s crossed as %q, want the explicit empty", name, crossed)
					}
				})
			}
		})
	}
}

// A predicate that over-matched would be silent and ruinous: a KEY read as a
// header block crosses stripped to nothing, and the sandboxed node loses its
// credential with nothing said. So the crossing is asked about every name it
// forwards, each holding a value a header-block filter could not leave alone.
//
// Mutation: make headerBlockEnvName answer true for every name and the
// "crossed as" arm reddens on the keys.
func TestOnlyAHeaderBlockIsFilteredOnTheWayIn(t *testing.T) {
	operatorEnv(t)
	value := func(name string) string { return "Authorization: Bearer sk-value-of-" + name }
	for _, name := range providerCredentialEnvVars {
		t.Setenv(name, value(name))
	}
	env, _, err := forwardableProviderEnv(context.Background(), "anthropic/claude-haiku-4-5")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	// Which names must cross WHOLE is read from the name's own shape and from
	// a floor of names this list is built on — never from the predicate under
	// test, or the expectation is that predicate's own echo and an over-match
	// passes.
	floor := []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ZAI_API_KEY", "MOONSHOT_API_KEY",
		"OPENAI_API_KEY", "XAI_API_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		"ITERION_LLM_USER_AGENT", "CLAW_USER_AGENT",
	}
	for _, name := range floor {
		if !slices.Contains(providerCredentialEnvVars, name) {
			t.Fatalf("%s left the forwarding list: this probe's floor no longer describes it", name)
		}
	}
	checked := 0
	for _, name := range providerCredentialEnvVars {
		keyShaped := strings.HasSuffix(name, "_API_KEY") || strings.Contains(name, "_TOKEN") ||
			strings.HasSuffix(name, "_BASE_URL") || strings.HasSuffix(name, "_ENDPOINT")
		if !keyShaped && !slices.Contains(floor, name) {
			continue
		}
		checked++
		if got := env[name]; got != value(name) {
			t.Errorf("%s crossed as %q, want the host's value %q — a name read as a header block loses its value, and a sandboxed node its credential", name, got, value(name))
		}
	}
	if checked < len(floor) {
		t.Fatalf("the probe checked %d names of the forwarding list: it lost its subject", checked)
	}
}

// The reading is on a name's SEGMENTS, so a name that merely contains the
// letters is not mistaken for a block of headers, and a name that carries the
// segment under another spelling still is.
func TestHeaderBlockEnvNameReadsASegment(t *testing.T) {
	for name, want := range map[string]bool{
		"ANTHROPIC_CUSTOM_HEADERS": true,
		"CLAW_EXTRA_HEADERS":       true,
		"ITERION_LLM_HEADER_SET":   true,
		"ANTHROPIC_API_KEY":        false,
		"ZAI_API_KEY":              false,
		"CLAW_USER_AGENT":          false,
		"HEADERLESS_KNOB":          false,
		"ITERION_HEADERROOM":       false,
	} {
		if got := headerBlockEnvName(name); got != want {
			t.Errorf("headerBlockEnvName(%q) = %v, want %v", name, got, want)
		}
	}
}

// A host-posed block with nothing to refuse crosses byte for byte: the
// crossing filters a credential, it does not reformat the operator's headers.
func TestAHeaderBlockWithNoCredentialCrossesUntouched(t *testing.T) {
	const block = "x-tenant: acme\r\nUser-Agent: my-tool/1.0"
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", block)
	env, refused, err := forwardableProviderEnv(context.Background(), "anthropic/claude-haiku-4-5")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if got := env["ANTHROPIC_CUSTOM_HEADERS"]; got != block {
		t.Errorf("the forwarded block = %q, want the operator's own %q", got, block)
	}
	if len(refused) != 0 {
		t.Errorf("refused = %v on a block that authenticates nothing", refused)
	}
}

// The credential names are the names the PRODUCER uses: claw's own auth sets
// them on the request, and the custom-header block is applied over it, so a
// line of such a name stands in for the node's credential. Read back out of
// the vendored CLI rather than restated — a provider claw gains, or an auth
// header it starts sending, arrives covered by the strip or red here.
//
// Mutation: drop "x-api-key" from credheader's set and this test names it as
// an auth header the strip would carry.
func TestTheCredentialHeaderNamesCoverClawsOwnAuthHeaders(t *testing.T) {
	root := moduleRootForVendorWalk(t)
	// The block is applied by the API clients' own identity pass
	// (internal/api: ResolveIdentity, then Identity.Apply on the request), so
	// the derivation reads exactly the tree whose requests it rides. Named
	// rather than hidden: claw's web_search tool sends an x-subscription-token
	// of its own, and no custom header reaches it — the tool runs on the
	// launcher with the launcher's credentials (webSearchLauncherEnv), and its
	// requests never pass through Identity.Apply.
	clawDir := filepath.Join(root, "vendor", "github.com", "SocialGouv", "claw-code-go", "internal", "api")
	if _, err := os.Stat(clawDir); err != nil {
		t.Fatalf("the vendored claw API tree is not at %s (%v): the derivation has nothing to read", clawDir, err)
	}
	// Header.Set("<name>", <expression that mentions a credential>) — the
	// shape every auth header of claw's providers is written in
	// (internal/api/auth.go, internal/api/client.go, the openai, vertex and
	// foundry providers).
	setHeader := regexp.MustCompile(`Header\.Set\(\s*"([A-Za-z0-9-]+)"\s*,\s*([^)]*)\)`)
	credentialish := regexp.MustCompile(`(?i)apikey|bearer|accesstoken|oauthtoken|\btoken\b`)
	found := map[string]string{} // header name -> the file it is set in
	err := filepath.WalkDir(clawDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, m := range setHeader.FindAllStringSubmatch(string(body), -1) {
			if !credentialish.MatchString(m[2]) {
				continue
			}
			name := strings.ToLower(m[1])
			if _, seen := found[name]; !seen {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					rel = path
				}
				found[name] = rel
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the vendored claw: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no auth header found in the vendored claw: the walk no longer matches its source, so a green verdict here proves nothing")
	}
	for _, want := range []string{"authorization", "x-api-key"} {
		if _, ok := found[want]; !ok {
			t.Errorf("the derivation no longer finds %s in the vendored claw (found %v): either claw stopped sending it, or the walk stopped reading its source", want, found)
		}
	}
	for name, file := range found {
		if !credheader.IsCredentialName(name) {
			t.Errorf("claw authenticates with the %s header (%s) and the sandbox crossing would carry an ambient line of that name: add it to credheader's credential set", name, file)
		}
	}
}
