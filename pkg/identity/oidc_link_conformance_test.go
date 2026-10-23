package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sort"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// runOIDCLinkSuite pins what DeleteSessionOIDCLinks selects, on both twins:
// the links a signed-in session wrote (SessionLineage set) — every one, or
// one lineage's — and never a link an SSO sign-in wrote itself, nor another
// account's.
func runOIDCLinkSuite(t *testing.T, s Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	put := func(subject, user, lineage string) {
		t.Helper()
		if err := s.UpsertOIDCLink(ctx, OIDCLink{Provider: "p", ProviderUserID: subject, UserID: user, CreatedAt: now, SessionLineage: lineage}); err != nil {
			t.Fatalf("upsert %s: %v", subject, err)
		}
	}
	subjects := func(user string) []string {
		t.Helper()
		links, err := s.ListOIDCLinksByUser(ctx, user)
		if err != nil {
			t.Fatalf("list %s: %v", user, err)
		}
		var out []string
		for _, l := range links {
			out = append(out, l.ProviderUserID)
		}
		sort.Strings(out)
		return out
	}
	put("signup", "u1", "")
	put("from-l1", "u1", "l1")
	put("from-l1-too", "u1", "l1")
	put("from-l2", "u1", "l2")
	put("other-account", "u2", "l1")

	if got, err := s.GetOIDCLink(ctx, "p", "from-l1"); err != nil || got.SessionLineage != "l1" {
		t.Fatalf("a session's link reads %+v (%v), want its lineage l1", got, err)
	}
	n, err := s.DeleteSessionOIDCLinks(ctx, "u1", "l1")
	if err != nil || n != 2 {
		t.Fatalf("deleting lineage l1's links of u1: %d (%v), want 2", n, err)
	}
	if got := subjects("u1"); len(got) != 2 || got[0] != "from-l2" || got[1] != "signup" {
		t.Fatalf("after lineage l1's links went u1 links %v, want [from-l2 signup]", got)
	}
	n, err = s.DeleteSessionOIDCLinks(ctx, "u1", "")
	if err != nil || n != 1 {
		t.Fatalf("deleting every session link of u1: %d (%v), want 1", n, err)
	}
	if got := subjects("u1"); len(got) != 1 || got[0] != "signup" {
		t.Fatalf("after every session link went u1 links %v, want [signup]", got)
	}
	if got := subjects("u2"); len(got) != 1 {
		t.Fatalf("another account's link went too: u2 links %v", got)
	}
	if _, err := s.DeleteSessionOIDCLinks(ctx, "", ""); err == nil {
		t.Fatal("a deletion naming no account answered no error")
	}
}

// runOIDCLinkKeepOneSuite pins the keep-one rule on both twins: a removal of
// session-written links never leaves a password-less account without a
// sign-in method it had — its OLDEST link survives — while an account with a
// password keeps none, and a lineage-scoped removal keeps only within its
// scope.
func runOIDCLinkKeepOneSuite(t *testing.T, s Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	put := func(subject, user string, at time.Time, lineage string) {
		t.Helper()
		if err := s.UpsertOIDCLink(ctx, OIDCLink{Provider: "p", ProviderUserID: subject, UserID: user, CreatedAt: at, SessionLineage: lineage}); err != nil {
			t.Fatalf("upsert %s: %v", subject, err)
		}
	}
	subjects := func(user string) []string {
		t.Helper()
		links, err := s.ListOIDCLinksByUser(ctx, user)
		if err != nil {
			t.Fatalf("list %s: %v", user, err)
		}
		var out []string
		for _, l := range links {
			out = append(out, l.ProviderUserID)
		}
		sort.Strings(out)
		return out
	}
	user := func(id, email, hash string) {
		t.Helper()
		if _, err := s.CreateUser(ctx, User{ID: id, Email: email, PasswordHash: hash, Status: UserStatusActive}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	user("u-ssonly", "ssonly@example.com", "")
	put("old", "u-ssonly", now.Add(-2*time.Hour), "lin-a")
	put("mid", "u-ssonly", now.Add(-1*time.Hour), "lin-b")
	put("new", "u-ssonly", now, "lin-c")
	n, err := s.DeleteSessionOIDCLinks(ctx, "u-ssonly", "")
	if err != nil || n != 2 {
		t.Fatalf("deleting every session link of a password-less account: %d (%v), want 2 removed and 1 kept", n, err)
	}
	if got := subjects("u-ssonly"); len(got) != 1 || got[0] != "old" {
		t.Fatalf("the password-less account links %v, want [old] (the oldest it had)", got)
	}

	user("u-pw", "pw@example.com", "hash")
	put("old2", "u-pw", now.Add(-2*time.Hour), "lin-a")
	put("new2", "u-pw", now, "lin-b")
	n, err = s.DeleteSessionOIDCLinks(ctx, "u-pw", "")
	if err != nil || n != 2 {
		t.Fatalf("deleting every session link of an account with a password: %d (%v), want 2", n, err)
	}
	if got := subjects("u-pw"); len(got) != 0 {
		t.Fatalf("an account with a password kept links: %v", got)
	}

	// A lineage-scoped removal keeps only within its scope, and keeps nothing
	// when the account keeps another sign-in method: the sign-up link (no
	// lineage) stays, so both of the lineage's links go.
	user("u-scope", "scope@example.com", "")
	put("signup", "u-scope", now.Add(-3*time.Hour), "")
	put("l1-old", "u-scope", now.Add(-2*time.Hour), "l1")
	put("l1-new", "u-scope", now, "l1")
	n, err = s.DeleteSessionOIDCLinks(ctx, "u-scope", "l1")
	if err != nil || n != 2 {
		t.Fatalf("deleting lineage l1 of a password-less account with a sign-up link: %d (%v), want 2", n, err)
	}
	if got := subjects("u-scope"); len(got) != 1 || got[0] != "signup" {
		t.Fatalf("after the lineage-scoped removal the account links %v, want [signup]", got)
	}

	// The tie: same created_at, the smaller (provider, subject) stays.
	user("u-tie", "tie@example.com", "")
	put("b-sub", "u-tie", now, "l9")
	put("a-sub", "u-tie", now, "l9")
	n, err = s.DeleteSessionOIDCLinks(ctx, "u-tie", "")
	if err != nil || n != 1 {
		t.Fatalf("deleting the tied links: %d (%v), want 1 removed and 1 kept", n, err)
	}
	if got := subjects("u-tie"); len(got) != 1 || got[0] != "a-sub" {
		t.Fatalf("the tie kept %v, want [a-sub]", got)
	}

	// A zero-time upsert — a writer that forgot the stamp — is stamped by
	// the STORE, the same rule both twins apply to the same field, so
	// keep-one keeps the SAME link on both: the first-written zero-time link
	// is the oldest present. The subjects are spelled so the order alone
	// decides (the second's key sorts first), and the writes sit a couple of
	// milliseconds apart so the mongo twin's millisecond dates cannot tie.
	user("u-zero", "zero@example.com", "")
	if err := s.UpsertOIDCLink(ctx, OIDCLink{Provider: "p", ProviderUserID: "z-first", UserID: "u-zero", SessionLineage: "lz"}); err != nil {
		t.Fatalf("upsert the first zero-time link: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := s.UpsertOIDCLink(ctx, OIDCLink{Provider: "p", ProviderUserID: "a-second", UserID: "u-zero", SessionLineage: "lz"}); err != nil {
		t.Fatalf("upsert the second zero-time link: %v", err)
	}
	n, err = s.DeleteSessionOIDCLinks(ctx, "u-zero", "")
	if err != nil || n != 1 {
		t.Fatalf("deleting the zero-time links: %d (%v), want 1 removed and 1 kept", n, err)
	}
	links, lerr := s.ListOIDCLinksByUser(ctx, "u-zero")
	if lerr != nil {
		t.Fatalf("list the zero-time account: %v", lerr)
	}
	if len(links) != 1 || links[0].ProviderUserID != "z-first" {
		t.Fatalf("the zero-time account kept %+v, want [z-first] (the first-written, stamped oldest)", links)
	}
	if links[0].CreatedAt.IsZero() {
		t.Fatalf("the kept link %s carries no created_at: the store left a zero-time upsert unstamped", links[0].ProviderUserID)
	}

	// The real sign-out-everywhere shape: a password-less account whose
	// sign-up link (no lineage) sits beside the links its sessions wrote.
	// The removal takes the session links and nothing else — a sign-up link
	// is a sign-in method the account still holds, so keep-one has nothing
	// to save.
	user("u-signup-side", "signup-side@example.com", "")
	put("signup-side", "u-signup-side", now.Add(-3*time.Hour), "")
	put("sess-old", "u-signup-side", now.Add(-2*time.Hour), "lin-1")
	put("sess-new", "u-signup-side", now, "lin-2")
	n, err = s.DeleteSessionOIDCLinks(ctx, "u-signup-side", "")
	if err != nil || n != 2 {
		t.Fatalf("sign-out-everywhere beside a sign-up link: %d (%v), want 2 session links removed", n, err)
	}
	if got := subjects("u-signup-side"); len(got) != 1 || got[0] != "signup-side" {
		t.Fatalf("the account kept %v, want [signup-side] (the sign-up link survives, no session link is kept)", got)
	}
}

func TestMemoryStore_OIDCLinks(t *testing.T) {
	runOIDCLinkSuite(t, NewMemoryStore())
}

func TestMemoryStore_OIDCLinkKeepOne(t *testing.T) {
	runOIDCLinkKeepOneSuite(t, NewMemoryStore())
}

// TestMongoStore_OIDCLinks runs the shared link suite against a real Mongo,
// gated on ITERION_TEST_MONGO_URI like the sibling suites.
func TestMongoStore_OIDCLinks(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo identity link suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_identity_links_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	st := NewMongoStore(db)
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	runOIDCLinkSuite(t, st)
	runOIDCLinkKeepOneSuite(t, st)
}
