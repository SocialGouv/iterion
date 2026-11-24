package cloudpublisher

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The walk's memory of what it passed over, exercised through the walk
// itself: every scenario here configures a RESERVATION, because without one
// nothing is ever floor-held and both defects are inert (capPolicyFor returns
// early, heldOut stays empty, and the restore hands everything back as it
// always did).
//
// The deployment shape is the one the budget floor was built for
// (2026-09-08): an 80% hard five-hour cap with 20% reserved for the
// reviewer, here `revi`. A credential at 65% of its window is therefore
// USABLE for revi and held back from everybody else — the state in which the
// difference between "dead" and "spoken for" decides what a run gets.
const (
	credWalkCap     = 80.0
	credWalkReserve = 20
	// heldUtilization sits above the unreserved ceiling (80 - 20 = 60) and
	// below the deployment's own cap, which is what makes the refusal the
	// RESERVE's rather than the credential's (botCapPolicy.deployment).
	heldUtilization = 0.65
)

func credWalkPublisher(t *testing.T, reserved bool) *Publisher {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	p := &Publisher{
		apiKeys:    secrets.NewMemoryApiKeyStore(),
		usageCaps:  usagecap.NewMemStore(),
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		logger:     iterlog.New(iterlog.LevelError, nil),
		capPolicy: usagecap.StaticPolicy{
			FiveHour: usagecap.WindowPolicy{MaxPercent: credWalkCap, Mode: usagecap.ModeHard},
		},
	}
	if reserved {
		p.budgetFloor = budgetfloor.Static{Reservations: []budgetfloor.Reservation{
			{BotID: "revi", Reserve: budgetfloor.Reserve{FiveHourPercent: credWalkReserve}},
		}}
	}
	return p
}

// recordHeld records the reading that makes a credential usable for the
// reserved bot and refused for everyone else.
func recordHeld(t *testing.T, p *Publisher, scope, fp string) {
	t.Helper()
	if err := p.usageCaps.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, scope, fp),
		usagecap.Reading{Window: usagecap.WindowFiveHour, Status: usagecap.StatusAllowed,
			Utilization: heldUtilization, ObservedAt: time.Now(), ResetsAt: time.Now().Add(2 * time.Hour)}); err != nil {
		t.Fatalf("record held reading: %v", err)
	}
}

// walkAndPreview resolves the bundle AND the read-only preview of the same
// publisher, and holds the preview to what the walk did: the preview is the
// operator's answer to "what will this launch spend", so a restore the walk
// learned and the preview did not is a confident lie about the next launch
// (credential_preview.go states that parity as a doctrine, and the drift it
// guards against is the one this repo has paid for repeatedly).
func walkAndPreview(t *testing.T, p *Publisher, runID, botID string) secrets.RunBundle {
	t.Helper()
	bundle := resolveFloorBundle(t, p, runID, "team1", botID)
	spec := runview.CredentialPreviewSpec{
		Context: runview.CredentialPreviewContext{TeamID: "team1", BotID: botID, Source: runview.CredentialPreviewSource{Kind: "personal"}},
		OwnerID: "owner1",
	}
	preview, err := previewReadOnly(p).PreviewCredentials(t.Context(), spec, nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if got, want := selectedDescriptions(preview), bundleDescriptions(bundle); !reflect.DeepEqual(got, want) {
		t.Errorf("the preview selects %v and the walk sealed %v — the operator is told about a launch that does not happen", got, want)
	}
	return bundle
}

// #2367 — a key the walk passes over on its CONCURRENCY CEILING, which a
// reservation also holds, was handed back to an unreserved bot: the ceiling
// branch answered before the evidence read, so the floor-held note the
// restore step reads was never written, and at the end of the walk the key
// looked exactly like a dead one.
//
// Both halves are asserted here, because only the pair proves the fix is the
// ORDERING and not a new refusal: the reserved key stays home for an
// unreserved bot, and a deployment that configured NO reservation still gets
// its ceiling-skipped key back (a parked run with a durable retry beats one
// that dies on an empty wire).
func TestCredWalk_AKeyAtItsCeilingAndHeldByTheReserveIsNotRestored(t *testing.T) {
	// One alive run already holds the key, which is its whole ceiling.
	withCeilingMet := func(t *testing.T, p *Publisher, fp string) {
		t.Helper()
		rs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		ctx := context.Background()
		if _, err := rs.CreateRun(ctx, "alive-1", "demo", nil); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		r, err := rs.LoadRun(ctx, "alive-1")
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		r.Status = store.RunStatusRunning
		if err := rs.SaveRun(ctx, r); err != nil {
			t.Fatalf("SaveRun: %v", err)
		}
		if err := rs.SetRunCredStamp(ctx, "alive-1", store.RunCredStamp{Fingerprints: []string{fp}}); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		p.store = rs
	}
	// The provider holds ONE key, capped at a single concurrent run.
	seedCapped := func(t *testing.T, p *Publisher) {
		t.Helper()
		id := secrets.NewApiKeyID()
		sealed, err := secrets.SealAPIKey(p.sealer, id, []byte("sk-held"))
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		if err := p.apiKeys.Create(context.Background(), secrets.ApiKey{
			ID: id, ScopeTeamID: "team1", Provider: secrets.ProviderAnthropic,
			Fingerprint: "fp-held", Name: "sk-held", SealedSecret: sealed,
			MaxConcurrentRuns: 1, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	t.Run("the reserve keeps a key the ceiling skipped", func(t *testing.T) {
		p := credWalkPublisher(t, true)
		seedCapped(t, p)
		withCeilingMet(t, p, "fp-held")
		recordHeld(t, p, usagecap.TenantScope("team1"), "fp-held")

		if got := walkAndPreview(t, p, "run-unreserved", "feature-dev").APIKeys[secrets.ProviderAnthropic]; got != "" {
			t.Fatalf("the reserve's key was handed to an unreserved bot (%q): the walk skipped it on its CEILING, so nothing recorded that a reservation holds it, and the restore could not tell it from a dead key", got)
		}
	})

	t.Run("with no reservation the ceiling-skipped key still comes back", func(t *testing.T) {
		p := credWalkPublisher(t, false)
		seedCapped(t, p)
		withCeilingMet(t, p, "fp-held")
		recordHeld(t, p, usagecap.TenantScope("team1"), "fp-held")

		if got := walkAndPreview(t, p, "run-plain", "feature-dev").APIKeys[secrets.ProviderAnthropic]; got != "sk-held" {
			t.Fatalf("anthropic wire = %q, want sk-held: with NO reservation configured nothing is floor-held, and the key the ceiling passed over is still the run's only park point", got)
		}
	})
}
