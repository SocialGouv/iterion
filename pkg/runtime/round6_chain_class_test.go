package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// A route whose answer stayed off its schema condemns that route, not the
// node: when a rescue route failed otherwise, the rescue's failure decides
// the chain, in both orders. Only a chain whose every route ended on a
// route-local verdict keeps one.
func TestR6RouteLocalVerdictsLeaveTheChainToTheOtherRoutes(t *testing.T) {
	schema := &model.OutputSchemaError{Cause: errors.New("structured response could not be parsed: prose")}
	refused := &api.APIError{Provider: "openai", StatusCode: 400, Message: "reasoning_effort must be high"}
	chain := func(errs ...error) error { return &model.ErrChainExhausted{Chain: "a,b", Errs: errs} }
	for name, rescue := range map[string]error{
		"usage_window": &delegate.ErrRateLimited{Provider: "anthropic", Kind: delegate.RateLimitKindUsageWindow, ResetAt: time.Now().Add(time.Hour)},
		"rate_limited": &delegate.ErrRateLimited{Provider: "anthropic", Kind: delegate.RateLimitKindTransient},
		"http_503":     &api.APIError{StatusCode: 503, Message: "overloaded"},
	} {
		want := ClassifyError(chain(rescue))
		for order, err := range map[string]error{"schema first": chain(schema, rescue), "rescue first": chain(rescue, schema)} {
			if got := ClassifyError(err); got != want {
				t.Errorf("%s, %s: chain = %s, want the rescue's own %s", name, order, got, want)
			}
		}
	}
	for name, tc := range map[string]struct {
		err  error
		want ErrorCode
	}{
		"every route off its schema":  {chain(schema, schema), ErrCodeSchemaValidation},
		"refused then off its schema": {chain(refused, schema), ErrCodeSchemaValidation},
		"off its schema then refused": {chain(schema, refused), ErrCodeSchemaValidation},
		"every route refused":         {chain(refused, refused), ErrCodeProviderRequestInvalid},
		"a lone route off its schema": {schema, ErrCodeSchemaValidation},
	} {
		if got := ClassifyError(tc.err); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}
