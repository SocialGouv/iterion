package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// mintVerdictContext is the verdict-carrying context the derivation gate
// stamps after its ask grants — issued through the same constructor
// (auth.NewMintVerdict), so a test mints under exactly the context a gated
// route's handler runs under.
func mintVerdictContext() context.Context {
	return auth.ContextWithMintVerdict(context.Background(), auth.NewMintVerdict(auth.MintTeamToken, "test"))
}

func TestMintAndVerifyToken(t *testing.T) {
	pt, hash, last4, fp, err := MintToken(mintVerdictContext())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pt, TokenPrefix) {
		t.Fatalf("missing prefix: %q", pt)
	}
	if hash == "" || last4 == "" || fp == "" {
		t.Fatalf("empty derived fields: hash=%q last4=%q fp=%q", hash, last4, fp)
	}
	if !VerifyToken(pt, hash) {
		t.Fatal("verify of the minted token should pass")
	}
	if VerifyToken(pt+"x", hash) {
		t.Fatal("tampered token must fail")
	}
	if VerifyToken("", hash) || VerifyToken(pt, "") {
		t.Fatal("empty inputs must fail")
	}
}

// mintThroughHelper is one caller shape of the witness below: a mint an
// intermediate function reaches.
func mintThroughHelper(ctx context.Context) (string, string, string, string, error) {
	return MintToken(ctx)
}

// TestMintTokenRefusesWithoutTheDerivationVerdict is the primitive's own
// probe: a webhook token is a team token (auth.MintTeamToken), and the
// context must carry the derivation chokepoint's verdict for it. Whatever
// shape the caller wears — the direct call, the primitive held as a value,
// a local binding, a branch arm, a helper in between — a context without
// the verdict refuses, fail closed, and a verdict minted for ANOTHER mint
// approves nothing. These are the shapes the round-4 review chased through
// an AST guard over the callers; the requirement lives in the primitive
// now, so they are all the same refusal, with no guard above to slip past.
func TestMintTokenRefusesWithoutTheDerivationVerdict(t *testing.T) {
	var alias = MintToken
	shapes := []struct {
		name string
		mint func(context.Context) (string, string, string, string, error)
	}{
		{"the direct call", MintToken},
		{"the primitive held as a value", alias},
		{"a local binding", func(ctx context.Context) (string, string, string, string, error) {
			mint := MintToken
			return mint(ctx)
		}},
		{"a branch arm", func(ctx context.Context) (string, string, string, string, error) {
			mint := MintToken
			if ctx != nil {
				return mint(ctx)
			}
			return "", "", "", "", nil
		}},
		{"a helper in between", mintThroughHelper},
	}
	refused := func(mint func(context.Context) (string, string, string, string, error), ctx context.Context) error {
		_, _, _, _, err := mint(ctx)
		return err
	}
	for _, shape := range shapes {
		err := refused(shape.mint, context.Background())
		var missing *auth.MintVerdictMissingError
		if !errors.As(err, &missing) {
			t.Errorf("%s without the verdict: %v, want a *auth.MintVerdictMissingError", shape.name, err)
			continue
		}
		if missing.Primitive != "webhooks.MintToken" {
			t.Errorf("%s: the refusal names %q, want webhooks.MintToken", shape.name, missing.Primitive)
		}
	}
	// A verdict for another mint approves nothing: the PAT gate's stamp
	// mints no webhook token, which is what a typed verdict buys over a
	// boolean.
	patCtx := auth.ContextWithMintVerdict(context.Background(), auth.NewMintVerdict(auth.MintPAT, "test"))
	err := refused(MintToken, patCtx)
	var missing *auth.MintVerdictMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("a MintPAT verdict minted a webhook token: %v", err)
	}
}

func TestSealAndVerifyHMACSignature(t *testing.T) {
	sealer := newTestSealer(t)

	pt, _, _, _, err := MintToken(mintVerdictContext())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealHMACSecret(sealer, "w1", pt)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	body := []byte(`{"hello":"world"}`)
	mac := hmac.New(sha256.New, []byte(pt))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	if !VerifyHMACSignature(sealer, "w1", sealed, body, sig) {
		t.Fatal("raw hex signature should verify")
	}
	if !VerifyHMACSignature(sealer, "w1", sealed, body, "sha256="+sig) {
		t.Fatal("sha256= prefix should be tolerated")
	}
	// wrong webhook id → AAD mismatch → false
	if VerifyHMACSignature(sealer, "other", sealed, body, sig) {
		t.Fatal("AAD mismatch must fail")
	}
	// tampered body
	if VerifyHMACSignature(sealer, "w1", sealed, []byte("tampered"), sig) {
		t.Fatal("tampered body must fail")
	}
	// malformed hex
	if VerifyHMACSignature(sealer, "w1", sealed, body, "zz") {
		t.Fatal("non-hex must fail")
	}
	// empty signature, empty sealed, nil sealer all return false (never panic)
	if VerifyHMACSignature(sealer, "w1", sealed, body, "") {
		t.Fatal("empty signature must fail")
	}
	if VerifyHMACSignature(sealer, "w1", nil, body, sig) {
		t.Fatal("empty sealed must fail")
	}
	if VerifyHMACSignature(nil, "w1", sealed, body, sig) {
		t.Fatal("nil sealer must fail")
	}
}

// newTestSealer returns a deterministic AES-GCM sealer for HMAC tests.
func newTestSealer(t *testing.T) secrets.Sealer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s, err := secrets.NewAESGCMSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConfig_AllowsBotAndSelect(t *testing.T) {
	scoped := &Config{BotIDs: []string{"review-pr"}}
	if !scoped.AllowsBot("review-pr") || scoped.AllowsBot("other") {
		t.Fatal("scoped allow broken")
	}
	if scoped.SelectBot() != "review-pr" {
		t.Fatal("sole-bot select")
	}
	wild := &Config{BotIDs: []string{"*"}, WildcardBots: true}
	if !wild.AllowsBot("anything") {
		t.Fatal("wildcard allow")
	}
	if wild.SelectBot() != "" {
		t.Fatal("wildcard select must be ambiguous")
	}
	def := &Config{BotIDs: []string{"a", "b"}, DefaultBotID: "b"}
	if def.SelectBot() != "b" {
		t.Fatal("default select")
	}
	if (&Config{BotIDs: []string{"a", "b"}}).SelectBot() != "" {
		t.Fatal("multi without default must be ambiguous")
	}
}

func TestMemoryConfigStore(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryConfigStore()
	c := Config{ID: "w1", TenantID: "t1", Name: "gl", Provider: ProviderGitLab, CreatedAt: time.Now()}
	if err := st.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(ctx, c); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate create: %v", err)
	}
	got, err := st.Get(ctx, "w1")
	if err != nil || got.Name != "gl" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if list, _ := st.ListByTenant(ctx, "t1"); len(list) != 1 {
		t.Fatal("list by tenant")
	}
	if other, _ := st.ListByTenant(ctx, "nope"); len(other) != 0 {
		t.Fatal("tenant isolation")
	}
	if err := st.MarkUsed(ctx, "w1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get(ctx, "w1"); got.LastUsedAt == nil {
		t.Fatal("MarkUsed did not stamp")
	}
	if err := st.Delete(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, "w1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestMemoryDeliveryStore_Idempotency(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryDeliveryStore()
	d := Delivery{ID: "d1", TenantID: "t1", WebhookID: "w1", IdempotencyKey: "k1", Status: StatusAccepted, ReceivedAt: time.Now()}
	if err := st.Insert(ctx, d); err != nil {
		t.Fatal(err)
	}
	dup := d
	dup.ID = "d2"
	if err := st.Insert(ctx, dup); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("idempotency dup: %v", err)
	}
	got, err := st.GetByIdempotencyKey(ctx, "k1")
	if err != nil || got.ID != "d1" {
		t.Fatalf("idem lookup: %+v %v", got, err)
	}
	got.Status = StatusLaunched
	got.RunID = "r1"
	if err := st.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.GetByIdempotencyKey(ctx, "k1"); again.Status != StatusLaunched || again.RunID != "r1" {
		t.Fatal("update not reflected")
	}
	if list, _ := st.ListByWebhook(ctx, "t1", "w1", 10); len(list) != 1 {
		t.Fatal("list by webhook")
	}
}

func TestMemoryCounter_OrgAndWebhookCaps(t *testing.T) {
	ctx := context.Background()
	c := NewMemoryCounter()
	now := time.Now()

	orgLim := Limits{PerOrgMonthly: 3}
	for i := 0; i < 3; i++ {
		if ok, _ := c.Allow(ctx, "t1", "w1", now, orgLim); !ok {
			t.Fatalf("org call %d should be allowed", i)
		}
	}
	if ok, _ := c.Allow(ctx, "t1", "w1", now, orgLim); ok {
		t.Fatal("4th org call should be denied")
	}
	if n, _ := c.OrgCount(ctx, "t1", now); n != 3 {
		t.Fatalf("denied call must not consume quota, count=%d", n)
	}

	whLim := Limits{PerOrgMonthly: 100, PerWebhookMonthly: 1}
	if ok, _ := c.Allow(ctx, "t2", "wA", now, whLim); !ok {
		t.Fatal("first per-webhook call ok")
	}
	if ok, _ := c.Allow(ctx, "t2", "wA", now, whLim); ok {
		t.Fatal("second per-webhook call denied")
	}
	if ok, _ := c.Allow(ctx, "t2", "wB", now, whLim); !ok {
		t.Fatal("a different webhook under the same org should still be allowed")
	}

	// month rollover resets counters.
	if ok, _ := c.Allow(ctx, "t1", "w1", now.AddDate(0, 1, 0), orgLim); !ok {
		t.Fatal("next month should reset the org counter")
	}
}
