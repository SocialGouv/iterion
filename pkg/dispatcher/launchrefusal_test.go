package dispatcher

import (
	"testing"
	"time"
)

func TestLaunchRefusalBackoff_DoublesAndCaps(t *testing.T) {
	for attempt, want := range map[int]time.Duration{
		0: time.Minute, 1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute,
		5: 16 * time.Minute, 6: 30 * time.Minute, 7: 30 * time.Minute, 40: 30 * time.Minute,
	} {
		if got := LaunchRefusalBackoff(attempt); got != want {
			t.Errorf("backoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}

func TestNextLaunchRefusal_AdvancesTheLedger(t *testing.T) {
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	first := NextLaunchRefusal(nil, now, "queue unavailable")
	if first.Attempts != 1 || !first.NotBefore.Equal(now.Add(time.Minute)) || first.LastReason != "queue unavailable" || !first.LastAt.Equal(now) {
		t.Fatalf("first refusal = %+v", first)
	}
	second := NextLaunchRefusal(first, now.Add(time.Minute), "still down")
	if second.Attempts != 2 || !second.NotBefore.Equal(now.Add(time.Minute).Add(2*time.Minute)) || second.LastReason != "still down" {
		t.Fatalf("second refusal = %+v", second)
	}
	if first.Attempts != 1 {
		t.Fatal("NextLaunchRefusal mutated its input")
	}
}

func TestLaunchAttemptCap_EnvOverride(t *testing.T) {
	t.Setenv(launchAttemptsEnv, "")
	if got := LaunchAttemptCap(); got != defaultLaunchAttempts {
		t.Fatalf("default cap = %d, want %d", got, defaultLaunchAttempts)
	}
	t.Setenv(launchAttemptsEnv, "3")
	if got := LaunchAttemptCap(); got != 3 {
		t.Fatalf("cap from env = %d, want 3", got)
	}
	if msg := LaunchAttemptCapMisspelling(); msg != "" {
		t.Fatalf("a valid override reported a misspelling: %s", msg)
	}
	for _, bad := range []string{"0", "-2", "many", "1.5"} {
		t.Setenv(launchAttemptsEnv, bad)
		if got := LaunchAttemptCap(); got != defaultLaunchAttempts {
			t.Errorf("cap with %q = %d, want the default %d", bad, got, defaultLaunchAttempts)
		}
		if msg := LaunchAttemptCapMisspelling(); msg == "" {
			t.Errorf("no diagnostic for %q — the default silently won", bad)
		}
	}
}
