// Package errclass implements a pluggable classifier that maps errors to a
// closed set of OpenTelemetry-compatible error.type labels. The base module
// defines only vendor-neutral classes (timeouts, cancellation, network, auth,
// rate-limiting, invalid requests, permission denial, upstream 5xx). Each
// consuming SDK registers its own sentinels or matchers.
package errclass

import (
	"context"
	"errors"
	"sync"

	"go.opentelemetry.io/otel/attribute"
)

// AttrKey is the OTel-standard error.type attribute key. Recorded spans and
// metrics use it as the closed-set error classification label.
var AttrKey = attribute.Key("error.type")

// Class is a closed-set label. Consuming libraries may define their own Class
// constants, but they SHOULD keep the total cardinality bounded and stable.
type Class string

// Attr returns the error.type attribute for this class.
func (c Class) Attr() attribute.KeyValue { return AttrKey.String(string(c)) }

const (
	// Unknown is the default when no matcher fires. Use sparingly — an Unknown
	// rate climbing is a signal that your sentinel coverage is lagging.
	Unknown Class = "unknown"

	// Standard closed set.
	Timeout          Class = "timeout"
	Canceled         Class = "canceled"
	InvalidRequest   Class = "invalid_request"
	RateLimited      Class = "rate_limited"
	Auth             Class = "auth"
	PermissionDenied Class = "permission_denied"
	Upstream5xx      Class = "upstream_5xx"
	Network          Class = "network"
)

// Matcher returns (class, true) when it recognizes the error, (_, false) otherwise.
type Matcher func(error) (Class, bool)

// Registry holds an ordered list of matchers. First match wins.
type Registry struct {
	mu       sync.RWMutex
	matchers []Matcher
}

// New returns an empty registry. Consider calling RegisterDefaults() if you
// want the standard context.Canceled / context.DeadlineExceeded mappings.
func New() *Registry {
	return &Registry{}
}

// RegisterDefaults registers the baseline mappings that apply to every
// library: context.DeadlineExceeded → Timeout, context.Canceled → Canceled.
func (r *Registry) RegisterDefaults() {
	if r == nil {
		return
	}

	r.RegisterSentinel(context.DeadlineExceeded, Timeout)
	r.RegisterSentinel(context.Canceled, Canceled)
}

// RegisterMatcher appends a custom matcher. Matchers are tried in registration
// order, so register more specific matchers before more general ones.
func (r *Registry) RegisterMatcher(m Matcher) {
	if r == nil || m == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.matchers = append(r.matchers, m)
}

// RegisterSentinel wires a sentinel error to a class via errors.Is.
func (r *Registry) RegisterSentinel(target error, class Class) {
	if target == nil {
		return
	}

	r.RegisterMatcher(func(err error) (Class, bool) {
		if errors.Is(err, target) {
			return class, true
		}

		return "", false
	})
}

// Classify returns the class for err. Returns "" for a nil error and Unknown
// if no matcher fires.
func (r *Registry) Classify(err error) Class {
	if err == nil {
		return ""
	}

	if r == nil {
		return Unknown
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, matcher := range r.matchers {
		if class, ok := matcher(err); ok {
			return class
		}
	}

	return Unknown
}
