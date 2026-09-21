package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// runUserSearchSuite exercises the UserFilter contract both Store
// implementations must satisfy. Like the patch suite next door it matters
// more than it looks: the twins reach the same result by opposite means —
// the memory store evaluates a Go predicate over a map, Mongo hands a
// `$or` document to the server — so "prefix" in one and "contains" in the
// other, or an escaped pattern here and a live regex there, would diverge
// silently. Only a suite that runs against BOTH can see it.
func runUserSearchSuite(t *testing.T, s Store) {
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond)

	// Distinct CreatedAt values: both stores order oldest-first, and a tie
	// would leave the order to a map walk here and to the server there —
	// a test that can only redden in one order is a flaky test, not a
	// witness.
	seed := func(id, email string, ageSeconds int) {
		t.Helper()
		if _, err := s.CreateUser(ctx, User{
			ID:        id,
			Email:     email,
			Status:    UserStatusActive,
			CreatedAt: base.Add(time.Duration(ageSeconds) * time.Second),
		}); err != nil {
			t.Fatalf("seed user %s: %v", id, err)
		}
	}

	seed("us-alice", "alice@example.org", 1)
	seed("us-alias", "alias@example.org", 2)
	// Shares the substring "ali" WITHOUT sharing the prefix: this row is
	// what separates a prefix match from a contains match.
	seed("us-mid", "xxALIce@example.org", 3)
	seed("us-bob", "bob@example.org", 4)
	// An id carrying an upper-case byte — the id arm must not lower-case.
	seed("us-MiXeD-Id", "carol@example.org", 5)

	ids := func(t *testing.T, f UserFilter) []string {
		t.Helper()
		got, err := s.ListUsers(ctx, f)
		if err != nil {
			t.Fatalf("ListUsers(%+v): %v", f, err)
		}
		out := make([]string, 0, len(got))
		for _, u := range got {
			out = append(out, u.ID)
		}
		return out
	}

	equal := func(t *testing.T, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}

	t.Run("an empty query lists everyone, oldest first", func(t *testing.T) {
		equal(t, ids(t, UserFilter{}),
			[]string{"us-alice", "us-alias", "us-mid", "us-bob", "us-MiXeD-Id"})
	})

	t.Run("the email arm is a PREFIX, not a substring", func(t *testing.T) {
		// us-mid carries "ALIce" in the middle of its address. A contains
		// match returns it; a prefix match must not.
		equal(t, ids(t, UserFilter{Query: "ali"}), []string{"us-alice", "us-alias"})
	})

	t.Run("the email arm ignores case on both sides", func(t *testing.T) {
		// Upper-case query against a stored normalized address...
		equal(t, ids(t, UserFilter{Query: "ALI"}), []string{"us-alice", "us-alias"})
		// ...and a stored address that was upper-case on the way in (both
		// stores normalize at write time, which this asserts rather than
		// assumes).
		equal(t, ids(t, UserFilter{Query: "xxali"}), []string{"us-mid"})
	})

	t.Run("surrounding whitespace is trimmed, not searched", func(t *testing.T) {
		equal(t, ids(t, UserFilter{Query: "  bob  "}), []string{"us-bob"})
	})

	t.Run("an exact id is its own arm", func(t *testing.T) {
		// "us-bob" is not the prefix of any email here, so only the id arm
		// can return this row.
		equal(t, ids(t, UserFilter{Query: "us-bob"}), []string{"us-bob"})
	})

	t.Run("the id arm is case-SENSITIVE", func(t *testing.T) {
		// An id is an opaque token. Lower-casing the query before comparing
		// it would make this account unfindable by its own id.
		equal(t, ids(t, UserFilter{Query: "us-MiXeD-Id"}), []string{"us-MiXeD-Id"})
		// And the lower-cased spelling must NOT resolve to it — that is what
		// makes the assertion above a witness rather than a coincidence.
		equal(t, ids(t, UserFilter{Query: "us-mixed-id"}), nil)
	})

	t.Run("regex metacharacters are matched literally", func(t *testing.T) {
		// Query is operator input reaching a `$regex`. Unescaped, ".*"
		// widens the match to every row — so this asserting EMPTY is the
		// guard: it reddens the moment QuoteMeta is dropped.
		equal(t, ids(t, UserFilter{Query: ".*"}), nil)
		// Same shape through an alternation and an anchor-defeating prefix.
		equal(t, ids(t, UserFilter{Query: "a|b"}), nil)
		equal(t, ids(t, UserFilter{Query: ".*@example.org"}), nil)
	})

	t.Run("a query matching nothing returns nothing", func(t *testing.T) {
		equal(t, ids(t, UserFilter{Query: "nobody"}), nil)
	})

	t.Run("the page applies AFTER the filter", func(t *testing.T) {
		// Two of the five rows carry the "a" prefix, and they are not the
		// two the page would land on if it were applied to the full list
		// first — so an implementation that pages before it filters returns
		// a different set here.
		equal(t, ids(t, UserFilter{Query: "a"}), []string{"us-alice", "us-alias"})
		equal(t, ids(t, UserFilter{Page: Page{Limit: 1}, Query: "a"}), []string{"us-alice"})
		equal(t, ids(t, UserFilter{Page: Page{Offset: 1, Limit: 1}, Query: "a"}), []string{"us-alias"})
		equal(t, ids(t, UserFilter{Page: Page{Offset: 2, Limit: 1}, Query: "a"}), nil)
	})
}

func TestMemoryStore_UserSearch(t *testing.T) {
	runUserSearchSuite(t, NewMemoryStore())
}

// TestMongoStore_UserSearch runs the shared suite against a real Mongo,
// gated on ITERION_TEST_MONGO_URI like its sibling suites. The
// mongo-conformance CI job already runs ./pkg/identity/..., so this is the
// half that makes the `$or` + `$regex` path executable at all.
func TestMongoStore_UserSearch(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo identity user-search suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_identity_usersearch_" + hex.EncodeToString(nonce))
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
	runUserSearchSuite(t, st)
}
