package cli

import "testing"

// TestOAuthPath is the CLI half of the credential chain: which link a command
// addresses. Two properties carry the weight.
//
// Rank 0 must be left IMPLICIT — every `remote admin llm oauth` and
// `remote orgs oauth` invocation written before chains existed has to keep
// emitting the byte-identical request, or the feature would silently rewrite
// what those commands mean.
//
// A negative rank must be REFUSED. Dropping it (the shape a `rank > 0` guard
// alone produces) would address the primary — on `delete`, the one credential
// the operator was not aiming at.
func TestOAuthPath(t *testing.T) {
	const base = "/api/admin/llm/oauth/claude_code"

	t.Run("the primary emits the request it always did", func(t *testing.T) {
		got, err := OAuthPath(base, "", 0)
		if err != nil {
			t.Fatalf("rank 0: %v", err)
		}
		if got != base {
			t.Fatalf("rank 0 = %q, want the bare path %q — an implicit primary must add nothing", got, base)
		}
	})

	t.Run("a fallback is named explicitly", func(t *testing.T) {
		got, err := OAuthPath(base, "", 2)
		if err != nil {
			t.Fatalf("rank 2: %v", err)
		}
		if got != base+"?rank=2" {
			t.Fatalf("rank 2 = %q, want %q", got, base+"?rank=2")
		}
	})

	t.Run("label and rank travel together, escaped", func(t *testing.T) {
		got, err := OAuthPath(base, "  jo the dev  ", 1)
		if err != nil {
			t.Fatalf("label+rank: %v", err)
		}
		// url.Values.Encode sorts keys, so account_label precedes rank.
		const want = base + "?account_label=jo+the+dev&rank=1"
		if got != want {
			t.Fatalf("label+rank = %q, want %q", got, want)
		}
	})

	t.Run("a negative rank is refused, never clamped to the primary", func(t *testing.T) {
		got, err := OAuthPath(base, "", -1)
		if err == nil {
			t.Fatalf("rank -1 was accepted and produced %q — it must be refused, not silently addressed at the primary", got)
		}
	})
}
