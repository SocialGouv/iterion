// Package protocol owns the leaf wire contract shared by the host relay and
// the Linux guest. It has no application initialization or backend dependency.
package protocol

import "errors"

var ErrTransport = errors.New("exec environment transport refused")

// Error deliberately does not print values or its cause: an I/O implementation
// may include transported data in its diagnostic. Is/As retain the cause.
type Error struct {
	Phase string
	Kind  string
	Cause error
}

func (e *Error) Error() string        { return "exec environment transport: " + e.Phase + ": " + e.Kind }
func (e *Error) Unwrap() error        { return e.Cause }
func (e *Error) Is(target error) bool { return target == ErrTransport }
func refused(phase, kind string, cause error) error {
	return &Error{Phase: phase, Kind: kind, Cause: cause}
}
