package netproxy

import "fmt"

// ErrInvalidMode is returned by [Compile] when the policy mode is not
// one of allowlist, denylist, or open.
type ErrInvalidMode struct {
	Mode Mode
}

// Error implements error.
func (e *ErrInvalidMode) Error() string {
	return fmt.Sprintf("netproxy: invalid mode %q (want allowlist, denylist, or open)", e.Mode)
}

// ErrInvalidRule is returned by [Compile] when a rule string fails to
// parse. The Raw field carries the user-supplied source so error
// messages let the user fix the right line.
type ErrInvalidRule struct {
	Raw    string
	Reason string
}

// Error implements error.
func (e *ErrInvalidRule) Error() string {
	return fmt.Sprintf("netproxy: rule %q: %s", e.Raw, e.Reason)
}
