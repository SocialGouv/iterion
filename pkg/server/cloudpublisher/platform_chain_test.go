package cloudpublisher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The platform tier is the one the branch's own documentation gives as the
// FIRST worked example of a credential chain (`iterion remote admin llm oauth
// set claude_code --rank 1`), and it was the one tier that could not walk one:
// rank 0 filled the kind and marked its wire taken, so the very next record —
// rank 1 of the same kind — was refused by !fillable before any window was
// looked at. A closed platform primary kept being handed to every run while
// the advertised fallback sat unreachable in the store.
//
// The tenant and org tiers already skip a window-closed forfait; these cases
// are the platform half, plus the rule that made the gap defensible while one
// record per kind was all the store could hold — the platform is the last
// DB-backed tier, so a kind with NO open link must still hand something over.

// seedPlatformLink seals one link of the platform's claude_code chain, with a
// fingerprint of its own: the window meter keys on the fingerprint, so links
// sharing one would close together and the case would prove nothing.
func seedPlatformLink(t *testing.T, st secrets.OAuthStore, sealer secrets.Sealer, rank int, token, fp string) {
	t.Helper()
	blob := []byte(`{"claudeAiOauth":{"accessToken":"` + token + `"}}`)
	sealed, err := secrets.SealOAuthPayload(sealer, secrets.PlatformOwnerKey, secrets.OAuthKindClaudeCode, blob)
	if err != nil {
		t.Fatalf("seal rank %d: %v", rank, err)
	}
	if err := st.Upsert(context.Background(), secrets.OAuthRecord{
		UserID:        secrets.PlatformOwnerKey,
		Kind:          secrets.OAuthKindClaudeCode,
		Rank:          rank,
		SealedPayload: sealed,
		Fingerprint:   fp,
	}); err != nil {
		t.Fatalf("upsert rank %d: %v", rank, err)
	}
}

// platformWindowClosed records the provider's own refusal for one platform
// credential, under the key the runner meters it with (ScopePlatform — the
// deployment's forfait is ONE meter for the whole fleet).
func platformWindowClosed(t *testing.T, st *usagecap.MemStore, fp string) {
	t.Helper()
	if err := st.Record(context.Background(), usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, fp), usagecap.Reading{
		Window: usagecap.WindowFiveHour, Status: usagecap.StatusRejected, Utilization: 1,
		ResetsAt: time.Now().Add(3 * time.Hour), ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record refusal for %s: %v", fp, err)
	}
}

func platformChainPublisher(t *testing.T, sealer secrets.Sealer, oauth secrets.OAuthStore, caps *usagecap.MemStore) *Publisher {
	t.Helper()
	p := &Publisher{
		oauthForfait: oauth,
		runSecrets:   secrets.NewMemoryRunSecretsStore(),
		sealer:       sealer,
		logger:       testLogger(),
	}
	if caps != nil {
		p.usageCaps = caps
	}
	return p
}

func TestPlatformChain_aClosedPrimaryFallsThroughToTheNextLink(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	oauth := secrets.NewMemoryOAuthStore()
	seedPlatformLink(t, oauth, sealer, 0, "sk-ant-platform-PRIMARY", "fp-platform-0")
	seedPlatformLink(t, oauth, sealer, 1, "sk-ant-platform-FALLBACK", "fp-platform-1")
	caps := usagecap.NewMemStore()
	platformWindowClosed(t, caps, "fp-platform-0")

	p := platformChainPublisher(t, sealer, oauth, caps)
	b := resolveBundle(t, p, p.runSecrets.(*secrets.MemoryRunSecretsStore), sealer, "run-chain", "team1", "webhook:cfg-1")

	got := string(b.OAuthCredentials["claude_code"])
	if got == "" {
		t.Fatal("the run got NO credential: skipping a closed link must fall through to the next one, not empty the bundle")
	}
	if contains(got, "sk-ant-platform-PRIMARY") {
		t.Fatalf("the window-closed platform primary was handed over anyway, with an open rank-1 link in the store: %q", got)
	}
	if !contains(got, "sk-ant-platform-FALLBACK") {
		t.Fatalf("claude_code blob = %q, want the rank-1 link", got)
	}
	// The fingerprint is what the runner's usage-cap meter keys on: stamped
	// with the primary's, the fallback inherits the exhausted readings of
	// the credential it was reached BECAUSE of, and parks on its first call.
	if got := b.OAuthFingerprints["claude_code"]; got != "fp-platform-1" {
		t.Fatalf("OAuthFingerprints[claude_code] = %q, want the served link's own %q", got, "fp-platform-1")
	}
	if !b.PlatformSourced["claude_code"] {
		t.Errorf("PlatformSourced = %v, missing claude_code — the usage cap would meter the deployment's forfait per tenant", b.PlatformSourced)
	}
}

func TestPlatformChain_anOpenPrimaryStillWins(t *testing.T) {
	// The chain is a FALLBACK chain: rank 1 is reached only when rank 0
	// cannot serve. A skip that fires on an open link would spend the
	// deployment's reserve credential on every run.
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	oauth := secrets.NewMemoryOAuthStore()
	seedPlatformLink(t, oauth, sealer, 0, "sk-ant-platform-PRIMARY", "fp-platform-0")
	seedPlatformLink(t, oauth, sealer, 1, "sk-ant-platform-FALLBACK", "fp-platform-1")

	p := platformChainPublisher(t, sealer, oauth, usagecap.NewMemStore())
	b := resolveBundle(t, p, p.runSecrets.(*secrets.MemoryRunSecretsStore), sealer, "run-open", "team1", "webhook:cfg-1")

	if got := string(b.OAuthCredentials["claude_code"]); !contains(got, "sk-ant-platform-PRIMARY") {
		t.Fatalf("claude_code blob = %q, want the rank-0 primary", got)
	}
	if got := b.OAuthFingerprints["claude_code"]; got != "fp-platform-0" {
		t.Fatalf("OAuthFingerprints[claude_code] = %q, want %q", got, "fp-platform-0")
	}
}

func TestPlatformChain_everyLinkClosedStillHandsOneOver(t *testing.T) {
	// The rule the no-skip comment was written for, now scoped to the chain:
	// the platform is the last DB-backed tier and the runner's env backstop
	// is invisible from here, so a kind with no open link must still be
	// funded — the run then parks on the provider's refusal with a durable
	// usage-window retry instead of dying on a no-credential auth error that
	// nothing retries.
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	oauth := secrets.NewMemoryOAuthStore()
	seedPlatformLink(t, oauth, sealer, 0, "sk-ant-platform-PRIMARY", "fp-platform-0")
	seedPlatformLink(t, oauth, sealer, 1, "sk-ant-platform-FALLBACK", "fp-platform-1")
	caps := usagecap.NewMemStore()
	platformWindowClosed(t, caps, "fp-platform-0")
	platformWindowClosed(t, caps, "fp-platform-1")

	p := platformChainPublisher(t, sealer, oauth, caps)
	ctx := store.WithTenant(context.Background(), "team1")
	creds, err := p.resolveAndSealCredentials(ctx, "run-all-closed", "", "team1", "webhook:cfg-1", "", nil, nil, nil, model.ModelOverrides{}, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	rec, err := p.runSecrets.Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	b, err := secrets.OpenRunBundle(sealer, "run-all-closed", rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}

	got := string(b.OAuthCredentials["claude_code"])
	if got == "" {
		t.Fatal("every link closed left the run with NO credential — a run nothing retries, where the previous behaviour parked with a durable retry")
	}
	if !contains(got, "sk-ant-platform-PRIMARY") {
		t.Fatalf("claude_code blob = %q, want the first skipped link restored", got)
	}
	if !b.PlatformSourced["claude_code"] {
		t.Errorf("PlatformSourced = %v — a RESTORED platform forfait must keep its fleet-wide metering scope", b.PlatformSourced)
	}
	// The durable-retry signal the restore exists to preserve: the runner
	// arms its usage-window retry on the earliest reopening the resolution
	// passed over.
	if creds.skippedReopensAt.IsZero() {
		t.Error("skippedReopensAt is zero — the runner has no earlier reopening to arm the retry on")
	}
}
