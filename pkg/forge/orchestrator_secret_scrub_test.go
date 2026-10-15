package forge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// echoingForge answers the hook update with a non-2xx whose BODY is built
// from the request it was handed — what a WAF, a debug proxy or a validating
// gateway in front of a self-managed forge answers with — through the same
// constructors the three provider clients use (HookWriteErr, RateLimitErr),
// so the error carries the forge's message exactly as production does.
type echoingForge struct {
	*fakeAdmin
	fail func(secret string) error
	sent string
}

func (f *echoingForge) UpdateHook(_ context.Context, _, _ string, spec HookSpec) (HookHandle, error) {
	f.sent = spec.Secret
	return HookHandle{}, f.fail(spec.Secret)
}

// leakedRun answers the longest leading run of the secret's own bytes found in
// s: oneCappedLine truncates, so a secret can survive as a prefix of itself,
// and a prefix of a live credential is still credential material.
func leakedRun(s, secret string) string {
	for n := len(secret); n >= hookSecretPartialMin; n-- {
		if strings.Contains(s, secret[:n]) {
			return secret[:n]
		}
	}
	return ""
}

// TestReprovision_ForgeEchoingTheSecretDoesNotLeakIt: the hook write POSTs the
// freshly minted iwh_ secret to an operator-supplied forge, behind whatever
// proxy that operator runs. Whatever that endpoint answers on a non-2xx is
// read back as the refusal's own message and travels to two places iterion
// controls — the operator-visible HTTP detail and the deployment's log sink —
// so a forge that quotes the request it refused would publish a live shared
// secret. Provision is the last frame that knows which bytes are the secret.
func TestReprovision_ForgeEchoingTheSecretDoesNotLeakIt(t *testing.T) {
	// 256 characters, so the 300-rune cap of the forge's message falls INSIDE
	// the token quoted after them.
	longFiller := strings.Repeat("the proxy refused this request. ", 8)
	for _, tc := range []struct {
		name string
		fail func(secret string) error
		// wantKept is a fragment of the forge's own words that must survive
		// the scrub: the message is the operator's only clue about WHICH
		// setting to fix.
		wantKept string
	}{
		{
			name: "the field error names the token",
			fail: func(secret string) error {
				return HookWriteErr("gitlab", "update hook", http.StatusUnprocessableEntity,
					[]byte(`{"message":{"token":["`+secret+` is not a valid token"]}}`))
			},
			wantKept: "is not a valid token",
		},
		{
			name: "the whole config is echoed",
			fail: func(secret string) error {
				return HookWriteErr("gitlab", "update hook", http.StatusUnprocessableEntity,
					[]byte(`{"message":"cannot create hook with config {\"url\":\"https://iterion.example.com/api/webhooks/gitlab/id-2\",\"token\":\"`+secret+`\"}"}`))
			},
			wantKept: "cannot create hook with config",
		},
		{
			name: "a WAF page quotes the body",
			fail: func(secret string) error {
				return HookWriteErr("gitlab", "update hook", http.StatusUnprocessableEntity,
					[]byte("<html><body><h1>403 denied</h1><p>blocked body: url=... token="+secret+"</p></body></html>"))
			},
			wantKept: "blocked body",
		},
		{
			// The cap cuts THROUGH the token, which leaves its first bytes at
			// the end of the line.
			name: "the cap truncates the token",
			fail: func(secret string) error {
				return HookWriteErr("gitlab", "update hook", http.StatusUnprocessableEntity,
					[]byte(`{"message":"`+longFiller+`body: token=`+secret+`"}`))
			},
			wantKept: "the proxy refused this request",
		},
		{
			// The other producer of a Detail: the transport types a
			// rate-limited answer itself, reading the forge's message.
			name: "a rate-limited answer echoes it",
			fail: func(secret string) error {
				return RateLimitErr("gitlab", "PUT /api/v4/projects/group%2Fapi/hooks/hook-1",
					http.StatusTooManyRequests, http.Header{"Retry-After": []string{"30"}},
					[]byte(`{"message":"throttled: PUT body {\"token\":\"`+secret+`\"}"}`))
			},
			wantKept: "throttled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, fa, sealer := newTestOrch(t)
			seedConn(t, o, sealer)
			provisionBots(t, o, "review-pr")

			ef := &echoingForge{fakeAdmin: fa, fail: tc.fail}
			useAdmin(o, ef)
			_, err := o.Provision(context.Background(), ProvisionRequest{
				TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
				BotIDs: []string{"revi-converse"}, ActorID: "u1",
			})
			if err == nil {
				t.Fatal("expected the refusal to surface")
			}
			if !strings.HasPrefix(ef.sent, "iwh_") {
				t.Fatalf("premise: the forge was handed %q, not a webhook secret", ef.sent)
			}

			// Everything a reader downstream can see: the error text (the
			// Warn line the route logs), and the typed details the HTTP
			// answer is built from.
			texts := map[string]string{"err.Error()": err.Error()}
			var se *StatusError
			if errors.As(err, &se) {
				texts["StatusError.Detail"] = se.Detail
			}
			var refused *HookRefusedError
			if errors.As(err, &refused) {
				texts["HookRefusedError.Detail"] = refused.Detail
			}
			for where, text := range texts {
				if run := leakedRun(text, ef.sent); run != "" {
					t.Errorf("%s carries %d byte(s) of the webhook secret the forge was handed:\n  %s",
						where, len(run), text)
				}
			}
			if se == nil {
				t.Fatalf("want a *StatusError in the chain, got %T: %v", err, err)
			}
			if !strings.Contains(se.Detail, hookSecretRedaction) {
				t.Errorf("the scrub left no marker of what it removed: %q", se.Detail)
			}
			if !strings.Contains(se.Detail, tc.wantKept) {
				t.Errorf("the forge's own words are gone from the detail (want %q): %q", tc.wantKept, se.Detail)
			}
		})
	}
}

// TestScrubSecretFromLine covers the two shapes of the line the scrub reads,
// directly: the secret whole, and the prefix of it a truncation leaves.
func TestScrubSecretFromLine(t *testing.T) {
	const secret = "iwh_0123456789abcdefghij"
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"whole", "token: " + secret + " is invalid", "token: " + hookSecretRedaction + " is invalid"},
		{"twice", secret + " and " + secret, hookSecretRedaction + " and " + hookSecretRedaction},
		{"truncated by the cap", "body: token=" + secret[:14] + cappedLineEllipsis, "body: token=" + hookSecretRedaction},
		// The capped message lands inside an error's text, so the truncation
		// is no longer at the end of the line.
		{"truncated, mid-text", "HTTP 422 (body: token=" + secret[:14] + cappedLineEllipsis + ")",
			"HTTP 422 (body: token=" + hookSecretRedaction + ")"},
		{"nothing of it", "Invalid url given", "Invalid url given"},
		// A run shorter than the floor is not credential material and must
		// not be eaten out of a message.
		{"a run below the floor", "see iwh_", "see iwh_"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scrubSecretFromLine(tc.line, secret); got != tc.want {
				t.Errorf("scrubSecretFromLine(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}
