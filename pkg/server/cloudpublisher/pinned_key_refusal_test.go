package cloudpublisher

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// A webhook `key_overrides` pin bypasses the evidence predicate BY DESIGN —
// the operator named that key, and honouring the pin over the optimisation
// is what keeps the predicate an optimisation. But when the pinned key
// carries fresh refusals, the only trace was the ABSENCE of a skip log:
// every run of that webhook fed the same wall, and nothing said so (#629
// pt 4). The pin still wins; it is no longer silent.
func TestResolve_pinnedKeyWithFreshRefusalsWarns(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-pinned", "fp-pinned")
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-healthy", "fp-healthy")
	pinned := firstKeyWithFingerprint(t, keys, "fp-pinned")

	caps := usagecap.NewMemStore()
	recordRefusal(t, caps, usagecap.TenantScope("team1"), "fp-pinned")

	var buf bytes.Buffer
	p := &Publisher{apiKeys: keys, usageCaps: caps,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: iterlog.New(iterlog.LevelInfo, &buf)}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	ctx := store.WithTenant(context.Background(), "team1")
	creds, err := p.resolveAndSealCredentials(ctx, "run-pin", "", "team1", "owner1", "",
		nil, map[string]string{string(secrets.ProviderAnthropic): pinned.ID}, nil, model.ModelOverrides{}, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	rec, err := rs.Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := secrets.OpenRunBundle(sealer, "run-pin", rec.SealedBundle)
	if err != nil {
		t.Fatal(err)
	}
	// The pin is still honoured — that is the contract this warns about,
	// not one it changes.
	if got := bundle.APIKeys[secrets.ProviderAnthropic]; got != "sk-pinned" {
		t.Fatalf("anthropic key = %q, want the PINNED key: a warning must not silently re-route the operator's choice", got)
	}
	log := buf.String()
	if !strings.Contains(log, pinned.ID) {
		t.Fatalf("log does not name the pinned key %q:\n%s", pinned.ID, log)
	}
	if !strings.Contains(log, "fp-pinned") {
		t.Fatalf("log does not name the refused credential:\n%s", log)
	}
	if !strings.Contains(strings.ToLower(log), "fair-usage") {
		t.Fatalf("log does not say WHY the provider is refusing:\n%s", log)
	}
}

// A pinned key with nothing against it must stay quiet: a warning that
// fires on every launch is one nobody reads.
func TestResolve_pinnedHealthyKeyIsQuiet(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-pinned", "fp-pinned")
	pinned := firstKeyWithFingerprint(t, keys, "fp-pinned")

	var buf bytes.Buffer
	p := &Publisher{apiKeys: keys, usageCaps: usagecap.NewMemStore(),
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: iterlog.New(iterlog.LevelInfo, &buf)}

	ctx := store.WithTenant(context.Background(), "team1")
	if _, err := p.resolveAndSealCredentials(ctx, "run-pin2", "", "team1", "owner1", "",
		nil, map[string]string{string(secrets.ProviderAnthropic): pinned.ID}, nil, model.ModelOverrides{}, nil); err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if strings.Contains(buf.String(), "pinned api key") {
		t.Fatalf("a healthy pinned key must produce no pin warning:\n%s", buf.String())
	}
}

// A pinned key that only a RESERVATION holds back from this bot is not a dead
// key, and the warning must not say it is.
//
// The distinction is invisible from the outside: refusedByEvidence judges the
// readings against the bot's floor-LOWERED ceiling, so it answers "refused"
// for a spent credential and for a merely held-back one alike. Only its
// floorHeld return separates them, and while the callsite discarded it a
// pinned key sitting between the lowered ceiling and the deployment's own cap
// logged "expect a park until X" on every launch — a park the run never
// meets, since the runner's guard enforces the deployment-wide policy. That is
// the shape an operator chases for an afternoon.
func TestResolve_pinnedKeyHeldOnlyByAReservationIsQuiet(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	// cap 80, 20 reserved for review-pr ⇒ an unreserved bot's ceiling is 60.
	const capPct, reservePct = 80.0, 20
	floor := budgetfloor.Static{Reservations: []budgetfloor.Reservation{
		{BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: reservePct}},
	}}

	newPub := func(t *testing.T, utilization float64) (*Publisher, secrets.ApiKey, *bytes.Buffer) {
		t.Helper()
		keys := secrets.NewMemoryApiKeyStore()
		seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-pinned", "fp-pinned")
		caps := usagecap.NewMemStore()
		if err := caps.Record(context.Background(),
			usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team1"), "fp-pinned"),
			usagecap.Reading{
				Window: usagecap.WindowFiveHour, Status: usagecap.StatusAllowed,
				Utilization: utilization, ObservedAt: time.Now(),
				ResetsAt: time.Now().Add(2 * time.Hour),
			}); err != nil {
			t.Fatalf("record: %v", err)
		}
		var buf bytes.Buffer
		p := &Publisher{apiKeys: keys, usageCaps: caps,
			runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
			logger: iterlog.New(iterlog.LevelInfo, &buf),
			capPolicy: usagecap.StaticPolicy{
				FiveHour: usagecap.WindowPolicy{MaxPercent: capPct, Mode: usagecap.ModeHard},
			},
			budgetFloor: floor,
		}
		return p, firstKeyWithFingerprint(t, keys, "fp-pinned"), &buf
	}
	resolve := func(t *testing.T, p *Publisher, pinned secrets.ApiKey, runID string) {
		t.Helper()
		ctx := store.WithTenant(context.Background(), "team1")
		if _, err := p.resolveAndSealCredentials(ctx, runID, "", "team1", "owner1", "feature-dev",
			nil, map[string]string{string(secrets.ProviderAnthropic): pinned.ID}, nil,
			model.ModelOverrides{}, nil); err != nil {
			t.Fatalf("resolveAndSealCredentials: %v", err)
		}
	}

	t.Run("held only by the reserve, nothing is announced", func(t *testing.T) {
		// 65% — over the unreserved ceiling (60), under the deployment cap (80).
		p, pinned, buf := newPub(t, 0.65)
		resolve(t, p, pinned, "run-held")
		if strings.Contains(buf.String(), "pinned api key") {
			t.Fatalf("warned about a key the RESERVE holds back, promising a park the run never meets:\n%s", buf.String())
		}
	})

	t.Run("past the deployment's own cap the warning still fires", func(t *testing.T) {
		// The guard must not swallow the case the warning exists for: at 95%
		// the deployment's own cap blocks the key too, so the park is real.
		p, pinned, buf := newPub(t, 0.95)
		resolve(t, p, pinned, "run-spent")
		if !strings.Contains(buf.String(), "pinned api key") {
			t.Fatalf("a pinned key spent past the DEPLOYMENT cap warned nothing:\n%s", buf.String())
		}
	})
}

func firstKeyWithFingerprint(t *testing.T, st secrets.ApiKeyStore, fp string) secrets.ApiKey {
	t.Helper()
	all, err := st.ListByTeam(context.Background(), "team1", "")
	if err != nil {
		t.Fatalf("ListByTeam: %v", err)
	}
	for _, k := range all {
		if k.Fingerprint == fp {
			return k
		}
	}
	t.Fatalf("no key with fingerprint %s", fp)
	return secrets.ApiKey{}
}
