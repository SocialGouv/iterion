package credroute

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A backend no arm decides answers ErrNoArm, and that answer is no Refusal:
// the reader keeps its own path for the leg (ADR-161 decision 9).
func TestABackendWithoutAnArmAnswersNoArmNotARefusal(t *testing.T) {
	_, err := Resolve(context.Background(), Call{Backend: "credroute-test-no-arm", Spec: "x/y"})
	if !errors.Is(err, ErrNoArm) {
		t.Fatalf("Resolve on an unregistered backend = %v, want ErrNoArm", err)
	}
	var r *Refusal
	if errors.As(err, &r) {
		t.Fatalf("ErrNoArm came back as a Refusal (%+v): a reader would refuse a leg it should keep on its own path", r)
	}
}

// A second arm for one backend is two answers to one question: Register
// refuses it.
func TestRegisterRefusesASecondArmForOneBackend(t *testing.T) {
	const backend = "credroute-test-twice"
	arm := func(context.Context, Call) (Spend, error) { return Spend{Channel: ChannelOwnConfig}, nil }
	registerForTest(t, backend, arm)
	if !slices.Contains(Registered(), backend) {
		t.Fatalf("Registered() = %v, want it to list %q", Registered(), backend)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a second arm for one backend was accepted")
		}
	}()
	Register(backend, arm)
}

// A Refusal unwraps to the cause its arm set: every errors.As and errors.Is a
// site matches today keeps matching through it, and its message names the
// leg, the reason and the cause.
func TestARefusalUnwrapsToItsArmsCause(t *testing.T) {
	cause := fmt.Errorf("site: %w", secrets.ErrSubscriptionOAuthForbidden)
	var err error = &Refusal{Backend: "claw", Spec: "anthropic/claude-x", Leg: Leg{Provider: "anthropic"}, Reason: ReasonSubscriptionSwitch, Cause: cause}
	if !errors.Is(err, secrets.ErrSubscriptionOAuthForbidden) {
		t.Fatalf("errors.Is(refusal, the site's sentinel) = false: %v", err)
	}
	for _, want := range []string{"claw", `"anthropic"`, "anthropic/claude-x", string(ReasonSubscriptionSwitch), secrets.ErrSubscriptionOAuthForbidden.Error()} {
		if msg := err.Error(); !strings.Contains(msg, want) {
			t.Errorf("refusal message %q does not name %q", msg, want)
		}
	}
}

// The posture is captured once per attempt: Resolve pins it before the arm
// runs, so an arm reading one name twice reads one value even when the
// process environment moves in between.
func TestResolvePinsOnePostureForTheArm(t *testing.T) {
	const backend, name = "credroute-test-posture", "CREDROUTE_TEST_POSTURE"
	t.Setenv(name, "captured")
	registerForTest(t, backend, func(ctx context.Context, call Call) (Spend, error) {
		first := Getenv(ctx, name)
		if err := os.Setenv(name, "moved"); err != nil {
			return Spend{}, err
		}
		second := Getenv(ctx, name)
		if first != second {
			return Spend{}, fmt.Errorf("one call read %s as %q then %q", name, first, second)
		}
		return Spend{Source: second}, nil
	})
	spend, err := Resolve(context.Background(), Call{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	if spend.Source != "captured" {
		t.Fatalf("the arm read %q, want the value at the capture", spend.Source)
	}
}

// A captured posture carries each name's provenance: a name a project `.env`
// planted reads as itself in process and as unset where only the operator's
// voice counts (Inherited).
func TestCaptureCarriesEachNamesProvenance(t *testing.T) {
	const planted, inherited = "CREDROUTE_TEST_PLANTED", "CREDROUTE_TEST_INHERITED"
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv(planted, "from-dotenv")
	t.Setenv(inherited, "from-operator")
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	envtrust.MarkPlanted(planted)

	p := Capture(context.Background())
	if got := p.Getenv(planted); got != "from-dotenv" {
		t.Errorf("Getenv(planted) = %q, want the value the process holds", got)
	}
	if !p.Planted(planted) || p.Inherited(planted) != "" {
		t.Errorf("planted name: Planted=%v Inherited=%q, want true and the explicit empty", p.Planted(planted), p.Inherited(planted))
	}
	if p.Planted(inherited) || p.Inherited(inherited) != "from-operator" {
		t.Errorf("inherited name: Planted=%v Inherited=%q, want false and the operator's value", p.Planted(inherited), p.Inherited(inherited))
	}
}

// A carried environment is hermetic: a name it does not hold reads as unset,
// never as the process value; Environ enumerates it sorted.
func TestACarriedEnvironmentIsHermetic(t *testing.T) {
	const name = "CREDROUTE_TEST_PROCESS_ONLY"
	t.Setenv(name, "process")
	ctx := WithEnv(context.Background(), map[string]string{"B_NAME": "b", "A_NAME": "a"})
	if v, ok := LookupEnv(ctx, name); ok || v != "" {
		t.Fatalf("a carried env answered %q (set=%v) for a name only the process holds", v, ok)
	}
	if got := Getenv(ctx, "A_NAME"); got != "a" {
		t.Fatalf("Getenv(A_NAME) = %q, want the carried value", got)
	}
	if got := Environ(ctx); !slices.Equal(got, []string{"A_NAME=a", "B_NAME=b"}) {
		t.Fatalf("Environ = %v, want the carried env sorted", got)
	}
	if got := Getenv(context.Background(), name); got != "process" {
		t.Fatalf("no carried env: Getenv = %q, want the process value", got)
	}
}

// KeySpend names the run's own key before one a shared tier pinned for the
// route, with the slot's fingerprint; ForfaitSpend names the sealed forfait.
func TestTheRunSlotSpendsNameTheirChannel(t *testing.T) {
	creds := secrets.Credentials{
		APIKeys:       map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-own"},
		PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-pin", secrets.ProviderZAI: "zai-pin"},
		Fingerprints:  map[string]string{"anthropic": "fp-a", "zai": "fp-z", "claude_code": "fp-c"},
	}
	for _, tc := range []struct {
		got  Spend
		want Spend
	}{
		{KeySpend(creds, secrets.ProviderAnthropic), Spend{Channel: ChannelRunKey, Slot: "anthropic", Fingerprint: "fp-a"}},
		{KeySpend(creds, secrets.ProviderZAI), Spend{Channel: ChannelPinnedKey, Slot: "zai", Fingerprint: "fp-z"}},
		{ForfaitSpend(creds, secrets.OAuthKindClaudeCode), Spend{Channel: ChannelRunForfait, Slot: "claude_code", Fingerprint: "fp-c"}},
	} {
		if tc.got != tc.want {
			t.Errorf("got %+v, want %+v", tc.got, tc.want)
		}
	}
}

// registerForTest registers arm for backend for the test's duration: the
// registry is the process's, and a test run again (-count) registers again.
func registerForTest(t *testing.T, backend string, arm Arm) {
	t.Helper()
	Register(backend, arm)
	t.Cleanup(func() {
		armsMu.Lock()
		defer armsMu.Unlock()
		delete(arms, backend)
	})
}
