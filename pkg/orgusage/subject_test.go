package orgusage

import (
	"context"
	"testing"
	"time"
)

// OrgSubject's spelling is FROZEN. Before Subject existed, usageKey built the
// document id as "org|" + tenantID + "|" + month, and those documents are
// live: they hold the current month's run count and LLM spend for every
// organisation on the platform. A different spelling would not fail — it
// would silently address a document that does not exist, which reads as a
// quota of zero and a spend of zero, mid-month, for everyone.
//
// The literal below is the OLD format string, written out by hand rather
// than derived from the code under test: a pin computed the same way as the
// thing it pins cannot fail.
func TestOrgSubject_ReproducesTheLegacyDocumentID(t *testing.T) {
	when := time.Date(2026, 9, 22, 15, 4, 5, 0, time.UTC)
	const legacy = "org|team-42|2026-09"
	if got := usageKey(OrgSubject("team-42"), when); got != legacy {
		t.Fatalf("usageKey(OrgSubject(...)) = %q, want the pre-existing id %q — a change here resets every org's live monthly counters to zero", got, legacy)
	}
}

// The two subject kinds must never collide. They share one collection, and a
// collision would let an outside contributor's abuse bound spend (or free) an
// organisation's billing budget.
func TestSubjects_DoNotCollideAcrossKinds(t *testing.T) {
	when := time.Now().UTC()
	org := usageKey(OrgSubject("acme"), when)
	// The adversarial shape: a tenant id chosen to look like the other kind's
	// key body. Distinct kinds must still produce distinct documents.
	fork := usageKey(ForkAuthorSubject("acme", "github", "1234"), when)
	if org == fork {
		t.Fatalf("an org subject and a fork-author subject produced the same document id %q", org)
	}
	// And two contributors, or one contributor on two tenants, are separate
	// budgets — otherwise one attacker exhausts every other contributor's.
	a := usageKey(ForkAuthorSubject("acme", "github", "1234"), when)
	b := usageKey(ForkAuthorSubject("acme", "github", "5678"), when)
	c := usageKey(ForkAuthorSubject("other", "github", "1234"), when)
	if a == b {
		t.Fatal("two different contributors share one budget document")
	}
	if a == c {
		t.Fatal("one contributor shares a budget document across two tenants — a second base repo would inherit the first's spend")
	}
}

// And the bound actually bounds: the counter enforces per-author, not just
// counts. Same Counter, same CAS as the org budget — that is the point of
// reusing the seam.
func TestForkAuthorSubject_IsEnforcedIndependentlyPerAuthor(t *testing.T) {
	c := NewMemoryCounter()
	ctx := context.Background()
	now := time.Now().UTC()
	alice := ForkAuthorSubject("acme", "github", "1111")
	mallory := ForkAuthorSubject("acme", "github", "2222")

	for i := 0; i < 2; i++ {
		if deny, err := c.AllowRun(ctx, mallory, now, 2, 0); err != nil || deny != DenyNone {
			t.Fatalf("mallory run %d: deny=%v err=%v, want admitted", i, deny, err)
		}
	}
	if deny, err := c.AllowRun(ctx, mallory, now, 2, 0); err != nil || deny != DenyRuns {
		t.Fatalf("mallory's third run: deny=%v err=%v, want DenyRuns — the bound must refuse, not merely count", deny, err)
	}
	// A different contributor is untouched by the one that exhausted its own.
	if deny, err := c.AllowRun(ctx, alice, now, 2, 0); err != nil || deny != DenyNone {
		t.Fatalf("alice: deny=%v err=%v, want admitted — one contributor exhausting its budget must not lock out the others", deny, err)
	}
	// And the org's own budget is a different document entirely.
	if u, err := c.Usage(ctx, OrgSubject("acme"), now); err != nil || u.Runs != 0 {
		t.Fatalf("org usage = %+v err=%v, want 0 runs — fork-author metering must not charge the org document twice", u, err)
	}
}

// The three fork-author segments are caller-supplied and only one of them is
// a forge's numeric id. Unescaped, a separator inside any of them merges two
// contributors into one budget document — one would spend the other's, which
// is precisely the bound the key exists to enforce.
func TestForkAuthorSubject_SeparatorInASegmentCannotMergeTwoContributors(t *testing.T) {
	when := time.Now().UTC()
	pairs := [][2][3]string{
		{{"", "|", "x"}, {"|", "", "x"}},
		{{"org", "", "|"}, {"org", "|", ""}},
		{{"a|b", "github", "1"}, {"a", "b|github", "1"}},
	}
	for _, p := range pairs {
		a := usageKey(ForkAuthorSubject(p[0][0], p[0][1], p[0][2]), when)
		b := usageKey(ForkAuthorSubject(p[1][0], p[1][1], p[1][2]), when)
		if a == b {
			t.Fatalf("%v and %v build the same document id %q — two contributors would share one budget", p[0], p[1], a)
		}
	}
	// The escape must be INJECTIVE, or the collision moves instead of
	// closing: with only "|" replaced, a segment holding the literal "%7C"
	// and one holding "|" build the same key. The escape character is
	// escaped first, which is what makes the map reversible.
	if a, b := usageKey(ForkAuthorSubject("t", "p", "%7C"), when), usageKey(ForkAuthorSubject("t", "p", "|"), when); a == b {
		t.Fatalf("a literal %%7C and a separator produced the same document id %q — escaping that is not injective only relocates the collision", a)
	}
	if a, b := usageKey(ForkAuthorSubject("t", "%7C", "x"), when), usageKey(ForkAuthorSubject("t", "|", "x"), when); a == b {
		t.Fatalf("same collision on the provider segment: %q", a)
	}
	// And the ordinary shape is untouched: no separator, no escaping.
	if got := usageKey(ForkAuthorSubject("acme", "github", "1234"), when); got != "forkauthor|acme|github|1234|"+monthKey(when) {
		t.Fatalf("plain key = %q — escaping must not change the ordinary spelling", got)
	}
}
