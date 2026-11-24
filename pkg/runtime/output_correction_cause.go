package runtime

import (
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Only control facts cross the durable boundary; provider bodies and task text
// stay out. Each priority searches the complete error graph, including joins.
func correctionCause(err error) *store.OutputCorrectionCause {
	c := &store.OutputCorrectionCause{Version: 1}
	if errtrack.IsAmbiguousEffect(err) {
		c.Kind = "ambiguous"
		return c
	}
	if errtrack.AnyCause(err, func(e error) bool {
		var a *delegate.ErrAuthFailed
		ok := errors.As(e, &a)
		if ok {
			c.Kind = "auth"
			c.Provider = a.Provider
		}
		return ok
	}) {
		return c
	}
	if errtrack.AnyCause(err, func(e error) bool {
		var a *delegate.ErrRateLimited
		ok := errors.As(e, &a)
		if ok && a.Kind == delegate.RateLimitKindUsageWindow {
			c.Kind = "usage_window"
			c.Provider = a.Provider
			c.WindowKind = a.Kind
			c.ResetAt = a.ResetAt
			c.SelfImposed = a.SelfImposed
			return true
		}
		return false
	}) {
		return c
	}
	if errtrack.AnyCause(err, func(e error) bool {
		var a *model.ErrNodeDeadline
		ok := errors.As(e, &a)
		if ok {
			c.Kind = "node_deadline"
			c.NodeID = a.NodeID
			c.TimeoutNanos = int64(a.Timeout)
		}
		return ok
	}) {
		return c
	}
	return nil
}

type correctionAmbiguousCause struct{}

func (correctionAmbiguousCause) Error() string {
	return "output correction stopped with an ambiguous effect"
}
func (correctionAmbiguousCause) AmbiguousEffect() bool { return true }

func rehydrateCorrectionCause(c *store.OutputCorrectionCause) error {
	if c == nil {
		return nil
	}
	if c.Version != 1 {
		return fmt.Errorf("output correction: unsupported cause version %d", c.Version)
	}
	switch c.Kind {
	case "ambiguous":
		return correctionAmbiguousCause{}
	case "auth":
		return &delegate.ErrAuthFailed{Provider: c.Provider, Detail: "durable output correction refusal"}
	case "usage_window":
		return &delegate.ErrRateLimited{Provider: c.Provider, Kind: delegate.RateLimitKindUsageWindow, ResetAt: c.ResetAt, SelfImposed: c.SelfImposed, Detail: "durable output correction refusal"}
	case "node_deadline":
		return &model.ErrNodeDeadline{NodeID: c.NodeID, Timeout: time.Duration(c.TimeoutNanos)}
	default:
		return errors.New("output correction: unsupported durable cause kind")
	}
}
