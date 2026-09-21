package identity

import (
	"regexp"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// regexArmOf digs the email arm's pattern out of a userQueryFilter
// document, failing loudly rather than returning a zero value — a helper
// that quietly yields "" would let every assertion below pass against a
// filter shape that changed.
func regexArmOf(t *testing.T, f bson.M) string {
	t.Helper()
	or, ok := f["$or"].([]bson.M)
	if !ok || len(or) != 2 {
		t.Fatalf("filter is not a two-armed $or: %#v", f)
	}
	arm, ok := or[0]["email"].(bson.M)
	if !ok {
		t.Fatalf("first arm is not an email match: %#v", or[0])
	}
	pattern, ok := arm["$regex"].(string)
	if !ok {
		t.Fatalf("email arm carries no $regex string: %#v", arm)
	}
	return pattern
}

// TestUserQueryFilterIsLiteral holds the one property the conformance
// suite can only observe indirectly: Query reaches a `$regex`, so every
// metacharacter in it must arrive at the server as a literal.
//
// The suite next door asserts the OUTCOME against a real Mongo — which is
// the guarantee, since MongoDB evaluates the pattern with PCRE2 and this
// test compiles it with Go's RE2. Keep both: this one needs no server, it
// runs on every `go test ./...`, and it names what breaks — an operator's
// search box feeding an unescaped pattern into the database.
func TestUserQueryFilterIsLiteral(t *testing.T) {
	for _, q := range []string{".*", "a|b", "^admin", "(x)+", "[a-z]", `\d`, ".*@example.org"} {
		t.Run(q, func(t *testing.T) {
			pattern := regexArmOf(t, userQueryFilter(q))

			want := "^" + regexp.QuoteMeta(NormalizeEmail(q))
			if pattern != want {
				t.Fatalf("pattern %q, want %q", pattern, want)
			}

			// And the consequence, stated as behaviour rather than as
			// spelling: compiled, the pattern must select only addresses
			// that literally begin with the query. An unescaped ".*" or
			// "a|b" matches this foreign address; the escaped form cannot.
			re, err := regexp.Compile(pattern)
			if err != nil {
				t.Fatalf("compile %q: %v", pattern, err)
			}
			if re.MatchString("zzz@other.test") {
				t.Fatalf("pattern %q matches an unrelated address — the query was not escaped", pattern)
			}
		})
	}
}

// TestUserQueryFilterEmptyMatchesEveryone pins the zero value: an empty
// query must produce an EMPTY filter document, not a `$or` that happens to
// match everything. A console that lists users with no search box relies on
// it, and a filter that silently narrowed would read as "no users yet".
func TestUserQueryFilterEmptyMatchesEveryone(t *testing.T) {
	for _, q := range []string{"", "   ", "\t\n"} {
		got := userQueryFilter(q)
		if len(got) != 0 {
			t.Fatalf("userQueryFilter(%q) = %#v, want an empty document", q, got)
		}
	}
}

// TestUserQueryFilterIDArmIsVerbatim guards the asymmetry between the two
// arms: the email is normalized (its unique index is on the normalized
// form) and the id is not (it is an opaque token). Folding the id would
// make an account carrying an upper-case byte in its id unfindable by that
// id — the exact lookup an operator reaches for when an email is ambiguous.
func TestUserQueryFilterIDArmIsVerbatim(t *testing.T) {
	or, ok := userQueryFilter("  us-MiXeD-Id  ")["$or"].([]bson.M)
	if !ok || len(or) != 2 {
		t.Fatalf("filter is not a two-armed $or")
	}
	if got := or[1]["_id"]; got != "us-MiXeD-Id" {
		t.Fatalf("id arm = %#v, want the trimmed query verbatim", got)
	}
}
