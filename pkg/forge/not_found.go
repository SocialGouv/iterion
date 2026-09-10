package forge

import (
	"strings"
)

// NotFoundError is a forge call answered 404, naming the operation that
// missed. Every provider's 404 used to collapse onto ErrHookNotFound, so a
// pull request that does not exist reported a missing webhook.
//
// MayNeed is what makes the type more than a message. GitHub answers 404 —
// not 403 — for a resource a credential is not allowed to see, so an object
// that is absent and a grant that was never approved are indistinguishable
// from the status alone. Naming the grants the endpoint is gated on lets an
// operator tell them apart; claiming the permission gap outright (a
// PermissionError) would assert what the status cannot prove.
type NotFoundError struct {
	Provider Provider
	// Op is the call that missed ("GET pull", "GET check-runs").
	Op string
	// MayNeed are the "permission:level" grants the endpoint is gated on,
	// when the caller knows them. Empty when it does not.
	MayNeed []string
}

func (e *NotFoundError) Error() string {
	var b strings.Builder
	if e.Provider != "" {
		b.WriteString(string(e.Provider))
		b.WriteString(": ")
	}
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	b.WriteString("not found")
	if len(e.MayNeed) > 0 {
		b.WriteString(" — absent, or not visible to this credential (the forge gates it on ")
		b.WriteString(strings.Join(e.MayNeed, ", "))
		b.WriteString(")")
	}
	return b.String()
}

// Unwrap makes errors.Is(err, ErrNotFound) hold for every typed 404.
func (e *NotFoundError) Unwrap() error { return ErrNotFound }

// notFoundFor maps a 404 by OPERATION. The hook operations keep
// ErrHookNotFound — the orchestrator reads it as "the hook is already gone"
// and treats deprovision as done; anything else is the resource its own
// operation names.
func notFoundFor(provider, op string, mayNeed []string) error {
	if isHookOp(op) {
		return ErrHookNotFound
	}
	return &NotFoundError{Provider: Provider(provider), Op: op, MayNeed: mayNeed}
}

// isHookOp reports whether an operation string names a webhook. The
// operations are "<verb> hook" / "GET hooks", so the last word decides —
// a substring match would catch any future op that merely mentions one.
func isHookOp(op string) bool {
	f := strings.Fields(op)
	if len(f) == 0 {
		return false
	}
	last := f[len(f)-1]
	return last == "hook" || last == "hooks"
}
