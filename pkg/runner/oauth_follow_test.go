package runner

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// claudeBlob is a materialised Claude Code .credentials.json.
func claudeBlob(access string, exp time.Time) []byte {
	return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"rt.team","expiresAt":%d}}`, access, exp.UnixMilli()))
}

// followFixture seeds a team claude_code record sealed from payload and
// returns a runner wired to the store, the record id and the owner key.
func followFixture(t *testing.T, payload []byte, fp string) (*Runner, *secrets.MemoryOAuthStore, string, string) {
	t.Helper()
	sealer := testSealer(t)
	st := secrets.NewMemoryOAuthStore()
	owner := secrets.OrgOwnerKey("tenant-1")
	sealed, err := secrets.SealOAuthPayload(sealer, owner, secrets.OAuthKindClaudeCode, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert(t.Context(), secrets.OAuthRecord{UserID: owner, Kind: secrets.OAuthKindClaudeCode, SealedPayload: sealed, Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{OAuthForfaits: st, Sealer: sealer}}
	return r, st, secrets.OAuthRecordID(owner, secrets.OAuthKindClaudeCode, 0), owner
}

// rotate rewrites the record's payload the way the server's refresh worker
// does, optionally under another subscription fingerprint.
func rotate(t *testing.T, r *Runner, st *secrets.MemoryOAuthStore, id string, payload []byte, fp string) {
	t.Helper()
	rec, err := st.GetByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	rec.SealedPayload, err = secrets.SealOAuthPayload(r.cfg.Sealer, rec.UserID, rec.Kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	rec.Fingerprint = fp
	if err := st.Upsert(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
}

func materialise(t *testing.T, payload []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestFollowOAuthRecordOnce_PicksUpTheWorkersRotation: the file a run
// materialised follows the record the server's refresh worker rotates.
func TestFollowOAuthRecordOnce_PicksUpTheWorkersRotation(t *testing.T) {
	before := claudeBlob("at.before", time.Now().Add(time.Hour))
	r, st, id, _ := followFixture(t, before, "fp-team")
	path := materialise(t, before)

	if changed, _, err := r.followOAuthRecordOnce(secrets.OAuthKindClaudeCode, id, "", path); err != nil || changed {
		t.Fatalf("an unrotated record changed the file: changed=%v err=%v", changed, err)
	}
	after := claudeBlob("at.after", time.Now().Add(8*time.Hour))
	rotate(t, r, st, id, after, "fp-team")
	changed, _, err := r.followOAuthRecordOnce(secrets.OAuthKindClaudeCode, id, "", path)
	if err != nil || !changed {
		t.Fatalf("the worker's rotation was not picked up: changed=%v err=%v", changed, err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(after) {
		t.Fatalf("the file does not carry the rotated record: %s", got)
	}
}

// TestFollowOAuthRecordLoop_FollowsARotationMidRun: the loop itself, not one
// pass — a rotation the worker makes while the run is live reaches the file.
func TestFollowOAuthRecordLoop_FollowsARotationMidRun(t *testing.T) {
	before := claudeBlob("at.before", time.Now().Add(time.Hour))
	r, st, id, _ := followFixture(t, before, "fp-team")
	path := materialise(t, before)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.followOAuthRecordLoop(stop, "run-live", secrets.OAuthKindClaudeCode, id, "fp-team", "", path, 10*time.Millisecond)
	}()
	defer func() { close(stop); <-done }()

	after := claudeBlob("at.after", time.Now().Add(8*time.Hour))
	rotate(t, r, st, id, after, "fp-team")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := os.ReadFile(path); string(got) == string(after) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the loop never picked up a rotation made while the run was live")
}

// rotatedToken is long enough for RefreshRecord's plausibility check.
var rotatedToken = "sk-ant-oat01-rotated-" + strings.Repeat("x", 48)

// TestFollowOAuthRecordOnce_FollowsTheWorkersReStamps: the refresh worker
// re-stamps a record's fingerprint on its own rotations. A run sealed before
// the re-stamp follows the rotation all the same — it runs the REAL sweep
// (claim, RefreshRecord, UpdateTokens) against a token and a profile
// endpoint.
func TestFollowOAuthRecordOnce_FollowsTheWorkersReStamps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(req.URL.Path, "/profile") {
			fmt.Fprint(w, `{"account":{"uuid":"c7f30bce-7e8b-4bb7-8199-afc172f3d580","email":"team@example.org"},"organization":{"uuid":"0d05ce64-6b67-4368-909c-0d26f5a5870a"}}`)
			return
		}
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rt.rotated","expires_in":28800}`, rotatedToken)
	}))
	defer srv.Close()
	t.Setenv("ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL", srv.URL+"/token")
	t.Setenv("ITERION_OAUTH_FORFAIT_ANTHROPIC_PROFILE_URL", srv.URL+"/profile")

	for _, tc := range []struct {
		name   string
		sealed string
		scopes []string
	}{
		{"an unstamped record is stamped", "", nil},
		{"a subscription is identified as an account", "fp-subscription", []string{"user:profile"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := claudeBlob("at.before", time.Now().Add(time.Hour))
			r, st, id, _ := followFixture(t, before, tc.sealed)
			rec, err := st.GetByID(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			rec.Scopes = tc.scopes
			if err := st.Upsert(t.Context(), rec); err != nil {
				t.Fatal(err)
			}
			path := materialise(t, before)

			w := &secrets.OAuthRefreshWorker{Store: st, Sealer: r.cfg.Sealer, HTTP: srv.Client(), AnthropicClientID: "client-test"}
			if n, err := w.RunOnce(t.Context()); err != nil || n != 1 {
				t.Fatalf("the worker's sweep: refreshed %d, err %v", n, err)
			}
			if rec, _ = st.GetByID(t.Context(), id); rec.Fingerprint == tc.sealed {
				t.Fatalf("the worker kept fingerprint %q — the fixture no longer re-stamps, so this proves nothing", rec.Fingerprint)
			}
			// The pass the loop and the catch-up run, sealed fingerprint in hand.
			r.followOAuthRecord("run-restamp", secrets.OAuthKindClaudeCode, id, tc.sealed, "", path)
			if got, _ := os.ReadFile(path); !strings.Contains(string(got), rotatedToken) {
				t.Fatalf("the worker's re-stamped rotation (fingerprint %q, sealed %q) was not followed: %s", rec.Fingerprint, tc.sealed, got)
			}
		})
	}
}

// TestFollowOAuthRecord_FollowsAReconnectAndSaysSo: a reconnect of the slot
// is its owner's choice of credential; the run follows the slot, and the
// move of its fingerprint is logged, not silent.
func TestFollowOAuthRecord_FollowsAReconnectAndSaysSo(t *testing.T) {
	before := claudeBlob("at.before", time.Now().Add(time.Hour))
	r, st, id, _ := followFixture(t, before, "fp-team")
	var out bytes.Buffer
	r.cfg.Logger = iterlog.New(iterlog.LevelInfo, &out)
	path := materialise(t, before)
	reconnected := claudeBlob("at.reconnected", time.Now().Add(8*time.Hour))
	rotate(t, r, st, id, reconnected, "fp-another-account")

	r.followOAuthRecord("run-reconnect", secrets.OAuthKindClaudeCode, id, "fp-team", "", path)
	if got, _ := os.ReadFile(path); string(got) != string(reconnected) {
		t.Fatalf("the reconnected slot was not followed: %s", got)
	}
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, "fp-another-account") && strings.Contains(l, "fp-team") {
			return
		}
	}
	t.Fatalf("the fingerprint move was not logged:\n%s", out.String())
}

// TestFollowOAuthRecord_aLentSlotFollowsOnlyTheLentSubscription: a lent
// slot follows the donor's record through its rotations, and stops at a
// record that names another subscription — in-flight runs finish on the
// credential they were granted, never on one the donor connects after.
func TestFollowOAuthRecord_aLentSlotFollowsOnlyTheLentSubscription(t *testing.T) {
	before := claudeBlob("at.before", time.Now().Add(time.Hour))
	r, st, id, _ := followFixture(t, before, "fp-lent")
	path := materialise(t, before)

	rotated := claudeBlob("at.rotated", time.Now().Add(8*time.Hour))
	rotate(t, r, st, id, rotated, "fp-lent")
	if !r.followOAuthRecord("run-borrower", secrets.OAuthKindClaudeCode, id, "fp-lent", "fp-lent", path) {
		t.Fatal("a rotation of the lent subscription ended the follow")
	}
	if got, _ := os.ReadFile(path); string(got) != string(rotated) {
		t.Fatalf("the lent subscription's rotation was not followed: %s", got)
	}

	rotate(t, r, st, id, claudeBlob("at.never-lent", time.Now().Add(8*time.Hour)), "fp-another-account")
	if r.followOAuthRecord("run-borrower", secrets.OAuthKindClaudeCode, id, "fp-lent", "fp-lent", path) {
		t.Fatal("the follow went on past a record that names another subscription")
	}
	if got, _ := os.ReadFile(path); string(got) != string(rotated) {
		t.Fatalf("a subscription that was never lent reached the borrower's file: %s", got)
	}
}

// TestFollowOAuthRecordLoop_aLentSlotStopsAtAReconnect: the loop ends at the
// refusal instead of re-reading the donor's record every minute.
func TestFollowOAuthRecordLoop_aLentSlotStopsAtAReconnect(t *testing.T) {
	before := claudeBlob("at.before", time.Now().Add(time.Hour))
	r, st, id, _ := followFixture(t, before, "fp-lent")
	path := materialise(t, before)
	rotate(t, r, st, id, claudeBlob("at.never-lent", time.Now().Add(8*time.Hour)), "fp-another-account")
	stop := make(chan struct{})
	defer close(stop)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.followOAuthRecordLoop(stop, "run-borrower", secrets.OAuthKindClaudeCode, id, "fp-lent", "fp-lent", path, 10*time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop kept following a lent slot past the donor's reconnect")
	}
}

// TestFollowOAuthRecordOnce_RefusesAnotherKind: a ref that resolves to
// another kind of forfait is not followed.
func TestFollowOAuthRecordOnce_RefusesAnotherKind(t *testing.T) {
	before := claudeBlob("at.before", time.Now().Add(time.Hour))
	r, _, id, _ := followFixture(t, before, "fp-team")
	path := materialise(t, before)
	changed, _, err := r.followOAuthRecordOnce(secrets.OAuthKindCodex, id, "", path)
	if err == nil || changed {
		t.Fatalf("a claude_code record was followed as a codex one: changed=%v err=%v", changed, err)
	}
}

// TestStartOAuthRefreshers_CatchesUpBeforeTheFirstSpawn: a bundle can wait in
// the queue past a rotation of its record. The file is brought up to date
// before startOAuthRefreshers returns — before the run's first spawn — not a
// follow interval later.
func TestStartOAuthRefreshers_CatchesUpBeforeTheFirstSpawn(t *testing.T) {
	sealedWith := claudeBlob("at.sealed", time.Now().Add(time.Hour))
	r, st, id, _ := followFixture(t, sealedWith, "fp-team")
	path := materialise(t, sealedWith)
	rotated := claudeBlob("at.rotated-while-queued", time.Now().Add(8*time.Hour))
	rotate(t, r, st, id, rotated, "fp-team")

	stop := make(chan struct{})
	defer close(stop)
	kind := string(secrets.OAuthKindClaudeCode)
	r.startOAuthRefreshers(stop, "run-queued", map[string]string{kind: path}, map[string]string{kind: id}, map[string]string{kind: "fp-team"}, nil)
	if got, _ := os.ReadFile(path); string(got) != string(rotated) {
		t.Fatalf("the run starts on the token its bundle was sealed with, rotated since: %s", got)
	}
}

// TestInjectCredentials_FollowsTheRecordTheBundleNames: the guarantee through
// the path a run takes — a sealed bundle whose slot names its record reaches
// the follower, so the materialised file is the record's, rotated since the
// bundle was sealed.
func TestInjectCredentials_FollowsTheRecordTheBundleNames(t *testing.T) {
	sealedWith := claudeBlob("at.sealed", time.Now().Add(time.Hour))
	r, st, id, _ := followFixture(t, sealedWith, "fp-team")
	// Rotated AND re-stamped, the way the refresh worker identifies an
	// account: the team's own slot follows it.
	rotated := claudeBlob("at.rotated-while-queued", time.Now().Add(8*time.Hour))
	rotate(t, r, st, id, rotated, "fp-team-restamped")

	kind := string(secrets.OAuthKindClaudeCode)
	sealed, err := secrets.SealRunBundle(r.cfg.Sealer, "run-1", secrets.RunBundle{
		OAuthCredentials:  map[string][]byte{kind: sealedWith},
		OAuthFingerprints: map[string]string{kind: "fp-team"},
		OAuthRecordRefs:   map[string]string{kind: id},
	})
	if err != nil {
		t.Fatal(err)
	}
	rs := secrets.NewMemoryRunSecretsStore()
	if err := rs.Put(context.Background(), secrets.RunSecretsRecord{ID: "ref-1", TenantID: "tenant-1", RunID: "run-1", SealedBundle: sealed}); err != nil {
		t.Fatal(err)
	}
	r.cfg.RunSecrets = rs
	r.cfg.Logger = iterlog.Nop()

	ctx, cleanup, err := r.injectCredentials(context.Background(), &queue.RunMessage{RunID: "run-1", TenantID: "tenant-1", SecretsRef: "ref-1"})
	if err != nil {
		t.Fatalf("injectCredentials: %v", err)
	}
	defer cleanup()
	creds, _ := secrets.CredentialsFromContext(ctx)
	got, err := os.ReadFile(filepath.Join(creds.OAuthCredentialFiles[kind], ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(rotated) {
		t.Fatalf("the run was handed the token its bundle was sealed with, not its record's: %s", got)
	}
}

// TestInjectCredentials_aLentSlotIsHeldToItsSubscription: the lent flag
// travels from the sealed bundle to the follower — a donor who reconnected
// another subscription before the borrower's run was claimed does not reach
// it.
func TestInjectCredentials_aLentSlotIsHeldToItsSubscription(t *testing.T) {
	sealedWith := claudeBlob("at.sealed", time.Now().Add(time.Hour))
	r, st, id, _ := followFixture(t, sealedWith, "fp-lent")
	rotate(t, r, st, id, claudeBlob("at.never-lent", time.Now().Add(8*time.Hour)), "fp-another-account")

	kind := string(secrets.OAuthKindClaudeCode)
	sealed, err := secrets.SealRunBundle(r.cfg.Sealer, "run-1", secrets.RunBundle{
		OAuthCredentials:  map[string][]byte{kind: sealedWith},
		OAuthFingerprints: map[string]string{kind: "fp-lent"},
		OAuthRecordRefs:   map[string]string{kind: id},
		PoolSourced:       map[string]bool{kind: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	rs := secrets.NewMemoryRunSecretsStore()
	if err := rs.Put(context.Background(), secrets.RunSecretsRecord{ID: "ref-1", TenantID: "tenant-1", RunID: "run-1", SealedBundle: sealed}); err != nil {
		t.Fatal(err)
	}
	r.cfg.RunSecrets = rs
	r.cfg.Logger = iterlog.Nop()
	ctx, cleanup, err := r.injectCredentials(context.Background(), &queue.RunMessage{RunID: "run-1", TenantID: "tenant-1", SecretsRef: "ref-1"})
	if err != nil {
		t.Fatalf("injectCredentials: %v", err)
	}
	defer cleanup()
	creds, _ := secrets.CredentialsFromContext(ctx)
	got, err := os.ReadFile(filepath.Join(creds.OAuthCredentialFiles[kind], ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sealedWith) {
		t.Fatalf("a borrower's run was handed a subscription the donor never lent: %s", got)
	}
}

// TestStartOAuthRefreshers_aLentSlotWithoutFingerprintIsNotFollowed: a lent
// slot can only be held to its subscription by its fingerprint; without one
// it is not followed at all, and keeps the self-refresh it always had.
func TestStartOAuthRefreshers_aLentSlotWithoutFingerprintIsNotFollowed(t *testing.T) {
	sealedWith := claudeBlob("at.sealed", time.Now().Add(time.Hour))
	r, st, id, _ := followFixture(t, sealedWith, "")
	rotate(t, r, st, id, claudeBlob("at.never-lent", time.Now().Add(8*time.Hour)), "fp-another-account")
	path := materialise(t, sealedWith)
	stop := make(chan struct{})
	defer close(stop)
	kind := string(secrets.OAuthKindClaudeCode)
	r.startOAuthRefreshers(stop, "run-borrower", map[string]string{kind: path}, map[string]string{kind: id}, map[string]string{kind: ""}, map[string]bool{kind: true})
	if got, _ := os.ReadFile(path); string(got) != string(sealedWith) {
		t.Fatalf("a lent slot without a fingerprint followed the donor's record: %s", got)
	}
}

// TestStartOAuthRefreshers_FollowsTheRecordInsteadOfExchanging: a slot that
// names its record never exchanges a refresh token — the exchange is what
// revokes the token every other holder of the record still uses. The same
// expired token WITHOUT a ref takes the self-refresh fallback: that is the
// positive control proving the endpoint below would have been hit.
func TestStartOAuthRefreshers_FollowsTheRecordInsteadOfExchanging(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"exchanged","refresh_token":"rt.rotated","expires_in":28800}`)
	}))
	defer srv.Close()
	t.Setenv("ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL", srv.URL)

	expired := claudeBlob("at.expired", time.Now().Add(-time.Hour))
	r, _, id, _ := followFixture(t, expired, "fp-team")
	kind := string(secrets.OAuthKindClaudeCode)

	stop := make(chan struct{})
	defer close(stop)
	r.startOAuthRefreshers(stop, "run-follow",
		map[string]string{kind: materialise(t, expired)},
		map[string]string{kind: id}, map[string]string{kind: "fp-team"}, nil)
	time.Sleep(500 * time.Millisecond)
	if got := hits.Load(); got != 0 {
		t.Fatalf("a run whose slot names its record exchanged the refresh token %d time(s)", got)
	}

	r.startOAuthRefreshers(stop, "run-legacy",
		map[string]string{kind: materialise(t, expired)}, nil, nil, nil)
	deadline := time.Now().Add(10 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatal("the self-refresh fallback never ran — the positive control failed, so the assertion above would pass vacuously")
	}
}
