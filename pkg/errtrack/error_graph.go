package errtrack

import (
	"errors"
	"reflect"
)

// AnyCause reads all siblings in an error graph, including redaction wrappers
// which deliberately hide Unwrap but forward As. The predicate must read
// types, not diagnostic text. Error implementations must obey the acyclic
// Unwrap/As contract expected by the standard errors package.
func AnyCause(err error, matches func(error) bool) bool {
	path := errorPath{comparable: map[error]bool{}}
	var walk func(error) bool
	walk = func(err error) bool {
		if err == nil {
			return false
		}
		if matches(err) {
			return true
		}
		if !path.enter(err) {
			return false
		}
		defer path.leave(err)
		children, _ := errorChildren(err)
		for _, child := range children {
			if walk(child) {
				return true
			}
		}
		return false
	}
	return walk(err)
}

// errorPath detects cycles without requiring every error value to be comparable.
// Entries leave with their stack frame, so a shared acyclic child is not a cycle.
type errorPath struct {
	comparable map[error]bool
	other      []error
}

func (p *errorPath) enter(err error) bool {
	if reflect.ValueOf(err).Comparable() {
		if p.comparable[err] {
			return false
		}
		p.comparable[err] = true
		return true
	}
	for _, old := range p.other {
		if reflect.DeepEqual(old, err) {
			return false
		}
	}
	p.other = append(p.other, err)
	return true
}
func (p *errorPath) leave(err error) {
	if reflect.ValueOf(err).Comparable() {
		delete(p.comparable, err)
	} else {
		p.other = p.other[:len(p.other)-1]
	}
}
func sameError(a, b error) bool { return reflect.DeepEqual(a, b) }

// errorChildren uses explicit unwrap contracts first. An opaque As forwarder
// can expose a scalar cause through As(&error) even without an unwrap facade.
func errorChildren(err error) ([]error, bool) {
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		return wrapped.Unwrap(), true
	case interface{ Unwrap() error }:
		if child := wrapped.Unwrap(); child != nil {
			return []error{child}, true
		}
		return nil, false
	case interface{ As(any) bool }:
		var children []error
		exposed := false
		var many interface{ Unwrap() []error }
		if wrapped.As(&many) && many != nil {
			children = append(children, many.Unwrap()...)
			exposed = true
		}
		var one interface{ Unwrap() error }
		if wrapped.As(&one) && one != nil {
			if child := one.Unwrap(); child != nil {
				children = append(children, child)
				exposed = true
			}
		}
		if exposed {
			return children, true
		}
		var child error
		if wrapped.As(&child) && child != nil && !sameError(err, child) {
			return []error{child}, true
		}
	}
	return nil, false
}

// AmbiguousEffect is the transport-neutral declaration of an undecided effect.
// Runtime errors reconstructed from durable codes must also implement it.
type AmbiguousEffect interface {
	error
	AmbiguousEffect() bool
}

// IsAmbiguousEffect does not let an ordinary sibling hide an ambiguous one.
func IsAmbiguousEffect(err error) bool {
	return AnyCause(err, func(e error) bool {
		var effect AmbiguousEffect
		return errors.As(e, &effect) && effect.AmbiguousEffect()
	})
}

// AllLeafCauses reports whether a nonempty error graph contains only allowed
// terminal causes. Unlike errors.Is, a matching sibling cannot hide another
// cause. Transparent wrappers and redaction wrappers forwarding As are traversed.
// Error implementations must obey the same acyclic contract as AnyCause.
func AllLeafCauses(err error, allowed func(error) bool) bool {
	path := errorPath{comparable: map[error]bool{}}
	var walk func(error) bool
	walk = func(err error) bool {
		if err == nil || !path.enter(err) {
			return false
		}
		defer path.leave(err)
		children, container := errorChildren(err)
		if !container {
			return allowed(err)
		}
		seen := false
		for _, child := range children {
			if child != nil {
				seen = true
				if !walk(child) {
					return false
				}
			}
		}
		return seen
	}
	return walk(err)
}

// ErrAmbiguousEffect carries only the fact of an undecided effect, without
// transporting another operation's provider or credential error graph.
var ErrAmbiguousEffect error = ambiguousEffectFact{}

type ambiguousEffectFact struct{}

func (ambiguousEffectFact) Error() string         { return "operation has an ambiguous effect" }
func (ambiguousEffectFact) AmbiguousEffect() bool { return true }
