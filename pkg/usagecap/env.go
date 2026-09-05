package usagecap

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables. The cap is a property of the CREDENTIAL and of the
// deployment that owns it — not of a run — so it is configured machine-wide
// and there is deliberately no per-run flag and no DSL field: a bot must not
// be able to lift the guard that protects the operator's subscription.
//
// Setting the two _PCT variables is enough; the modes default to the posture
// each window deserves (5h soft, week hard — see the package doc).
const (
	EnvEnabled  = "ITERION_USAGE_CAP"           // "off" disables both caps
	EnvFiveHour = "ITERION_USAGE_CAP_5H_PCT"    // 0–100, 0/unset = no cap
	EnvFiveMode = "ITERION_USAGE_CAP_5H_MODE"   // off|soft|hard (default soft)
	EnvWeek     = "ITERION_USAGE_CAP_WEEK_PCT"  // 0–100, 0/unset = no cap
	EnvWeekMode = "ITERION_USAGE_CAP_WEEK_MODE" // off|soft|hard (default hard)
	// EnvTrustWindow bounds how long a stored reading is believed after it
	// was observed (a Go duration, default DefaultTrustWindow). Governs the
	// cap pre-flights AND the credential-skip evidence: both read the same
	// ledger, and both must forget a pre-reset reading at the same moment.
	EnvTrustWindow = "ITERION_USAGE_CAP_TRUST_WINDOW"
)

// Default enforcement postures, applied when only a percentage is set.
const (
	DefaultFiveHourMode = ModeSoft
	DefaultWeekMode     = ModeHard
)

// FromEnv resolves the machine-wide policy.
//
// A malformed value is an ERROR, not a fallback: every wrong answer here
// fails open (no cap), and a guard that silently stopped guarding because of
// a typo is worse than one that refuses to start. Callers surface it. The
// trust window is validated here too, so a typo in it refuses to start on
// every surface that already refuses a malformed percentage — the callers
// that consume it (TrustFromEnv) run after this check.
func FromEnv() (Policy, error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvEnabled)), "off") {
		if _, err := TrustFromEnv(); err != nil {
			return Policy{}, err
		}
		return Policy{}, nil
	}
	var errs []error
	five, err := windowFromEnv(EnvFiveHour, EnvFiveMode, DefaultFiveHourMode)
	if err != nil {
		errs = append(errs, err)
	}
	week, err := windowFromEnv(EnvWeek, EnvWeekMode, DefaultWeekMode)
	if err != nil {
		errs = append(errs, err)
	}
	if _, err := TrustFromEnv(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return Policy{}, errors.Join(errs...)
	}
	return Policy{FiveHour: five, Week: week}, nil
}

// TrustFromEnv resolves the reading-trust bound: DefaultTrust with the
// window replaced by ITERION_USAGE_CAP_TRUST_WINDOW when set. A malformed
// or non-positive duration is an error, never a silent default — the same
// rule as the percentages, for the same reason.
func TrustFromEnv() (Trust, error) {
	t := DefaultTrust()
	raw := strings.TrimSpace(os.Getenv(EnvTrustWindow))
	if raw == "" {
		return t, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return Trust{}, fmt.Errorf("%s: %q is not a duration (want e.g. 3h, 90m)", EnvTrustWindow, raw)
	}
	if d <= 0 {
		return Trust{}, fmt.Errorf("%s: %s must be positive (a reading must be trusted for SOME time)", EnvTrustWindow, d)
	}
	t.Window = d
	return t, nil
}

func windowFromEnv(pctVar, modeVar string, defMode Mode) (WindowPolicy, error) {
	raw := strings.TrimSpace(os.Getenv(pctVar))
	mode, err := ParseMode(os.Getenv(modeVar), defMode)
	if err != nil {
		return WindowPolicy{}, fmt.Errorf("%s: %w", modeVar, err)
	}
	if raw == "" {
		return WindowPolicy{Mode: mode}, nil
	}
	pct, err := strconv.ParseFloat(strings.TrimSuffix(raw, "%"), 64)
	if err != nil {
		return WindowPolicy{}, fmt.Errorf("%s: %q is not a percentage (want 0–100)", pctVar, raw)
	}
	if pct < 0 || pct > 100 {
		return WindowPolicy{}, fmt.Errorf("%s: %g is out of range (want 0–100)", pctVar, pct)
	}
	return WindowPolicy{MaxPercent: pct, Mode: mode}, nil
}
