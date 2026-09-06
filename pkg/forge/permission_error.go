package forge

import (
	"fmt"
	"strings"
)

// PermissionError is a forge call refused for want of a NAMED permission:
// the credential behind the call exists and is valid, but does not carry a
// grant the call is gated on. It is the typed form of two refusals that are
// otherwise opaque at the call site — a 403 whose body says "Resource not
// accessible by integration" (an installation token minted without the
// grant), and a GitHub-App token mint answered 422 "permissions not granted"
// (an installation whose owner never approved the grant) — and it exists so
// the operator reads the permission to approve and where, not an HTTP code.
//
// Missing lists "permission:level" pairs ("checks:read"). Cause is the
// forge's own refusal (ErrForbidden or an ErrPermissionsNotGranted-wrapped
// mint error), reachable through errors.Is so every caller that classified
// on those sentinels keeps doing so.
type PermissionError struct {
	Provider Provider
	// Op is the refused call ("GET check-runs", "mint installation token").
	Op string
	// Missing is what the call needs and the credential cannot prove, as
	// "permission:level" pairs.
	Missing []string
	// Remedy is the operator step that closes the gap.
	Remedy string
	// Cause is the forge's refusal, kept verbatim: it carries the forge's
	// own wording, which is the evidence the operator needs when the remedy
	// does not apply.
	Cause error
}

func (e *PermissionError) Error() string {
	var b strings.Builder
	if e.Provider != "" {
		b.WriteString(string(e.Provider))
		b.WriteString(": ")
	}
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	b.WriteString("missing permission ")
	if len(e.Missing) > 0 {
		b.WriteString(strings.Join(e.Missing, ", "))
	} else {
		b.WriteString("(unnamed)")
	}
	if e.Remedy != "" {
		b.WriteString(" — ")
		b.WriteString(e.Remedy)
	}
	if e.Cause != nil {
		fmt.Fprintf(&b, " (%v)", e.Cause)
	}
	return b.String()
}

// Unwrap exposes the forge's refusal, so errors.Is(err, ErrForbidden) and
// errors.Is(err, ErrPermissionsNotGranted) hold exactly as before the error
// was typed.
func (e *PermissionError) Unwrap() error { return e.Cause }
