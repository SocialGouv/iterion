package sandbox

// CommandCancellationOwner declares that every successful Command result owns
// its process group and its Cancel hook. SDK adapters preserve that hook rather
// than replacing a relay's coordinated shutdown with an immediate SIGKILL.
// This covers local client cleanup only; remote workload teardown is separate.
type CommandCancellationOwner interface {
	OwnsCommandCancellation() bool
}

// OwnsCommandCancellation keeps unmarked drivers on the SDK's existing policy.
func OwnsCommandCancellation(run Run) bool {
	owner, ok := run.(CommandCancellationOwner)
	return ok && owner.OwnsCommandCancellation()
}
