package delegate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clawapi "github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// TestIsAuthErrorResult guards the auth-failure classifier that keeps a dead
// forfait token (or rejected API key) from masquerading as a structured-output
// "missing required field" error — the exact trap that once cost a full
// debugging session.
func TestIsAuthErrorResult(t *testing.T) {
	authy := []string{
		"Failed to authenticate. API Error: 401 Invalid bearer token",
		"API Error: 401 {\"type\":\"authentication_error\",\"message\":\"invalid x-api-key\"}",
		"invalid api key",
		"OAuth token has expired",
		"Not logged in \u00b7 Please run /login",
		// A facade's bracketed 401 and a bare 403: the status is the
		// provider's verdict on the credential, whatever prose follows it.
		"API Error: [401][Unauthorized][2026090610430471b2ed5a5eaa4de7]",
		"API Error: 403 Forbidden",
		"API Error: [403][Request blocked][abc]",
	}
	for _, s := range authy {
		if !isAuthErrorResult(s) {
			t.Errorf("isAuthErrorResult(%q) = false, want true", s)
		}
	}

	notAuthy := []string{
		"",
		"Hello! I'm ready to help you with your workflow.",
		// A real answer that merely discusses authentication must not trip it.
		"To fix the login flow, validate the bearer token server-side and reject an invalid api key with a 401 so the client can refresh; " +
			"add a regression test that a malformed token fails to authenticate cleanly instead of 500ing, and document the error shape for the SDK consumers.",
		// The length cap keeps a real answer that QUOTES the no-credential
		// render from tripping the classifier.
		"The health probe you asked about renders \u00ab Not logged in \u00b7 Please run /login \u00bb whenever the session cookie is absent; " +
			"to reproduce it, clear the cookie jar, reload the dashboard, and assert the banner text in the e2e spec so a regression is caught early.",
		// A rate-limit notice is a DIFFERENT class (handled by isRateLimitMessage).
		"You've hit your weekly limit · resets 9pm (Europe/Paris)",
		// A server error is the transient class, never a credential verdict.
		"API Error: [500][Operation failed][2026090610430471b2ed5a5eaa4de7]",
	}
	for _, s := range notAuthy {
		if isAuthErrorResult(s) {
			t.Errorf("isAuthErrorResult(%q) = true, want false", s)
		}
	}
}

// The malformed-credential render: a secret that is not a token at all is
// quoted back inside an unbounded "API Error: Header …" line. Prefix-anchored
// so only a result that IS the error matches — an agent quoting one
// mid-answer must not.
func TestIsAuthErrorResult_malformedCredential(t *testing.T) {
	// The paid real-world shape is a whole CLI login transcript quoted
	// back as the header value — comfortably past the 200-byte prose cap,
	// which is exactly why the prefix branch sits ABOVE that cap. The
	// assertion on len pins the property the fixture exists for.
	long := "API Error: Header '14' has invalid value: 'Bearer \x1b[?2004h\x1b[?1004h\x1b[?2031hWelcome to Claude Code v2.1.220\nWelcome to Claude Code v2.1.220\n\n · Opening browser to sign in…\nPaste code here if prompted > \nWelcome to Claude Code v2.1.220\n · Opening browser to sign in…'"
	if len(long) <= 200 {
		t.Fatalf("fixture is %d bytes — it must exceed the 200-byte prose cap to guard the branch ordering", len(long))
	}
	if !isAuthErrorResult(long) {
		t.Fatal("the malformed-Authorization render must classify as an auth failure")
	}
	quoted := "The run failed because the log said: API Error: Header '14' has invalid value: 'Bearer x'. We should investigate the credential store."
	if isAuthErrorResult(quoted) {
		t.Fatal("prose quoting the error mid-answer must not classify")
	}
}

// The evidence write: an auth failure must leave a rejected WindowAuth
// reading behind (through the task hook, so the Source stamp applies)
// before the typed error surfaces — otherwise the next resolution
// re-picks the same dead credential.
func TestAuthFailureFast_recordsEvidence(t *testing.T) {
	var got []usagecap.Reading
	task := Task{}
	task.Hooks.OnUsageWindow = func(r usagecap.Reading) error { got = append(got, r); return nil }

	res := "Failed to authenticate. API Error: 401 Invalid bearer token"
	err := authFailureFast(&res, task, credSpawn{})
	var auth *ErrAuthFailed
	if !errors.As(err, &auth) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	if len(got) != 1 || got[0].Window != usagecap.WindowAuth || got[0].Status != usagecap.StatusRejected {
		t.Fatalf("readings = %+v, want one rejected WindowAuth reading", got)
	}

	ok := "All done, docs aligned."
	if authFailureFast(&ok, task, credSpawn{}) != nil || len(got) != 1 {
		t.Fatal("a normal result must produce neither error nor evidence")
	}
	if authFailureFast(nil, task, credSpawn{}) != nil {
		t.Fatal("a nil result must be a no-op")
	}
}

// R7cac91: the short-garbage half of the malformed-Authorization render —
// a 54-byte result must classify, not panic on a fixed-width slice.
func TestIsAuthErrorResult_shortMalformedCredential(t *testing.T) {
	for _, s := range []string{
		"API Error: Header '14' has invalid value: 'Bearer abc'",
		"API Error: Header '1' has invalid value: 'Bearer '",
	} {
		if !isAuthErrorResult(s) {
			t.Errorf("isAuthErrorResult(%q) = false, want true", s)
		}
	}
}

// Two thresholds, two blast radii: every auth render fails the node, but
// only the CLI's own high-confidence shapes write skip evidence — a terse
// agent answer containing "not logged in" must not bench a healthy
// credential fleet-wide for an hour.
func TestAuthFailureFast_looseSignatureFailsWithoutEvidence(t *testing.T) {
	var got []usagecap.Reading
	task := Task{}
	task.Hooks.OnUsageWindow = func(r usagecap.Reading) error { got = append(got, r); return nil }

	prose := "The deploy step failed: the gh CLI says you are not logged in."
	if authFailureFast(&prose, task, credSpawn{}) == nil {
		t.Fatal("the loose signature must still fail the node fast")
	}
	if len(got) != 0 {
		t.Fatalf("readings = %+v, want NONE — prose must not arm the credential skip", got)
	}

	// The CLI's own "Not logged in" render fails the node but writes no
	// evidence either: it indicts the delivery, not the credential — a
	// healthy forfait shadowed by the pod env produces the same render
	// (claudeForfaitEnv's documented precedent), and benching the
	// credential for it would skip a key that works everywhere else.
	cli := "Not logged in · Please run /login"
	if authFailureFast(&cli, task, credSpawn{}) == nil {
		t.Fatal("the CLI render must fail the node")
	}
	if len(got) != 0 {
		t.Fatalf("readings = %+v, want NONE — 'Not logged in' is delivery-ambiguous", got)
	}

	// A provider verdict on the credential itself does arm the skip.
	verdict := "Failed to authenticate. API Error: 401 Invalid bearer token"
	if authFailureFast(&verdict, task, credSpawn{}) == nil {
		t.Fatal("the provider verdict must fail the node")
	}
	if len(got) != 1 {
		t.Fatalf("readings = %+v, want one — the provider rejected the credential itself", got)
	}
}

// The rejected secret must never reach durable state: the Detail (run
// document, failure events) and the operator log both go through
// redactAuthRender, which drops everything after the identifying prefix.
func TestAuthFailureFast_redactsTheQuotedCredential(t *testing.T) {
	res := "API Error: Header '14' has invalid value: 'Bearer sk-ant-oat01-SECRETSECRETSECRET'"
	err := authFailureFast(&res, Task{}, credSpawn{})
	if err == nil {
		t.Fatal("want an auth failure")
	}
	if msg := err.Error(); strings.Contains(msg, "SECRETSECRET") || strings.Contains(msg, "Bearer sk-") {
		t.Fatalf("Detail leaks the credential: %q", msg)
	}
	if !strings.Contains(err.Error(), "has invalid value: <redacted>") {
		t.Fatalf("Detail should keep the identifying prefix: %q", err.Error())
	}
}

// A node PINNED to a facade provider dies naming that provider and the two
// channels that fund such a route — the legacy wording indicts the forfait and
// the Anthropic key, channels the pin took out of the path. The remediation
// reads the pin's ROUTE, never the pin: the funded half of a facade hint
// stamps the slot, and the unfunded half never reaches a remediation at all —
// facadeHintRefusal turns it into ErrNoFacadeCredential at both spawn sites,
// before any CLI runs, for every spelling of the hint. A node that is neither
// pinned nor routed to a facade keeps the legacy wording: there those ARE the
// channels. Mutation: drop the facadeEnvKey branch in authFailureFast and the
// pinned case reddens on its detail.
func TestAuthFailureFast_FacadePinIsNamedByTheRouteItStamped(t *testing.T) {
	res := "Failed to authenticate. API Error: 401 Invalid bearer token"

	for _, hint := range []string{"zai", "ZAI", " Zai "} {
		t.Run("funded_"+hint, func(t *testing.T) {
			resetClaudeCredEnv(t)
			ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderZAI: "zai-byok"}, nil)
			task := Task{ProviderHint: hint}
			credEnv, selected := anthropicCredRouteForTask(ctx, task)
			if refusal := facadeHintRefusal(task.ProviderHint, credEnv); refusal != nil {
				t.Fatalf("a funded facade pin was refused: %v", refusal)
			}

			err := authFailureFast(&res, task, credSpawnOf(ctx, credEnv, selected, false))
			var auth *ErrAuthFailed
			if !errors.As(err, &auth) {
				t.Fatalf("err = %v, want ErrAuthFailed", err)
			}
			if !strings.Contains(auth.Detail, "no working zai credential — check this run's zai key") {
				t.Errorf("pinned detail = %q, want the provider and the channel that funded the route", auth.Detail)
			}
			if strings.Contains(auth.Detail, "ZAI_API_KEY") {
				t.Errorf("pinned detail names a variable the run's own key took out of the path: %q", auth.Detail)
			}
			if strings.Contains(auth.Detail, "forfait CLAUDE_CODE_OAUTH_TOKEN") {
				t.Errorf("pinned detail still indicts the forfait: %q", auth.Detail)
			}
		})

		// The unfunded half: no remediation is ever worded for it, so the pin
		// has no state left to answer for. Mutation: make facadeHintRefusal
		// return nil and this reddens.
		t.Run("unfunded_"+hint, func(t *testing.T) {
			resetClaudeCredEnv(t)
			ctx := ctxWithCreds(t, nil, nil)
			task := Task{ProviderHint: hint}
			credEnv, selected := anthropicCredRouteForTask(ctx, task)
			var refused *ErrNoFacadeCredential
			if !errors.As(facadeHintRefusal(task.ProviderHint, credEnv), &refused) {
				t.Fatalf("an unfunded facade pin reached the spawn: env = %v", credEnv)
			}
			spawn := credSpawnOf(ctx, credEnv, selected, false)
			if spawn.facadeSlot != "" || len(spawn.ambientChannels) != 0 {
				t.Errorf("the refusal env names a channel: %+v", spawn)
			}
		})
	}

	// Unpinned, and no channel the engine bound: the legacy wording stands.
	err := authFailureFast(&res, Task{}, credSpawn{})
	var auth *ErrAuthFailed
	if !errors.As(err, &auth) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	if !strings.Contains(auth.Detail, "check the forfait CLAUDE_CODE_OAUTH_TOKEN or the Anthropic API key") {
		t.Errorf("unpinned detail = %q, want the legacy wording", auth.Detail)
	}
}

// The remediation of a route the engine did not fund itself: the operator's
// own ambient credential variables — forwarded into a sandboxed spawn
// (ambientAnthropicEnvForSandbox) or inherited by a host one (hostSpawnEnv) —
// carry no facade slot, because a gateway's base URL names no vendor. Naming
// the forfait and the Anthropic key there is #1527's failure mode on the wiring
// z.ai and Moonshot document, and on a sandboxed spawn those two are not even
// in the env.
//
// Every channel the spawn carried is named, not just the one that ranks first:
// claw's ResolveStartupAuth sends x-api-key AND Authorization when a key and a
// bearer are both set, so the render's own verdict can indict either, and an
// exclusive answer suppresses it. Every route shape of
// selectedAnthropicCredEnvForCLI is a row, each with the ambient bearer
// exported: a route the engine BOUND keeps its own wording, since the credential
// is the run's and no variable of the operator's holds it.
//
// Mutations: ambientCredChannels returning nil, its process-env half returning
// "" on a host spawn or reading the host on a sandboxed one, its
// cleared-channel read made non-empty-only, either veto dropped, the extra
// channels dropped from the list, the header predicate made value-wide,
// spawnCloudProviderSwitches reading a switch as on whatever its value, the
// record's authorizationHeader or customHeaders field left false, the header
// clause's cloud arm dropped, the ambiguous clause's consequence left strong
// beside a mode really on — each reddens its own row.
func TestAuthFailureFast_AmbientChannelsNameWhatTheSpawnCarried(t *testing.T) {
	const bearerWording = "no working ANTHROPIC_AUTH_TOKEN credential — check that variable"
	const legacyWording = "check the forfait CLAUDE_CODE_OAUTH_TOKEN or the Anthropic API key"
	res := "Failed to authenticate. API Error: 401 Invalid bearer token"

	cases := []struct {
		name         string
		sandboxed    bool
		hint         string
		byok         map[secrets.Provider]string
		pinned       map[secrets.Provider]string
		forfait      bool
		forfaitEmpty bool
		switchOn     string
		extraEnv     []string
		env          map[string]string
		want         string
		wantNot      []string
	}{
		// Nothing these readings see is in the path: an unbound route, no
		// credential of the run, every ambient variable empty. The CLI
		// authenticated from something no reading here covers — the forfait
		// file under $HOME/.claude, whose expiry renders a failure
		// TestIsAuthErrorResult pins — so the disjunction names the channel to
		// ARM and no clause rides it.
		{name: "nothing_the_record_can_read_is_in_the_path",
			env:  map[string]string{"ANTHROPIC_AUTH_TOKEN": "", "ANTHROPIC_BASE_URL": ""},
			want: legacyWording,
			wantNot: []string{"no working", "ANTHROPIC_CUSTOM_HEADERS",
				"set to a value other than", "is in this spawn's path"}},
		{name: "ambient_forwarded_into_a_sandboxed_spawn", sandboxed: true, want: bearerWording},
		{name: "ambient_inherited_by_a_host_spawn", want: bearerWording},
		{name: "node_env_bearer_on_an_unbound_route", extraEnv: []string{"ANTHROPIC_AUTH_TOKEN=node-env-key"}, want: bearerWording},
		// The CLI reads a cloud-provider switch BEFORE the bearer: the token is
		// in the env and not in the request, and the credential that answered
		// is the cloud account's — neither the bearer nor the forfait.
		{name: "a_cloud_switch_outranks_the_bearer", switchOn: "CLAUDE_CODE_USE_BEDROCK",
			want:    "no working credential on the CLAUDE_CODE_USE_BEDROCK route",
			wantNot: []string{legacyWording, "ANTHROPIC_AUTH_TOKEN"}},
		// ... on the env the spawn really gets. A host switch is not among the
		// four names forwarded into a container, so it rules nothing there.
		{name: "a_host_switch_does_not_reach_a_sandboxed_spawn", sandboxed: true, switchOn: "CLAUDE_CODE_USE_BEDROCK",
			want: bearerWording, wantNot: []string{legacyWording, "CLAUDE_CODE_USE_BEDROCK route"}},
		{name: "a_switch_the_container_gets_outranks_the_bearer", sandboxed: true,
			extraEnv: []string{"CLAUDE_CODE_USE_BEDROCK=1"},
			want:     "no working credential on the CLAUDE_CODE_USE_BEDROCK route",
			wantNot:  []string{legacyWording, "ANTHROPIC_AUTH_TOKEN"}},
		// A switch set to anything else claims NO mode: claw enters it on "1"
		// alone, so the bearer is what it sent — while a reader testing the
		// variable's presence would have spent a cloud account. Which binary
		// ran is not this record's fact, so the operator is sent to both.
		{name: "a_switch_at_a_value_the_cli_may_not_read_as_on", env: map[string]string{"CLAUDE_CODE_USE_BEDROCK": "0"},
			want:    bearerWording + ` — and CLAUDE_CODE_USE_BEDROCK is set to a value other than "1", the one that enters that mode, so a CLI reading it as on spent a cloud account instead`,
			wantNot: []string{legacyWording, "no working credential on the CLAUDE_CODE_USE_BEDROCK route"}},
		// Plural: three names cannot share one "that mode", and no mode being
		// on keeps the strong consequence.
		{name: "two_switches_at_a_value_the_cli_may_not_read_as_on",
			env:     map[string]string{"CLAUDE_CODE_USE_BEDROCK": "0", "CLAUDE_CODE_USE_VERTEX": "true"},
			want:    `CLAUDE_CODE_USE_BEDROCK, CLAUDE_CODE_USE_VERTEX are set to a value other than "1", the one that enters those modes, so a CLI reading it as on spent a cloud account instead`,
			wantNot: []string{legacyWording, "no working credential on the CLAUDE_CODE_USE_BEDROCK", "enters that mode"}},
		// One of each: the mode the CLI really entered leads, the ambiguous
		// one rides beside it — and the consequence is a different ACCOUNT,
		// not a different kind of credential: a cloud account was spent either
		// way, so "instead" would be false here.
		{name: "a_switch_on_beside_one_at_another_value",
			env:     map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "CLAUDE_CODE_USE_VERTEX": "0"},
			want:    `not with an Anthropic variable — and CLAUDE_CODE_USE_VERTEX is set to a value other than "1", the one that enters that mode, so a CLI reading it as on may have spent a different cloud account`,
			wantNot: []string{legacyWording, "ANTHROPIC_AUTH_TOKEN", "spent a cloud account instead"}},
		// A cloud route beside the operator's own Authorization line: the
		// variable is named, and the override is attributed to a CLI that
		// applies it there rather than asserted — claw's vertex and foundry
		// providers resolve the identity from the environment, its bedrock one
		// never reads the variable, and the sentence must not contradict its
		// own lead.
		{name: "a_cloud_route_beside_an_ambient_authorization_line", switchOn: "CLAUDE_CODE_USE_BEDROCK",
			env:  map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer sk-operator-gateway"},
			want: "not with an Anthropic variable — and ANTHROPIC_CUSTOM_HEADERS carries an Authorization line, which a CLI that applies it on that route sends in place of the mode's own credential",
			wantNot: []string{legacyWording, "sk-operator-gateway",
				"applies over whatever credential this route resolved"}},
		{name: "a_cloud_route_beside_a_gateway_token_header", switchOn: "CLAUDE_CODE_USE_VERTEX",
			env:     map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "x-gateway-token: tok-dead"},
			want:    "not with an Anthropic variable — and ANTHROPIC_CUSTOM_HEADERS is in this spawn's path",
			wantNot: []string{legacyWording, "tok-dead", "carries an Authorization line"}},
		// The weak arm on the route whose known reader does NOT read the
		// variable: one text for every route, since the mechanism it states is
		// conditioned on a CLI reading it — claw's bedrock provider never does
		// — and it claims nothing about the mode's own credential being
		// replaced. The unconditioned form is what this row forbids.
		{name: "a_bedrock_route_beside_a_gateway_token_header", switchOn: "CLAUDE_CODE_USE_BEDROCK",
			env:  map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "x-gateway-token: tok-dead"},
			want: "not with an Anthropic variable — and ANTHROPIC_CUSTOM_HEADERS is in this spawn's path, carrying lines a CLI that reads it applies over the headers its own auth set, so check it for a credential of its own",
			wantNot: []string{legacyWording, "tok-dead", "carries an Authorization line",
				"carrying lines a CLI applies"}},
		// Read on the env the spawn really gets, this half too: a host switch
		// at any value is not among the four names forwarded into a container.
		{name: "a_host_switch_at_another_value_does_not_reach_a_sandboxed_spawn", sandboxed: true,
			env:  map[string]string{"CLAUDE_CODE_USE_BEDROCK": "0"},
			want: bearerWording, wantNot: []string{legacyWording, "set to a value other than"}},
		// A bound route takes the switches out of the spawn itself — the
		// composition clears every channel a direct credential owns
		// (anthropicCredRouteForTask), the helper's own map not being the
		// whole route — so the forfait really is what answered, and the
		// cloud-mode wording must NOT fire there.
		{name: "a_bound_forfait_clears_a_host_switch", forfait: true, switchOn: "CLAUDE_CODE_USE_VERTEX",
			want: legacyWording, wantNot: []string{"no working"}},
		// Several ambient channels at once: the request carries more than the
		// first, so the operator is sent to all of them.
		{name: "a_bearer_beside_an_ambient_anthropic_key", env: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-dead"},
			want:    "no working credential among the channels this spawn carried — check ANTHROPIC_AUTH_TOKEN, ANTHROPIC_API_KEY",
			wantNot: []string{legacyWording}},
		{name: "a_bearer_beside_an_ambient_forfait", env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-expired"},
			want:    "no working credential among the channels this spawn carried — check ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN",
			wantNot: []string{legacyWording}},
		{name: "a_bearer_beside_an_ambient_forfait_forwarded_into_a_sandbox", sandboxed: true,
			env:     map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-expired"},
			want:    "check ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN",
			wantNot: []string{legacyWording}},
		// An Authorization line in ANTHROPIC_CUSTOM_HEADERS is a credential of
		// its own: the CLI applies it over the header its own auth set. Where
		// the ambient reading names the variable, the override clause stands
		// down — one sentence per channel, never the same name twice.
		// Both arms of the clause stand down there, the weak one included: the
		// value that carries an Authorization line is non-empty, so dropping
		// the weak arm's own guard would append its text under the name the
		// ambient reading just used.
		{name: "an_authorization_line_in_the_custom_headers", env: map[string]string{
			"ANTHROPIC_AUTH_TOKEN": "", "ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer sk-operator-gateway"},
			want: "no working ANTHROPIC_CUSTOM_HEADERS credential — check that variable",
			wantNot: []string{legacyWording, "sk-operator-gateway",
				"applies over whatever credential", "is in this spawn's path"}},
		{name: "the_bearer_beside_an_authorization_line", env: map[string]string{
			"ANTHROPIC_CUSTOM_HEADERS": "x-tenant: t1\nAuthorization: Bearer sk-operator-gateway"},
			want: "check ANTHROPIC_AUTH_TOKEN, ANTHROPIC_CUSTOM_HEADERS",
			wantNot: []string{"sk-operator-gateway",
				"applies over whatever credential", "is in this spawn's path"}},
		// Only the header NAME makes it a credential CHANNEL — a value
		// mentioning the word does not — so the variable keeps the weak claim
		// there: named as something in the spawn's path, with nothing said
		// about what it holds and nothing of its contents printed.
		{name: "custom_headers_without_an_authorization_line", env: map[string]string{
			"ANTHROPIC_AUTH_TOKEN": "", "ANTHROPIC_CUSTOM_HEADERS": "x-note: see the authorization: guide"},
			want: legacyWording + " — and ANTHROPIC_CUSTOM_HEADERS is in this spawn's path",
			wantNot: []string{"no working ANTHROPIC_CUSTOM_HEADERS credential",
				"applies over whatever credential", "x-note", "see the authorization"}},
		// The gateway whose ONLY credential is a header of its own: no
		// credential VARIABLE is set, so the legacy disjunction named two
		// channels both empty in that spawn while the variable that really
		// carried the refused credential was named nowhere. The override the
		// CLI performs is name-agnostic (claw's identity.Apply, h.Set per
		// name), so the variable is named whatever name holds the credential.
		{name: "a_gateway_authenticated_by_an_x_api_key_header", env: map[string]string{
			"ANTHROPIC_AUTH_TOKEN": "", "ANTHROPIC_CUSTOM_HEADERS": "x-api-key: sk-gateway-dead"},
			want:    "ANTHROPIC_CUSTOM_HEADERS is in this spawn's path, carrying lines a CLI that reads it applies over the headers its own auth set, so check it for a credential of its own",
			wantNot: []string{"sk-gateway-dead", "no working ANTHROPIC_CUSTOM_HEADERS credential"}},
		{name: "a_gateway_authenticated_by_a_token_header_of_its_own", env: map[string]string{
			"ANTHROPIC_AUTH_TOKEN": "", "ANTHROPIC_CUSTOM_HEADERS": "x-gateway-token: tok-dead"},
			want:    "ANTHROPIC_CUSTOM_HEADERS is in this spawn's path",
			wantNot: []string{"tok-dead", "no working ANTHROPIC_CUSTOM_HEADERS credential"}},
		// Blank is empty, as it is for the CLI's own parser: a variable set to
		// whitespace puts no header in the request, so it is named nowhere.
		{name: "custom_headers_set_to_whitespace_carry_nothing", env: map[string]string{
			"ANTHROPIC_AUTH_TOKEN": "", "ANTHROPIC_CUSTOM_HEADERS": "  \n\t"},
			want: legacyWording, wantNot: []string{"ANTHROPIC_CUSTOM_HEADERS"}},
		// Proxy-Authorization is NOT the name the CLI's own auth uses, so it
		// gets the weak claim too — the strong one would assert an override of
		// the route's credential that this header does not perform.
		{name: "a_gateway_authenticated_by_a_proxy_authorization_header", env: map[string]string{
			"ANTHROPIC_AUTH_TOKEN": "", "ANTHROPIC_CUSTOM_HEADERS": "Proxy-Authorization: Bearer tok-dead"},
			want:    "ANTHROPIC_CUSTOM_HEADERS is in this spawn's path",
			wantNot: []string{"tok-dead", "applies over whatever credential", "carries an Authorization line"}},
		// The same weak claim rides a bound route: there the lead names the
		// run's own credential, and the operator's header is the one thing in
		// the path the rest of the sentence cannot make them suspect.
		{name: "a_bound_key_beside_a_gateway_token_header",
			byok:    map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant"},
			env:     map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "x-gateway-token: tok-dead"},
			want:    legacyWording + " — and ANTHROPIC_CUSTOM_HEADERS is in this spawn's path",
			wantNot: []string{"no working", "tok-dead"}},
		// Both sandbox halves of that reading, like the Authorization line's:
		// a container carrying the variable itself names it, a container that
		// does not never hears of the host's.
		{name: "a_sandboxed_bound_route_whose_container_carries_other_headers", sandboxed: true,
			byok:     map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant"},
			extraEnv: []string{"ANTHROPIC_CUSTOM_HEADERS=x-gateway-token: tok-container"},
			want:     legacyWording + " — and ANTHROPIC_CUSTOM_HEADERS is in this spawn's path",
			wantNot:  []string{"no working", "tok-container"}},
		{name: "a_sandboxed_spawn_does_not_inherit_the_hosts_other_headers", sandboxed: true,
			byok: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant"},
			env:  map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "x-gateway-token: tok-host"},
			want: legacyWording, wantNot: []string{"no working", "ANTHROPIC_CUSTOM_HEADERS"}},
		// The bound routes: the engine's map clears the bearer, so the
		// channels the legacy wording names ARE the ones that ran — and an
		// operator variable must NOT be named beside them, since the
		// credential the CLI holds came from the run.
		{name: "a_bound_anthropic_key_clears_the_bearer", byok: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant"},
			want: legacyWording, wantNot: []string{"no working"}},
		// ... with one exception, which is the point of the clause: an
		// Authorization line in the operator's own ANTHROPIC_CUSTOM_HEADERS is
		// applied by the CLI OVER the credential the engine bound (claw's
		// identity.Apply runs last, h.Set per header), so the bound route's
		// own wording would send the operator to re-arm a healthy forfait.
		{name: "a_bound_key_beside_an_ambient_authorization_line",
			byok:    map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant"},
			env:     map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer sk-operator-gateway"},
			want:    legacyWording + " — and ANTHROPIC_CUSTOM_HEADERS carries an Authorization line",
			wantNot: []string{"no working", "sk-operator-gateway"}},
		{name: "a_bound_forfait_beside_an_ambient_authorization_line", forfait: true,
			env:     map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer sk-operator-gateway"},
			want:    legacyWording + " — and ANTHROPIC_CUSTOM_HEADERS carries an Authorization line",
			wantNot: []string{"no working", "sk-operator-gateway"}},
		// The sandbox half of that class, both ways: a container carrying the
		// header itself names it, a container that does not never hears of the
		// host's — the four names forwarded into one do not include it.
		{name: "a_sandboxed_bound_route_whose_container_carries_the_line", sandboxed: true,
			byok:     map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant"},
			extraEnv: []string{"ANTHROPIC_CUSTOM_HEADERS=Authorization: Bearer sk-container-gateway"},
			want:     legacyWording + " — and ANTHROPIC_CUSTOM_HEADERS carries an Authorization line",
			wantNot:  []string{"no working", "sk-container-gateway"}},
		{name: "a_sandboxed_spawn_does_not_inherit_the_hosts_authorization_line", sandboxed: true,
			byok: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant"},
			env:  map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer sk-operator-gateway"},
			want: legacyWording, wantNot: []string{"no working", "ANTHROPIC_CUSTOM_HEADERS"}},
		{name: "a_bound_forfait_clears_the_bearer", forfait: true, want: legacyWording, wantNot: []string{"no working"}},
		{name: "a_bound_forfait_whose_file_carries_no_token", forfait: true, forfaitEmpty: true,
			want: legacyWording, wantNot: []string{"no working"}},
		{name: "an_explicit_anthropic_pin_clears_the_bearer", hint: "anthropic", want: legacyWording, wantNot: []string{"no working"}},
		// An explicit pin that binds nothing inherits the ambient env: there
		// the variables ARE the route, and the bearer is the one it cleared.
		{name: "an_anthropic_pin_inheriting_an_ambient_key", hint: "anthropic",
			env:     map[string]string{"ANTHROPIC_API_KEY": "sk-ant-ambient"},
			want:    "no working ANTHROPIC_API_KEY credential — check that variable",
			wantNot: []string{legacyWording, "ANTHROPIC_AUTH_TOKEN"}},
		// A funded facade outranks the ambient reading: its slot names the
		// vendor whose key died, which a variable cannot.
		{name: "a_facade_route_names_its_vendor", byok: map[secrets.Provider]string{secrets.ProviderZAI: "zai-byok"},
			want: "no working zai credential — check this run's zai key", wantNot: []string{"ZAI_API_KEY"}},
		// Third funding channel of a facade route: a key a shared tier pinned
		// BECAUSE the node names the provider. It comes with the run, so the
		// provider's variable is as inert there as a run key makes it.
		{name: "a_facade_route_funded_by_a_tiers_pinned_key", hint: "zai",
			pinned: map[secrets.Provider]string{secrets.ProviderZAI: "tier-zai"},
			want:   "no working zai credential — check this run's zai key", wantNot: []string{"ZAI_API_KEY"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeCredEnv(t)
			for _, k := range cloudProviderSwitches {
				t.Setenv(k, "")
			}
			t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example.test/anthropic")
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-gateway-key")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if tc.switchOn != "" {
				t.Setenv(tc.switchOn, "1")
			}
			ctx := context.Background()
			if tc.byok != nil || tc.pinned != nil || tc.forfait {
				var dirs map[string]string
				if tc.forfait {
					dir := t.TempDir()
					if tc.forfaitEmpty {
						writeForfait(t, dir, "")
					} else {
						writeForfait(t, dir, "at.live")
					}
					dirs = map[string]string{string(secrets.OAuthKindClaudeCode): dir}
				}
				ctx = secrets.WithCredentials(ctx, secrets.Credentials{
					APIKeys:              tc.byok,
					PinnedAPIKeys:        tc.pinned,
					OAuthCredentialFiles: dirs,
				})
			}
			task := Task{ProviderHint: tc.hint, ExtraEnv: tc.extraEnv}
			if tc.sandboxed {
				task.Sandbox = k8sLikeRun{}
			}
			credEnv, selected := anthropicCredRouteForTask(ctx, task)

			err := authFailureFast(&res, task, credSpawnOf(ctx, credEnv, selected, taskSandboxed(task)))
			var auth *ErrAuthFailed
			if !errors.As(err, &auth) {
				t.Fatalf("err = %v, want ErrAuthFailed", err)
			}
			if !strings.Contains(auth.Detail, tc.want) {
				t.Errorf("detail = %q, want %q named (spawn env: %v)", auth.Detail, tc.want, credEnv)
			}
			for _, unwanted := range tc.wantNot {
				if strings.Contains(auth.Detail, unwanted) {
					t.Errorf("detail = %q names %q, which this route did not run on (spawn env: %v)",
						auth.Detail, unwanted, credEnv)
				}
			}
			// A SANDBOXED spawn may only be sent to a variable the container
			// itself carries: the host env is not its fallback, so a name read
			// off this process would be an answer about an env the CLI never
			// saw. The legacy LEAD is exempt, clauses riding it included —
			// that wording names channels as the run's credential, not as
			// variables to check, and each clause carries its own row for the
			// container env (the pair above).
			if tc.sandboxed && !strings.HasPrefix(tc.want, legacyWording) {
				for _, k := range append(append([]string{}, ambientCredentialChannels...), "ANTHROPIC_CUSTOM_HEADERS") {
					if strings.Contains(auth.Detail, k) && credEnv[k] == "" {
						t.Errorf("detail = %q names %s, which the container env does not carry: %v",
							auth.Detail, k, credEnv)
					}
				}
			}
		})
	}
}

// facadeRouteCase is one way a spawn ends up on an anthropic-wire facade with
// no `provider:` pin at all: the default precedence over the run's own keys,
// and the ZAI_API_KEY shortcut. Both are the UNPINNED half of the class — the
// hint is empty, so a remediation read from the hint names the forfait and the
// Anthropic key, two channels facadeRouteEnv cleared out of the route.
// wantChannel is the ONE channel that funded the route, the only one the
// remediation may name: the other is inert there, and sending the operator to
// it costs a cycle on a credential nothing reads (facadeKeyFromProcessEnv).
type facadeRouteCase struct {
	name        string
	byok        map[secrets.Provider]string
	envKey      string
	envVal      string
	wantSlot    string
	wantChannel string
	wantNot     string
}

var unpinnedFacadeRoutes = []facadeRouteCase{
	{"default_precedence_zai", map[secrets.Provider]string{secrets.ProviderZAI: "zai-byok"}, "", "", "zai", "this run's zai key", "ZAI_API_KEY"},
	{"default_precedence_moonshot", map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-byok"}, "", "", "moonshot", "this run's moonshot key", "MOONSHOT_API_KEY"},
	{"zai_api_key_shortcut", nil, "ZAI_API_KEY", "zai-env", "zai", "ZAI_API_KEY", "this run's"},
}

// spawnOnRoute resolves rt through the real credential resolver and returns the
// spawn record built from the env the CLI would receive — never a hand-built
// map: the slot stamp is the producer's (facadeRouteEnv), and a fixture that
// writes it itself proves nothing about the route.
func spawnOnRoute(t *testing.T, rt facadeRouteCase) credSpawn {
	t.Helper()
	resetClaudeCredEnv(t)
	if rt.envKey != "" {
		t.Setenv(rt.envKey, rt.envVal)
	}
	ctx := context.Background()
	if rt.byok != nil {
		ctx = ctxWithCreds(t, rt.byok, nil)
	}
	credEnv := anthropicCredEnvForCLI(ctx, "", false)
	if credEnv["ANTHROPIC_AUTH_TOKEN"] == "" {
		t.Fatalf("%s did not route to a facade: env = %v", rt.name, credEnv)
	}
	selected, _ := selectedAnthropicCredEnvForCLI(ctx, "", false)
	return credSpawnOf(ctx, credEnv, selected, false)
}

// The UNPINNED-funded residual of #1527: a facade route reached without a
// `provider:` hint whose key dies. The remediation must name the channel the
// spawn ACTUALLY ran on — the slot facadeRouteEnv stamped into its env — and
// never the forfait or the Anthropic key, which that same route cleared.
// Mutation: read task.ProviderHint instead of spawn.facadeSlot and every row
// reddens on its detail.
func TestAuthFailureFast_UnpinnedFacadeRouteNamesTheChannelThatRan(t *testing.T) {
	for _, rt := range unpinnedFacadeRoutes {
		t.Run(rt.name, func(t *testing.T) {
			spawn := spawnOnRoute(t, rt)

			// No pin: the hint a hint-only reading would use is empty, and
			// the route is known only from the spawn's env.
			task := Task{}
			got := authEvidenceRecorder(&task)

			res := "Failed to authenticate. API Error: 401 Invalid bearer token"
			err := authFailureFast(&res, task, spawn)
			var auth *ErrAuthFailed
			if !errors.As(err, &auth) {
				t.Fatalf("err = %v, want ErrAuthFailed", err)
			}
			if !strings.Contains(auth.Detail, "no working "+rt.wantSlot+" credential") || !strings.Contains(auth.Detail, rt.wantChannel) {
				t.Errorf("detail = %q, want the %s route and %s named", auth.Detail, rt.wantSlot, rt.wantChannel)
			}
			if strings.Contains(auth.Detail, rt.wantNot) {
				t.Errorf("detail = %q names %q, which funded nothing on this route", auth.Detail, rt.wantNot)
			}
			if strings.Contains(auth.Detail, "forfait CLAUDE_CODE_OAUTH_TOKEN") || strings.Contains(auth.Detail, "Anthropic API key") {
				t.Errorf("detail indicts channels the facade route cleared: %q", auth.Detail)
			}
			// The wording moved; the meter did not. A provider verdict on the
			// credential still files exactly one rejected auth reading.
			if len(*got) != 1 || (*got)[0].Window != usagecap.WindowAuth || (*got)[0].Status != usagecap.StatusRejected {
				t.Fatalf("readings = %+v, want one rejected WindowAuth reading", *got)
			}

			// The second threshold is untouched too: a delivery-ambiguous
			// render fails the node with the same route wording and benches
			// nothing. Same task as above, whose hook the count of 1 just
			// proved live — a fresh one would read "none filed" from a hook
			// that never fired.
			cli := "Not logged in · Please run /login"
			err2 := authFailureFast(&cli, task, spawn)
			var auth2 *ErrAuthFailed
			if !errors.As(err2, &auth2) {
				t.Fatalf("err = %v, want ErrAuthFailed", err2)
			}
			if !strings.Contains(auth2.Detail, rt.wantChannel) {
				t.Errorf("detail = %q, want %s named", auth2.Detail, rt.wantChannel)
			}
			if len(*got) != 1 {
				t.Fatalf("readings = %+v, want still one — 'Not logged in' is delivery-ambiguous", *got)
			}
		})
	}
}

// The stamp has to survive the trip: credSpawnOf reads the slot off the env
// the CLI receives, for every route that reaches a facade — pinned or not —
// and leaves it empty for the routes that reach Anthropic itself, where there
// is no vendor variable to name. Mutation: stop reading FacadeSlotEnvKey in
// credSpawnOf and every facade row reddens.
func TestCredSpawnOf_CarriesTheFacadeSlotOfEveryFacadeRoute(t *testing.T) {
	type routeCase struct {
		name        string
		hint        string
		byok        map[secrets.Provider]string
		oauth       bool
		envKey      string
		envVal      string
		wantSlot    string
		wantFromEnv bool
	}
	cases := []routeCase{
		{"hint_zai", "zai", map[secrets.Provider]string{secrets.ProviderZAI: "zai-byok"}, false, "", "", "zai", false},
		{"hint_moonshot", "moonshot", map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-byok"}, false, "", "", "moonshot", false},
		{"default_precedence_zai", "", map[secrets.Provider]string{secrets.ProviderZAI: "zai-byok"}, false, "", "", "zai", false},
		{"default_precedence_moonshot", "", map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-byok"}, false, "", "", "moonshot", false},
		{"zai_api_key_shortcut", "", nil, false, "ZAI_API_KEY", "zai-env", "zai", true},
		{"hint_moonshot_from_the_process_env", "moonshot", nil, false, "MOONSHOT_API_KEY", "moonshot-env", "moonshot", true},
		// The run's own key wins the tie where both channels hold it: that is
		// the resolver's order, and the variable funds nothing there.
		{"a_run_key_the_variable_also_holds", "zai", map[secrets.Provider]string{secrets.ProviderZAI: "same-key"}, false, "ZAI_API_KEY", "same-key", "zai", false},
		{"anthropic_key", "", map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant"}, false, "", "", "", false},
		{"forfait", "", nil, true, "", "", "", false},
	}
	for _, rc := range cases {
		t.Run(rc.name, func(t *testing.T) {
			resetClaudeCredEnv(t)
			if rc.envKey != "" {
				t.Setenv(rc.envKey, rc.envVal)
			}
			ctx := context.Background()
			var oauthDirs map[string]string
			if rc.oauth {
				dir := t.TempDir()
				writeForfait(t, dir, "at.live")
				oauthDirs = map[string]string{string(secrets.OAuthKindClaudeCode): dir}
			}
			if rc.byok != nil || oauthDirs != nil {
				ctx = ctxWithCreds(t, rc.byok, oauthDirs)
			}

			selected, _ := selectedAnthropicCredEnvForCLI(ctx, rc.hint, false)
			spawn := credSpawnOf(ctx, anthropicCredEnvForCLI(ctx, rc.hint, false), selected, false)

			if spawn.facadeSlot != rc.wantSlot {
				t.Errorf("facadeSlot = %q, want %q", spawn.facadeSlot, rc.wantSlot)
			}
			// The forfait fields answer to their own route, independently —
			// renewedWithin needs both, so both are asserted.
			if rc.oauth && (spawn.token == "" || spawn.dir == "") {
				t.Errorf("the forfait route must still carry its token and dir, got token=%q dir=%q", spawn.token, spawn.dir)
			}
			if rc.wantSlot != "" && spawn.token != "" {
				t.Errorf("a facade route must carry no forfait token, got %q", spawn.token)
			}
			// A route that is neither a facade nor the forfait leaves the
			// record zero: there is no channel to name, and the remediation
			// keeps the legacy wording.
			if rc.wantSlot == "" && !rc.oauth && (spawn.token != "" || spawn.dir != "") {
				t.Errorf("record = %+v, want zero on an Anthropic-direct route", spawn)
			}
			// Which of the two channels funded the route is a fact of its own:
			// it decides the one the remediation names, and the other is inert
			// there. Mutation: drop the APIKeyForRoute tie-break and
			// a_run_key_the_variable_also_holds reddens; drop the value
			// comparison and every byok row does.
			if spawn.facadeKeyFromEnv != rc.wantFromEnv {
				t.Errorf("facadeKeyFromEnv = %v, want %v on this route", spawn.facadeKeyFromEnv, rc.wantFromEnv)
			}
			// No ambient variable is set here, and every route of this table is
			// one the ENGINE bound: the operator's channel list is empty, so a
			// facade's own bearer is never offered as a variable to check.
			if len(spawn.ambientChannels) != 0 {
				t.Errorf("ambientChannels = %v, want none: the engine bound this route itself", spawn.ambientChannels)
			}
		})
	}
}

// The remediation is exercised from the caller that RENDERS it. The record it
// reads is built elsewhere — setupCredsAndSession and formatOutput — so this
// one covers the wording, and TestExecute_authRemediationNamesTheChannelThe
// SpawnRanOn covers the two producers. Mutation: pass credSpawn{} at
// claude_code.go's authFailureFast call and this reddens.
func TestRenderedFailure_NamesTheFacadeChannelOfTheSpawn(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
	task := Task{NodeID: "n", Iteration: 1}
	render := "Failed to authenticate. API Error: 401 Invalid bearer token"
	rm := &claudesdk.ResultMessage{Result: &render}

	for _, pass := range []string{"pass 1", "formatting pass 1/2", "recovery formatting pass"} {
		err := b.renderedFailure(context.Background(), rm, task, pass, credSpawn{facadeSlot: string(secrets.ProviderZAI)})
		var auth *ErrAuthFailed
		if !errors.As(err, &auth) {
			t.Fatalf("%s: err = %v, want ErrAuthFailed", pass, err)
		}
		if !strings.Contains(auth.Detail, "no working zai credential — check this run's zai key") {
			t.Errorf("%s: detail = %q, want the zai route and the channel that funded it", pass, auth.Detail)
		}
		// The same record funded by the provider's variable names THAT one.
		fromEnv := credSpawn{facadeSlot: string(secrets.ProviderZAI), facadeKeyFromEnv: true}
		err = b.renderedFailure(context.Background(), rm, task, pass, fromEnv)
		if !errors.As(err, &auth) {
			t.Fatalf("%s: err = %v, want ErrAuthFailed", pass, err)
		}
		if !strings.Contains(auth.Detail, "no working zai credential — check ZAI_API_KEY") {
			t.Errorf("%s: detail = %q, want ZAI_API_KEY named", pass, auth.Detail)
		}
	}
}

// fakeClaudeAuthRender stands in for the claude CLI: it records the credential
// channels the env it was handed actually carried, then prints the line its
// spawn number is given — the provider's render of a dead credential.
const fakeClaudeAuthRender = `#!/bin/sh
case "$*" in *--input-format*)
	while read -r line; do
		case "$line" in *'"type":"user"'*) break ;; esac
	done ;;
esac
n=$(( $(cat "$AUTHDUMP/count" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$AUTHDUMP/count"
printf 'spawn %s slot=%s bearer=%s key=%s forfait=%s bedrock=[%s] headers=[%s]\n' \
	"$n" "$ITERION_FACADE_SLOT" "$ANTHROPIC_AUTH_TOKEN" "$ANTHROPIC_API_KEY" "$CLAUDE_CODE_OAUTH_TOKEN" \
	"$CLAUDE_CODE_USE_BEDROCK" "$ANTHROPIC_CUSTOM_HEADERS" >> "$AUTHDUMP/env"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1","model":"fake","tools":[],"mcp_servers":[]}'
cat "$AUTHDUMP/$n"
`

// runAuthRender runs the REAL Execute against that CLI — spawns maps a spawn
// number to the result line it prints — and returns what the child env held
// beside the typed failure.
func runAuthRender(t *testing.T, ctx context.Context, task Task, spawns map[int][]byte) (string, *[]usagecap.Reading, error) {
	t.Helper()
	dump := t.TempDir()
	for n, line := range spawns {
		if err := os.WriteFile(filepath.Join(dump, fmt.Sprint(n)), line, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(script, []byte(fakeClaudeAuthRender), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTHDUMP", dump)
	task.NodeID, task.Iteration = "n", 1
	task.Command, task.WorkDir, task.UserPrompt = script, t.TempDir(), "x"
	got := authEvidenceRecorder(&task)
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	_, err := b.Execute(runCtx, task)
	env, _ := os.ReadFile(filepath.Join(dump, "env"))
	return string(env), got, err
}

// The end-to-end witness of #1527: the record the remediation reads is built
// by setupCredsAndSession (pass 1) and formatOutput (pass 2), and nothing
// below Execute proves either carries the route — drop the facade slot at both
// and every helper test stays green. So the channel is pinned from the resolver
// that routes the spawn to the operator's detail, against a CLI that renders
// the 401 and dumps the credential env it received.
//
// Mutation: clear facadeSlot (or gatewayAuth) on the record either producer
// returns, and the row for that route reddens on its detail.
func TestExecute_authRemediationNamesTheChannelTheSpawnRanOn(t *testing.T) {
	const authRender = "Failed to authenticate. API Error: 401 Invalid bearer token"
	cases := []struct {
		name    string
		hint    string
		byok    map[secrets.Provider]string
		env     map[string]string
		render  string // the CLI's own verdict, when the row is about which channel it names
		want    string
		wantNot string
		wantEnv string // a substring of the child env dump
		twoPass bool
	}{
		{name: "zai_api_key_shortcut", env: map[string]string{"ZAI_API_KEY": "zai-env"},
			want:    "no working zai credential — check ZAI_API_KEY",
			wantNot: "forfait CLAUDE_CODE_OAUTH_TOKEN", wantEnv: "slot=zai bearer=zai-env"},
		{name: "default_precedence_zai", byok: map[secrets.Provider]string{secrets.ProviderZAI: "zai-byok"},
			want:    "no working zai credential — check this run's zai key",
			wantNot: "ZAI_API_KEY", wantEnv: "slot=zai bearer=zai-byok"},
		{name: "default_precedence_moonshot", byok: map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-byok"},
			want:    "no working moonshot credential — check this run's moonshot key",
			wantNot: "MOONSHOT_API_KEY", wantEnv: "slot=moonshot bearer=moonshot-byok"},
		{name: "pinned_zai", hint: "zai", byok: map[secrets.Provider]string{secrets.ProviderZAI: "zai-byok"},
			want:    "no working zai credential — check this run's zai key",
			wantNot: "forfait CLAUDE_CODE_OAUTH_TOKEN", wantEnv: "slot=zai bearer=zai-byok"},
		// The operator's own gateway, wired the way z.ai documents it: the
		// engine routes nothing, the host spawn inherits the bearer, and the
		// two channels the legacy wording names are empty in the child env.
		{name: "ambient_host_gateway", env: map[string]string{
			"ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic", "ANTHROPIC_AUTH_TOKEN": "host-gateway-key", "ZAI_API_KEY": "zai-env"},
			want:    "no working ANTHROPIC_AUTH_TOKEN credential — check that variable",
			wantNot: "forfait CLAUDE_CODE_OAUTH_TOKEN", wantEnv: "slot= bearer=host-gateway-key key= forfait="},
		// The same gateway on a host that ALSO carries the prod runner's
		// forfait delivery channel: the child env holds both, the provider's
		// verdict names the forfait, and an exclusive answer would suppress
		// exactly the channel it indicts.
		{name: "ambient_gateway_beside_an_ambient_forfait", env: map[string]string{
			"ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic", "ANTHROPIC_AUTH_TOKEN": "host-gateway-key",
			"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-expired"},
			render:  "Failed to authenticate. API Error: 401 OAuth token has expired",
			want:    "check ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN",
			wantNot: "forfait CLAUDE_CODE_OAUTH_TOKEN or the Anthropic API key",
			wantEnv: "slot= bearer=host-gateway-key key= forfait=sk-ant-oat-expired"},
		{name: "ambient_gateway_beside_an_ambient_key", env: map[string]string{
			"ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic", "ANTHROPIC_AUTH_TOKEN": "stale-gateway-bearer",
			"ANTHROPIC_API_KEY": "sk-ant-dead"},
			render:  `Failed to authenticate. API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			want:    "check ANTHROPIC_AUTH_TOKEN, ANTHROPIC_API_KEY",
			wantNot: "forfait CLAUDE_CODE_OAUTH_TOKEN or the Anthropic API key",
			wantEnv: "slot= bearer=stale-gateway-bearer key=sk-ant-dead forfait="},
		// A route that reaches Anthropic itself keeps the legacy wording:
		// there those ARE the channels.
		{name: "anthropic_direct", byok: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant"},
			want:    "check the forfait CLAUDE_CODE_OAUTH_TOKEN or the Anthropic API key",
			wantNot: "no working", wantEnv: "slot= bearer= key=sk-ant"},
		// ... and the operator's own Authorization line rides it into the
		// child, over the key the engine bound: the dump holds both.
		{name: "anthropic_direct_beside_an_ambient_authorization_line",
			byok:    map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant"},
			env:     map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer sk-operator-dead"},
			want:    "or the Anthropic API key — and ANTHROPIC_CUSTOM_HEADERS carries an Authorization line",
			wantNot: "no working", wantEnv: "key=sk-ant forfait= bedrock=[] headers=[Authorization: Bearer sk-operator-dead]"},
		// The gateway whose credential is a header of its own: the child env
		// shows the two channels the legacy wording names EMPTY and the
		// refused credential living in ANTHROPIC_CUSTOM_HEADERS, so the
		// sentence has to name that variable to name anything in the path.
		{name: "ambient_gateway_authenticated_by_a_header_of_its_own", env: map[string]string{
			"ANTHROPIC_BASE_URL": "https://gw.example.test/anthropic", "ANTHROPIC_CUSTOM_HEADERS": "x-api-key: sk-gateway-dead"},
			want:    "or the Anthropic API key — and ANTHROPIC_CUSTOM_HEADERS is in this spawn's path",
			wantNot: "sk-gateway-dead", wantEnv: "bearer= key= forfait= bedrock=[] headers=[x-api-key: sk-gateway-dead]"},
		// A cloud switch the child really receives at a value claw does not
		// enter the mode on: the bearer it sent is named, the mode is not
		// claimed, and the switch is named beside it.
		{name: "a_cloud_switch_at_a_value_the_cli_may_not_read_as_on", env: map[string]string{
			"ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic", "ANTHROPIC_AUTH_TOKEN": "host-gateway-key",
			"CLAUDE_CODE_USE_BEDROCK": "0"},
			want:    `check that variable — and CLAUDE_CODE_USE_BEDROCK is set to a value other than "1"`,
			wantNot: "no working credential on the CLAUDE_CODE_USE_BEDROCK route",
			wantEnv: "bearer=host-gateway-key key= forfait= bedrock=[0]"},
		// The SECOND producer: pass 1 answers prose, the formatting pass
		// renders the refusal. Its record is formatOutput's.
		{name: "formatting_pass_on_a_facade_route", env: map[string]string{"ZAI_API_KEY": "zai-env"}, twoPass: true,
			want:    "no working zai credential — check ZAI_API_KEY",
			wantNot: "forfait CLAUDE_CODE_OAUTH_TOKEN", wantEnv: "spawn 2 slot=zai bearer=zai-env"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeCredEnv(t)
			for _, k := range cloudProviderSwitches {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			ctx := context.Background()
			if tc.byok != nil {
				ctx = ctxWithCreds(t, tc.byok, nil)
			}
			task := Task{ProviderHint: tc.hint}
			render := authRender
			if tc.render != "" {
				render = tc.render
			}
			spawns := map[int][]byte{1: resultLine(t, render, true)}
			if tc.twoPass {
				task.OutputSchema = []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
				task.AllowedTools = []string{"Read"}
				spawns[1] = resultLine(t, "working notes, no answer yet", false)
				spawns[2] = resultLine(t, render, true)
			}

			childEnv, got, err := runAuthRender(t, ctx, task, spawns)
			var auth *ErrAuthFailed
			if !errors.As(err, &auth) {
				t.Fatalf("err = %v, want ErrAuthFailed (child env: %s)", err, childEnv)
			}
			if !strings.Contains(auth.Detail, tc.want) {
				t.Errorf("detail = %q, want %q (child env: %s)", auth.Detail, tc.want, childEnv)
			}
			if strings.Contains(auth.Detail, tc.wantNot) {
				t.Errorf("detail = %q names a channel this route did not use (child env: %s)", auth.Detail, childEnv)
			}
			if !strings.Contains(childEnv, tc.wantEnv) {
				t.Errorf("child env = %q, want %q — the detail must name what the CLI actually held", childEnv, tc.wantEnv)
			}
			if len(*got) != 1 || (*got)[0].Window != usagecap.WindowAuth || (*got)[0].Status != usagecap.StatusRejected {
				t.Errorf("readings = %+v, want one rejected WindowAuth reading", *got)
			}
		})
	}
}

// The header predicate reads the variable the CLI reads, so it is pinned
// against the CLI's own parser rather than against a second description of the
// format: claw's ParseCustomHeaders is that reader (vendored, first-party), and
// on every block that parser ACCEPTS, "an Authorization header is present"
// must mean the same thing on both sides of the wire. A mirror written from the
// documentation drifts at the edges the documentation does not mention — a \r
// line ending, a blank line, a colon in the value, the word in a value instead
// of a name. Mutation: widen the predicate to the whole value
// (strings.Contains on the raw text) and the value-mention rows redden.
//
// On a block that parser REFUSES there is no mirror to hold: the predicate
// answers per LINE on purpose, and each such block says here which answer it
// owes. The lenient one costs nothing where the provider resolves its identity
// from the environment — the refusal happens before any request (the subtest
// below executes that path), so no auth render follows — and where a reader
// drops the bad line instead, or never reads the variable at all, the line is
// still the operator's to check. wantOnRefusal is that expectation, written per
// input rather than derived, so the arm discriminates instead of restating the
// predicate. Mutation: make the predicate refuse the block too (return false
// on a line with no name) and the four rows that name a line redden.
func TestCarriesAuthorizationHeader_AgreesWithTheCLIsParserAndAnswersPerLineBeyondIt(t *testing.T) {
	cases := []struct {
		raw           string
		wantOnRefusal bool
	}{
		{raw: ""},
		{raw: "   "},
		{raw: "Authorization: Bearer sk-operator-gateway"},
		{raw: "authorization: Bearer sk-lower"},
		{raw: "  Authorization  :  Bearer sk-spaced"},
		{raw: "x-tenant: t1\nAuthorization: Bearer sk-second-line"},
		{raw: "Authorization: Bearer sk-crlf\r\nx-tenant: t1"},
		{raw: "x-tenant: t1\n\nAuthorization: Bearer sk-after-a-blank-line"},
		{raw: "User-Agent: my-tool/1.0"},
		{raw: "x-note: see the authorization: guide"},
		{raw: "X-Authorization: Bearer sk-other-header"},
		{raw: "x-proxy: https://gw.example.test/authorization: no"},
		// The refused half: a block the parser rejects, with and without a
		// well-formed Authorization line beside the bad one. The commented
		// block is how an operator annotates this variable, and the line is
		// still theirs to check.
		{raw: "Authorization", wantOnRefusal: false},
		{raw: "nocolon", wantOnRefusal: false},
		{raw: "Authorization: Bearer sk-x\nnocolon", wantOnRefusal: true},
		{raw: "# our gateway\nAuthorization: Bearer sk-x", wantOnRefusal: true},
		{raw: "Authorization: Bearer sk-x\n# our gateway", wantOnRefusal: true},
		{raw: "Authorization: Bearer sk-x\n:novalue", wantOnRefusal: true},
	}
	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.raw, "\n", "|"), func(t *testing.T) {
			headers, err := clawapi.ParseCustomHeaders(tc.raw)
			if err != nil {
				if got := carriesAuthorizationHeader(tc.raw); got != tc.wantOnRefusal {
					t.Errorf("carriesAuthorizationHeader(%q) = %v on a block the CLI's parser refuses (%v), want %v",
						tc.raw, got, err, tc.wantOnRefusal)
				}
				return
			}
			want := false
			for name := range headers {
				if strings.EqualFold(strings.TrimSpace(name), "authorization") {
					want = true
				}
			}
			if got := carriesAuthorizationHeader(tc.raw); got != want {
				t.Errorf("carriesAuthorizationHeader(%q) = %v, the CLI's parser sees %v (headers %v)",
					tc.raw, got, want, headers)
			}
		})
	}
	// Why the lenient answer is free on the readers that resolve the identity
	// from the environment: the refusal travels up from the variable to the
	// client, which returns it INSTEAD of sending the request
	// (internal/api/client.go resolves the identity before the first attempt),
	// so such a CLI never renders an auth failure on such a block. It is the
	// FUNCTION that is executed here, which is the scope of the claim: a
	// provider that bypasses it — claw's bedrock one reads no header at all —
	// can still reach an auth render, and the remediation is written to be
	// true there too.
	t.Run("a_refused_block_stops_the_identity_resolving_readers", func(t *testing.T) {
		t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "# our gateway\nAuthorization: Bearer sk-x")
		if _, err := clawapi.ResolveIdentity("", "ua/1.0", nil); err == nil {
			t.Fatal("ResolveIdentity accepted a malformed block: the CLI would send the request after all")
		}
		t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "Authorization: Bearer sk-x")
		if _, err := clawapi.ResolveIdentity("", "ua/1.0", nil); err != nil {
			t.Fatalf("ResolveIdentity refused a well-formed block: %v", err)
		}
	})
}

// The record never tells the remediation that a variable funded a route when
// that variable does not hold the route's key: the claim is made by comparing
// the KEY the spawn carries, so no inference over the resolver's three funding
// channels stands between the fact and the sentence. The run's own channels win
// the tie, as the resolver's order does.
//
// Mutation: drop the value comparison (keep only "the run does not carry it")
// and neither_channel_holds_it nor the_variable_holds_another_key can redden —
// the two states no producer leaves behind, which is exactly why the comparison
// and not the inference is what the sentence rests on.
func TestFacadeKeyFromProcessEnv_NamesAVariableOnlyWhenItHoldsTheKey(t *testing.T) {
	cases := []struct {
		name   string
		slot   string
		key    string
		envVal string
		run    map[secrets.Provider]string
		pinned map[secrets.Provider]string
		want   bool
	}{
		{name: "the_variable_holds_it", slot: "zai", key: "k1", envVal: "k1", want: true},
		{name: "the_variable_holds_it_for_moonshot", slot: "moonshot", key: "k1", envVal: "k1", want: true},
		{name: "the_run_holds_it_too", slot: "zai", key: "k1", envVal: "k1",
			run: map[secrets.Provider]string{secrets.ProviderZAI: "k1"}},
		{name: "a_tier_pinned_it_and_the_variable_holds_it_too", slot: "zai", key: "k1", envVal: "k1",
			pinned: map[secrets.Provider]string{secrets.ProviderZAI: "k1"}},
		{name: "the_run_holds_it_alone", slot: "zai", key: "k1",
			run: map[secrets.Provider]string{secrets.ProviderZAI: "k1"}},
		{name: "the_variable_holds_another_key", slot: "zai", key: "k1", envVal: "k2"},
		{name: "neither_channel_holds_it", slot: "zai", key: "k1"},
		{name: "a_slot_with_no_variable_of_its_own", slot: "anthropic", key: "k1"},
		{name: "no_key_at_all", slot: "zai", envVal: "k1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeCredEnv(t)
			if envKey := facadeEnvKey(tc.slot); envKey != "" && tc.envVal != "" {
				t.Setenv(envKey, tc.envVal)
			}
			ctx := context.Background()
			if tc.run != nil || tc.pinned != nil {
				ctx = secrets.WithCredentials(ctx, secrets.Credentials{APIKeys: tc.run, PinnedAPIKeys: tc.pinned})
			}
			if got := facadeKeyFromProcessEnv(ctx, tc.slot, tc.key); got != tc.want {
				t.Errorf("facadeKeyFromProcessEnv(%q, key) = %v, want %v", tc.slot, got, tc.want)
			}
		})
	}
}

// Every producer of the record hands credSpawnOf the env THIS spawn gets, not
// a default: the sandbox bit decides whether this process's env is the fallback
// at all, and the resolver's selection decides which channels are the run's.
// A producer that passes a literal is blind in exactly the way that let the
// host/sandbox confusion ship — and a third one will be added by someone who
// reads neither. Asserted over the package's own source because the formatting
// pass's spawn runs a CLI inside a container, which no unit test stands up.
//
// Mutation: pass `false` for the sandbox bit at either call site and this
// reddens, naming the site.
func TestCredSpawnOfProducers_EveryCallSitePassesTheSpawnsOwnFacts(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	sites := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "credSpawnOf" {
				return true
			}
			sites++
			where := fset.Position(call.Pos())
			if len(call.Args) != 4 {
				t.Errorf("%s: credSpawnOf takes 4 arguments, this call passes %d", where, len(call.Args))
				return true
			}
			if id, literal := call.Args[2].(*ast.Ident); literal && id.Name == "nil" {
				t.Errorf("%s: the resolver's selection is passed as nil — every channel would read as the operator's", where)
			}
			sandboxArg, ok := call.Args[3].(*ast.CallExpr)
			if !ok {
				t.Errorf("%s: the sandbox bit is a literal, not this task's (taskSandboxed)", where)
				return true
			}
			if fn, ok := sandboxArg.Fun.(*ast.Ident); !ok || fn.Name != "taskSandboxed" {
				t.Errorf("%s: the sandbox bit comes from %v, want taskSandboxed(task)", where, sandboxArg.Fun)
			}
			return true
		})
	}
	// The two producers: setupCredsAndSession (pass 1) and formatOutput.
	if sites < 2 {
		t.Errorf("found %d credSpawnOf call sites in the package source, want both producers", sites)
	}
}
