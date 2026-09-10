package dispatcher

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// The launch-refusal retry policy of the cloud board dispatcher.
//
// A card whose launch the run service refuses before any run exists (a
// sealing failure, a queue outage, a bot that does not compile, …) is not a
// failed card: the same launch succeeds ten minutes later, or never. The
// dispatcher gives the card back to its column and tries again — but not on
// every 5s tick, and not for ever: an exponential backoff spaces the
// attempts, and an attempt cap turns a permanent refusal into a `blocked`
// filing that names the last refusal, which is the point where a human has
// to decide.

const (
	launchRefusalBackoffBase = time.Minute
	launchRefusalBackoffMax  = 30 * time.Minute
	// launchAttemptsEnv overrides the attempt cap per deployment. 0 or an
	// unparsable value leaves the default in force (and is logged once at
	// startup, see LaunchAttemptCapMisspelling).
	launchAttemptsEnv     = "ITERION_BOARD_LAUNCH_ATTEMPTS"
	defaultLaunchAttempts = 8
)

// LaunchRefusalBackoff is how long the dispatch listing skips a card after
// its n-th consecutive refusal: 1m, 2m, 4m, … doubling, capped at 30m. With
// the default cap of 8 attempts a permanently refused card is filed after
// roughly an hour and a half.
func LaunchRefusalBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := launchRefusalBackoffBase
	for i := 1; i < attempt && d < launchRefusalBackoffMax; i++ {
		d *= 2
	}
	if d > launchRefusalBackoffMax {
		d = launchRefusalBackoffMax
	}
	return d
}

// LaunchAttemptCap is the number of consecutive launch refusals after which
// the card is filed blocked instead of given back once more.
func LaunchAttemptCap() int {
	cap, _ := launchAttemptSetting()
	return cap
}

// LaunchAttemptCapEnvName is the override's env var, for the startup log.
func LaunchAttemptCapEnvName() string { return launchAttemptsEnv }

// LaunchAttemptCapMisspelling returns a diagnostic when the override is set
// to a value that is not a positive integer, and "" otherwise — the default
// stays in force, and the operator must be able to see that it did.
func LaunchAttemptCapMisspelling() string {
	_, raw := launchAttemptSetting()
	if raw == "" {
		return ""
	}
	return fmt.Sprintf("%s=%q is not a positive integer — the launch attempt cap stays at its default of %d",
		launchAttemptsEnv, raw, defaultLaunchAttempts)
}

func launchAttemptSetting() (cap int, unrecognised string) {
	raw := strings.TrimSpace(os.Getenv(launchAttemptsEnv))
	if raw == "" {
		return defaultLaunchAttempts, ""
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return defaultLaunchAttempts, raw
	}
	return n, ""
}

// NextLaunchRefusal folds one more refusal into a card's ledger: attempts
// advance, NotBefore is pushed out by the backoff for the NEW attempt count,
// and the reason and instant are recorded for the operator. prev may be nil
// (first refusal).
func NextLaunchRefusal(prev *native.LaunchRefusal, now time.Time, reason string) *native.LaunchRefusal {
	attempts := 1
	if prev != nil {
		attempts = prev.Attempts + 1
	}
	return &native.LaunchRefusal{
		Attempts:   attempts,
		LastAt:     now,
		NotBefore:  now.Add(LaunchRefusalBackoff(attempts)),
		LastReason: reason,
	}
}
