package secrets

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// blockingOAuthServer answers the token endpoint but parks the FIRST
// request until release is closed, so a test can hold one refresher inside
// its provider round trip — the window every claim in this file exists to
// fence — and act on the store while it is stuck there.
type blockingOAuthServer struct {
	*httptest.Server
	hits    int32
	entered chan struct{}
	release chan struct{}
	relOnce sync.Once
}

func newBlockingOAuthServer(t *testing.T, body string) *blockingOAuthServer {
	t.Helper()
	f := &blockingOAuthServer{entered: make(chan struct{}, 1), release: make(chan struct{})}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&f.hits, 1) == 1 {
			f.entered <- struct{}{}
			<-f.release
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	// Unblock before Close on EVERY exit path: a t.Fatal between the two
	// would otherwise leave the parked request in flight, and
	// httptest.Server.Close waits for it — turning an assertion failure
	// into a ten-minute hang with no message.
	t.Cleanup(func() {
		f.Unblock()
		f.Close()
	})
	return f
}

func (f *blockingOAuthServer) Unblock() { f.relOnce.Do(func() { close(f.release) }) }

// Every server replica runs its own OAuthRefreshWorker on the same records,
// with no leader election — so "one credential, one refresher" is only true
// if the record itself elects one. It has to be true: OpenAI retires the
// refresh token it is handed, so a second concurrent exchange leaves one
// holder with a credential the provider already invalidated, and the
// deployment's shared codex forfait dies until a human re-connects it.
//
// The oracle is the PROVIDER's hit count, not the stored record: two
// exchanges that both "succeed" leave a perfectly plausible record behind
// and a retired refresh token nobody can see.
func TestOAuthRefreshWorker_ConcurrentSweepsExchangeOnce(t *testing.T) {
	freshRetrySchedule(t)
	sealer, _ := NewAESGCMSealer(make([]byte, 32))
	st := NewMemoryOAuthStore()
	seedCodexRecord(t, st, sealer, "alice", time.Now().Add(5*time.Minute))

	newExp := time.Now().Add(6 * time.Hour).UTC().Truncate(time.Second)
	srv := newBlockingOAuthServer(t, `{"access_token":"`+
		fakeJWT(t, map[string]any{"exp": newExp.Unix(), "client_id": "app_derived"})+
		`","refresh_token":"rt.rotated"}`)

	worker := func() *OAuthRefreshWorker {
		return &OAuthRefreshWorker{Store: st, Sealer: sealer, HTTP: redirectingClient(srv.URL), Lead: 30 * time.Minute}
	}

	type sweep struct {
		n   int
		err error
	}
	first := make(chan sweep, 1)
	go func() {
		n, err := worker().RunOnce(context.Background())
		first <- sweep{n, err}
	}()

	// Replica A is now inside the exchange, holding the claim.
	select {
	case <-srv.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("provider was never called: the first sweep never reached the exchange")
	}

	// Replica B sweeps the same record while A is mid-flight. It must find
	// the claim held and leave the provider alone.
	n, err := worker().RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("second sweep refreshed %d records, want 0: it must not exchange behind a live claim", n)
	}

	srv.Unblock()
	got := <-first
	if got.err != nil {
		t.Fatalf("first sweep: %v", got.err)
	}
	if got.n != 1 {
		t.Fatalf("first sweep refreshed %d records, want 1", got.n)
	}
	if hits := atomic.LoadInt32(&srv.hits); hits != 1 {
		t.Fatalf("provider was called %d times for one record, want 1 — the second exchange "+
			"retires the refresh token the first one stored", hits)
	}
}

// The claim also has to fence the COMMIT, not only the exchange: a
// re-connect during the provider round trip installs a new session, and the
// in-flight refresher is holding tokens minted from the session that one
// replaced. Writing them would silently undo an operator's upload — the
// credential they just connected, replaced by a refresh of the dead one.
func TestOAuthRefreshWorker_ReconnectDuringExchangeKeepsTheNewCredential(t *testing.T) {
	freshRetrySchedule(t)
	sealer, _ := NewAESGCMSealer(make([]byte, 32))
	st := NewMemoryOAuthStore()
	seedCodexRecord(t, st, sealer, "alice", time.Now().Add(5*time.Minute))

	newExp := time.Now().Add(6 * time.Hour).UTC().Truncate(time.Second)
	srv := newBlockingOAuthServer(t, `{"access_token":"`+
		fakeJWT(t, map[string]any{"exp": newExp.Unix(), "client_id": "app_derived"})+
		`","refresh_token":"rt.from-the-old-session"}`)

	w := &OAuthRefreshWorker{Store: st, Sealer: sealer, HTTP: redirectingClient(srv.URL), Lead: 30 * time.Minute}
	done := make(chan error, 1)
	go func() {
		_, err := w.RunOnce(context.Background())
		done <- err
	}()
	select {
	case <-srv.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("provider was never called")
	}

	// The operator re-connects while the refresh is in flight. The connect
	// path replaces the record wholesale — which is what clears the claim.
	reconnected := codexBlob(t, fakeJWT(t, map[string]any{
		"exp": time.Now().Add(90 * time.Minute).Unix(), "client_id": "app_derived",
	}), "")
	sealed, err := SealOAuthPayload(sealer, "alice", OAuthKindCodex, reconnected)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	exp := time.Now().Add(90 * time.Minute).UTC()
	if err := st.Upsert(context.Background(), OAuthRecord{
		UserID: "alice", Kind: OAuthKindCodex, SealedPayload: sealed, AccessTokenExpiresAt: &exp,
	}); err != nil {
		t.Fatalf("re-connect upsert: %v", err)
	}

	srv.Unblock()
	if err := <-done; err != nil {
		t.Fatalf("sweep: %v", err)
	}

	rec, err := st.Get(context.Background(), "alice", OAuthKindCodex)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	plain, err := OpenOAuthPayload(sealer, "alice", OAuthKindCodex, rec.SealedPayload)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	view, err := ParseCodexView(plain)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if view.Tokens.RefreshToken == "rt.from-the-old-session" {
		t.Fatal("the in-flight refresh overwrote the credential the operator had just connected: " +
			"its tokens belong to the session that upload replaced")
	}
}

// A refresh that succeeds but yields no readable deadline used to leave the
// record's PAST expiry in place — and the record was selected precisely
// because that expiry is past, so every sweep re-ran the exchange, every 10
// minutes, for good. Each one rotates the refresh token at OpenAI.
//
// The oracle is again the provider: one exchange for two sweeps. The stored
// expiry must stay the truthful past one — nothing may invent a deadline
// for a token that states none.
func TestOAuthRefreshWorker_UndatableCodexRefreshStopsRepeating(t *testing.T) {
	freshRetrySchedule(t)
	sealer, _ := NewAESGCMSealer(make([]byte, 32))
	st := NewMemoryOAuthStore()
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	seedCodexRecord(t, st, sealer, "alice", past)

	// An opaque access token: no expires_in, no `exp` claim to read.
	srv := newFakeOAuthServer(`{"access_token":"opaque-access-token-value","refresh_token":"rt.rotated"}`, http.StatusOK)
	defer srv.Close()

	w := &OAuthRefreshWorker{Store: st, Sealer: sealer, HTTP: redirectingClient(srv.URL), Lead: 30 * time.Minute}
	if n, err := w.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("first sweep: n=%d err=%v, want 1 refreshed", n, err)
	}
	if n, err := w.RunOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("second sweep: n=%d err=%v, want 0 — the record must be cooling down", n, err)
	}
	if hits := atomic.LoadInt32(&srv.hits); hits != 1 {
		t.Fatalf("provider called %d times over two sweeps, want 1: an undatable refresh must not "+
			"re-run on every tick, rotating the refresh token each time", hits)
	}

	rec, err := st.Get(context.Background(), "alice", OAuthKindCodex)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec.AccessTokenExpiresAt == nil || !rec.AccessTokenExpiresAt.UTC().Equal(past) {
		t.Fatalf("stored expiry = %v, want the truthful past %s: the cool-down is a retry cadence, "+
			"never a claimed token lifetime", rec.AccessTokenExpiresAt, past)
	}
	if rec.RefreshNotBefore == nil || !rec.RefreshNotBefore.After(time.Now()) {
		t.Fatalf("RefreshNotBefore = %v, want a future cool-down", rec.RefreshNotBefore)
	}
	if rec.RefreshClaimOwner != "" {
		t.Fatalf("claim owner = %q after a committed refresh, want released", rec.RefreshClaimOwner)
	}
}

// The claim's own contract, at the store: one holder at a time, a dead
// holder's claim reclaimable once its lease passes, and release/commit both
// conditional on still owning it — the property that makes a slow refresher
// unable to free (or overwrite) its successor's work.
func TestMemoryOAuthStore_RefreshClaimIsFencedAndSelfHealing(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryOAuthStore()
	exp := time.Now().Add(time.Minute)
	if err := st.Upsert(ctx, OAuthRecord{UserID: "alice", Kind: OAuthKindCodex,
		SealedPayload: []byte("sealed"), AccessTokenExpiresAt: &exp}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := time.Now().UTC()

	ok, err := st.ClaimRefresh(ctx, OAuthRecordID("alice", OAuthKindCodex, 0), "owner-a", now, now.Add(2*time.Minute))
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v, want acquired", ok, err)
	}
	ok, err = st.ClaimRefresh(ctx, OAuthRecordID("alice", OAuthKindCodex, 0), "owner-b", now, now.Add(2*time.Minute))
	if err != nil || ok {
		t.Fatalf("second claim: ok=%v err=%v, want refused while owner-a holds it", ok, err)
	}
	if err := st.ReleaseRefreshClaim(ctx, OAuthRecordID("alice", OAuthKindCodex, 0), "owner-b", nil); !errors.Is(err, ErrRefreshClaimLost) {
		t.Fatalf("release by a non-owner = %v, want ErrRefreshClaimLost: it must not free owner-a's claim", err)
	}
	if err := st.UpdateTokens(ctx, OAuthRecordID("alice", OAuthKindCodex, 0),
		OAuthTokenUpdate{SealedPayload: []byte("from-owner-b")}.WithClaim("owner-b")); !errors.Is(err, ErrRefreshClaimLost) {
		t.Fatalf("commit by a non-owner = %v, want ErrRefreshClaimLost", err)
	}
	rec, _ := st.Get(ctx, "alice", OAuthKindCodex)
	if string(rec.SealedPayload) != "sealed" {
		t.Fatalf("payload = %q: a refused commit must write nothing", rec.SealedPayload)
	}

	// owner-a dies without releasing: the lease expiring is what unblocks
	// the record, so nothing needs a janitor.
	later := now.Add(3 * time.Minute)
	ok, err = st.ClaimRefresh(ctx, OAuthRecordID("alice", OAuthKindCodex, 0), "owner-c", later, later.Add(2*time.Minute))
	if err != nil || !ok {
		t.Fatalf("claim after the lease expired: ok=%v err=%v, want acquired", ok, err)
	}
	if err := st.UpdateTokens(ctx, OAuthRecordID("alice", OAuthKindCodex, 0),
		OAuthTokenUpdate{SealedPayload: []byte("from-owner-c")}.WithClaim("owner-c")); err != nil {
		t.Fatalf("commit by the holder: %v", err)
	}
	rec, _ = st.Get(ctx, "alice", OAuthKindCodex)
	if string(rec.SealedPayload) != "from-owner-c" {
		t.Fatalf("payload = %q, want the holder's write", rec.SealedPayload)
	}
	if rec.RefreshClaimOwner != "" || rec.RefreshNotBefore != nil {
		t.Fatalf("claim not released by the commit: owner=%q notBefore=%v", rec.RefreshClaimOwner, rec.RefreshNotBefore)
	}
}

// The fence lives in the Mongo write's FILTER, which is the half of the
// write that leaves no trace: an unfenced UpdateOne matches every time and
// the record it leaves is indistinguishable from a legitimate commit's. So
// a store that built the fenced filter and then sent an unfenced literal
// compiled, passed every claim test above (they run on the memory twin,
// which enforces the fence in Go) and shipped a promise it did not keep —
// the only suite that could see it, the Mongo conformance one, skips
// without ITERION_TEST_MONGO_URI.
//
// Assert the pair the store actually sends, with no Mongo at all.
func TestOAuthTokenUpdateWrite_FencesOnTheClaim(t *testing.T) {
	now := time.Now().UTC()
	cool := now.Add(time.Hour)

	id := OAuthRecordID("alice", OAuthKindCodex, 1)
	filter, update := oauthTokenUpdateWrite(id,
		OAuthTokenUpdate{SealedPayload: []byte("sealed-v2"), RefreshNotBefore: &cool}.WithClaim("owner-a"), now)
	// The write addresses ONE record by its id. A filter that still keyed on
	// (user_id, kind) would match the chain's PRIMARY whatever link the
	// refresh was called for — renewing rank 0 forever while the fallback it
	// held a claim on expired, with every claim test still green.
	if got := filter["_id"]; got != id {
		t.Fatalf("filter _id = %v, want %v (the record the refresh claimed)", got, id)
	}
	if _, keyedOnPair := filter["user_id"]; keyedOnPair {
		t.Fatal("filter still carries user_id — that pair stopped identifying one record when a chain became possible")
	}
	if got := filter["refresh_claim_owner"]; got != "owner-a" {
		t.Fatalf("fenced commit filter refresh_claim_owner = %v, want owner-a — without it a superseded "+
			"holder's tokens overwrite the credential an operator's re-connect just installed", got)
	}
	set, ok := update["$set"].(bson.M)
	if !ok {
		t.Fatalf("update = %v, want a $set body", update)
	}
	if got := set["refresh_claim_owner"]; got != "" {
		t.Fatalf("$set refresh_claim_owner = %v, want the claim released by the same write", got)
	}
	if got, isTime := set["refresh_not_before"].(time.Time); !isTime || !got.Equal(cool) {
		t.Fatalf("$set refresh_not_before = %v, want the cool-down %v", set["refresh_not_before"], cool)
	}
	if got := set["sealed_payload"]; string(got.([]byte)) != "sealed-v2" {
		t.Fatalf("$set sealed_payload = %v, want the refreshed blob", got)
	}

	// The unfenced shape (the self-heal, which takes no claim) must neither
	// fence on a claim it does not hold nor clear the holder's.
	filter, update = oauthTokenUpdateWrite(OAuthRecordID("alice", OAuthKindCodex, 0), OAuthTokenUpdate{NotRefreshable: true}, now)
	if _, fenced := filter["refresh_claim_owner"]; fenced {
		t.Fatalf("unfenced write filtered on a claim: %v", filter)
	}
	set, _ = update["$set"].(bson.M)
	for _, k := range []string{"refresh_claim_owner", "refresh_not_before"} {
		if _, wrote := set[k]; wrote {
			t.Fatalf("unfenced write touched %q: %v — it would free (or re-stamp) a live holder's claim", k, set)
		}
	}
}
