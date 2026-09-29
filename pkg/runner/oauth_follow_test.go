package runner

import (
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

	if changed, err := r.followOAuthRecordOnce(secrets.OAuthKindClaudeCode, id, "fp-team", path); err != nil || changed {
		t.Fatalf("an unrotated record changed the file: changed=%v err=%v", changed, err)
	}
	after := claudeBlob("at.after", time.Now().Add(8*time.Hour))
	rotate(t, r, st, id, after, "fp-team")
	changed, err := r.followOAuthRecordOnce(secrets.OAuthKindClaudeCode, id, "fp-team", path)
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
		r.followOAuthRecordLoop(stop, "run-live", secrets.OAuthKindClaudeCode, id, "fp-team", path, 10*time.Millisecond)
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

// TestFollowOAuthRecordOnce_RefusesAnotherSubscription: a record that now
// names another subscription is not followed — that would switch the account
// the run spends on mid-run.
func TestFollowOAuthRecordOnce_RefusesAnotherSubscription(t *testing.T) {
	before := claudeBlob("at.before", time.Now().Add(time.Hour))
	r, st, id, _ := followFixture(t, before, "fp-team")
	path := materialise(t, before)
	rotate(t, r, st, id, claudeBlob("at.other-account", time.Now().Add(8*time.Hour)), "fp-another-account")
	changed, err := r.followOAuthRecordOnce(secrets.OAuthKindClaudeCode, id, "fp-team", path)
	if err == nil || changed || !strings.Contains(err.Error(), "another subscription") {
		t.Fatalf("a record naming another subscription was followed: changed=%v err=%v", changed, err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(before) {
		t.Fatalf("the file changed: %s", got)
	}
}

// TestFollowOAuthRecordOnce_RefusesAReconnectedLegacyRecord: a record sealed
// before fingerprints were stamped is followed while it has none; a stamp
// appears when a human reconnects it, which may be another account.
func TestFollowOAuthRecordOnce_RefusesAReconnectedLegacyRecord(t *testing.T) {
	before := claudeBlob("at.before", time.Now().Add(time.Hour))
	r, st, id, _ := followFixture(t, before, "")
	path := materialise(t, before)
	rotate(t, r, st, id, claudeBlob("at.rotated", time.Now().Add(8*time.Hour)), "")
	if changed, err := r.followOAuthRecordOnce(secrets.OAuthKindClaudeCode, id, "", path); err != nil || !changed {
		t.Fatalf("an unstamped record's rotation was not followed: changed=%v err=%v", changed, err)
	}
	rotate(t, r, st, id, claudeBlob("at.reconnected", time.Now().Add(8*time.Hour)), "fp-reconnected")
	changed, err := r.followOAuthRecordOnce(secrets.OAuthKindClaudeCode, id, "", path)
	if err == nil || changed || !strings.Contains(err.Error(), "another subscription") {
		t.Fatalf("a reconnected record was followed by a run sealed before stamping: changed=%v err=%v", changed, err)
	}
}

// TestFollowOAuthRecordOnce_RefusesAnotherKind: a ref that resolves to
// another kind of forfait is not followed.
func TestFollowOAuthRecordOnce_RefusesAnotherKind(t *testing.T) {
	before := claudeBlob("at.before", time.Now().Add(time.Hour))
	r, _, id, _ := followFixture(t, before, "fp-team")
	path := materialise(t, before)
	changed, err := r.followOAuthRecordOnce(secrets.OAuthKindCodex, id, "fp-team", path)
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
	r.startOAuthRefreshers(stop, "run-queued", map[string]string{kind: path}, map[string]string{kind: id}, map[string]string{kind: "fp-team"})
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
	rotated := claudeBlob("at.rotated-while-queued", time.Now().Add(8*time.Hour))
	rotate(t, r, st, id, rotated, "fp-team")

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
		map[string]string{kind: id}, map[string]string{kind: "fp-team"})
	time.Sleep(500 * time.Millisecond)
	if got := hits.Load(); got != 0 {
		t.Fatalf("a run whose slot names its record exchanged the refresh token %d time(s)", got)
	}

	r.startOAuthRefreshers(stop, "run-legacy",
		map[string]string{kind: materialise(t, expired)}, nil, nil)
	deadline := time.Now().Add(10 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatal("the self-refresh fallback never ran — the positive control failed, so the assertion above would pass vacuously")
	}
}
