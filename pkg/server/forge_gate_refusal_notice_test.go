package server

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/store"
)

// #3048: a review parked on a gateway budget (LiteLLM's "Budget has been
// exceeded!", parked as the spend ceiling it is) carries the same caveat as
// an account's spend limit: waiting may not be enough.
func TestPauseNoticeNamesAGatewayBudgetAsASpendCeiling(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	at := now.Add(time.Hour)
	run := &store.Run{ID: "r1", FailureCode: store.FailureUsageLimitBlocked,
		Error:      `rate_limited (claw): openai: API error 422: Budget has been exceeded! Current cost: 150.18, Max budget: 150.0`,
		RetryState: &store.RunRetryState{RetryAfter: &at, Attempts: 1}}
	if body := gatePauseNoticeBody(run, pauseNoticeRoleReviewer, now); !strings.Contains(body, "Waiting may not be enough") {
		t.Errorf("the notice of a budget park lacks the spend-ceiling caveat:\n%s", body)
	}
}

// #3048: CAPABILITY_UNSUPPORTED and SCHEMA_UNUSABLE are raised by iterion
// itself too (an image without the backend's CLI, a schema it cannot parse):
// the status blames no provider for them. PROVIDER_REQUEST_INVALID can only
// come from a provider and says so.
func TestGateStatusOfADeterministicRefusalBlamesNoProviderItDidNotHear(t *testing.T) {
	for _, tc := range []struct {
		code      store.FailureCode
		err       string
		provider  bool
		wantTails string
	}{
		{store.FailureCapabilityUnsupported, `sandbox: node "review" runs on claude_code, whose CLI the image ghcr.io/acme/custom:1 does not ship`, false, gateDeterministicRefusalRemedy},
		{store.FailureSchemaUnusable, `claude_code: parse explicitschema: unsupported keyword "$dynamicRef"`, false, gateDeterministicRefusalRemedy},
		{store.FailureProviderRequestInvalid, `openai: API error 415: Unsupported Media Type`, true, gateProviderRefusalRemedy},
	} {
		desc := gateInterruptedDescriptionFor(&store.Run{ID: "r1", FailureCode: tc.code, Error: tc.err})
		if !strings.HasSuffix(desc, tc.wantTails) || utf8.RuneCountInString(desc) > 140 {
			t.Errorf("%s: %q, want the %q remedy within 140 runes", tc.code, desc, tc.wantTails)
		}
		remedy := strings.TrimPrefix(desc[strings.LastIndex(desc, ")"):], ")")
		if got := strings.Contains(remedy, "provider"); got != tc.provider {
			t.Errorf("%s: remedy %q names a provider = %v, want %v", tc.code, remedy, got, tc.provider)
		}
	}
}
