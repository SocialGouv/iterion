//go:build !linux

package scripthost

// Confine refuses on every platform but Linux. The bounds codemode promises —
// RLIMIT_DATA for a real heap cap, RLIMIT_CPU for the native-regex class,
// Landlock for the filesystem and TCP — have no portable equivalent, and
// running a model-authored script with a weaker set than the record promises
// would be a silent downgrade of the whole guarantee.
//
// This is a typed refusal, not a missing feature: the caller turns it into
// [RefusalUnavailable] and says so, which is what philosophy 1 asks of a bound
// with no override.
func Confine(Limits) (Applied, error) { return Applied{}, ErrConfinementUnsupported }

// LandlockABI is zero off Linux: there is no Landlock to report.
func LandlockABI() int { return 0 }
