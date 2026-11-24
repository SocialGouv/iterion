package marketplace

import (
	"reflect"
	"testing"
)

func TestEffectiveHelpers_LegacyZeroValues(t *testing.T) {
	// A legacy flat entry (no scope/status/source set) must read as
	// public + approved + git so it stays world-visible with no migration.
	var e Entry
	if got := EffectiveStatus(e); got != StatusApproved {
		t.Errorf("EffectiveStatus zero = %q, want approved", got)
	}
	if got := EffectiveScope(e); got != ScopePublic {
		t.Errorf("EffectiveScope zero = %q, want public", got)
	}
	if got := EffectiveSource(e); got != SourceGit {
		t.Errorf("EffectiveSource zero = %q, want git", got)
	}
}

func TestVisible(t *testing.T) {
	approvedPublic := Entry{Slug: "a", Status: StatusApproved, Scope: ScopePublic}
	legacy := Entry{Slug: "legacy"} // empty status/scope → approved/public
	approvedInstance := Entry{Slug: "i", Status: StatusApproved, Scope: ScopeInstance}
	approvedOrgX := Entry{Slug: "ox", Status: StatusApproved, Scope: ScopeOrg, OrgID: "X"}
	pendingPublicOwnedByU := Entry{Slug: "p", Status: StatusPending, Scope: ScopePublic, SubmittedBy: "U"}

	anon := ViewerContext{Enforce: true}
	authedNoOrg := ViewerContext{Enforce: true, Authenticated: true, UserID: "U"}
	authedOrgX := ViewerContext{Enforce: true, Authenticated: true, UserID: "V", OrgIDs: []string{"X"}}
	authedOrgY := ViewerContext{Enforce: true, Authenticated: true, UserID: "W", OrgIDs: []string{"Y"}}
	superAdmin := ViewerContext{Enforce: true, Authenticated: true, UserID: "S", IsSuperAdmin: true}
	local := ViewerContext{} // Enforce false

	cases := []struct {
		name string
		e    Entry
		v    ViewerContext
		want bool
	}{
		{"local sees everything (pending)", pendingPublicOwnedByU, local, true},
		{"anon sees approved public", approvedPublic, anon, true},
		{"anon sees legacy", legacy, anon, true},
		{"anon cannot see instance", approvedInstance, anon, false},
		{"anon cannot see org", approvedOrgX, anon, false},
		{"anon cannot see pending", pendingPublicOwnedByU, anon, false},
		{"authed sees instance", approvedInstance, authedNoOrg, true},
		{"authed without org cannot see org", approvedOrgX, authedNoOrg, false},
		{"org member sees own org", approvedOrgX, authedOrgX, true},
		{"other org cannot see org", approvedOrgX, authedOrgY, false},
		{"owner sees own pending", pendingPublicOwnedByU, authedNoOrg, true},
		{"non-owner cannot see pending", pendingPublicOwnedByU, authedOrgY, false},
		{"super-admin sees any org approved", approvedOrgX, superAdmin, true},
		{"super-admin still cannot see foreign pending via browse", pendingPublicOwnedByU, superAdmin, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Visible(tc.e, tc.v); got != tc.want {
				t.Errorf("Visible(%s, %+v) = %v, want %v", tc.e.Slug, tc.v, got, tc.want)
			}
		})
	}
}

func TestForViewer(t *testing.T) {
	full := Entry{
		Slug:         "e",
		Author:       "manifest author",
		RepoURL:      "https://example.test/repo",
		Status:       StatusApproved,
		Scope:        ScopePublic,
		OrgID:        "X",
		SubmittedBy:  "U",
		ReviewedBy:   "S",
		ReviewedAt:   "2026-11-03T00:00:00Z",
		RejectReason: "why",
	}

	anon := ViewerContext{Enforce: true}
	otherUser := ViewerContext{Enforce: true, Authenticated: true, UserID: "V", OrgIDs: []string{"X"}}
	owner := ViewerContext{Enforce: true, Authenticated: true, UserID: "U"}
	superAdmin := ViewerContext{Enforce: true, Authenticated: true, UserID: "S", IsSuperAdmin: true}
	local := ViewerContext{} // Enforce false

	cases := []struct {
		name string
		v    ViewerContext
		keep bool
	}{
		{"anonymous enforced read is redacted", anon, false},
		{"authenticated non-owner is redacted (same org too)", otherUser, false},
		{"owner keeps the moderation metadata", owner, true},
		{"super-admin keeps the moderation metadata", superAdmin, true},
		{"local mode keeps everything", local, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ForViewer(full, tc.v)
			redacted := got.SubmittedBy == "" && got.ReviewedBy == "" && got.ReviewedAt == "" && got.RejectReason == ""
			if tc.keep && redacted {
				t.Fatalf("ForViewer(%+v) redacted; want whole entry", tc.v)
			}
			if !tc.keep && !redacted {
				t.Fatalf("ForViewer(%+v) leaked submitted_by=%q reviewed_by=%q reviewed_at=%q reject_reason=%q",
					tc.v, got.SubmittedBy, got.ReviewedBy, got.ReviewedAt, got.RejectReason)
			}
			if tc.keep && !reflect.DeepEqual(got, full) {
				t.Fatalf("ForViewer altered a kept entry: %+v", got)
			}
			// The public fields survive either way.
			if got.Slug != full.Slug || got.Author != full.Author || got.RepoURL != full.RepoURL || got.OrgID != full.OrgID {
				t.Fatalf("ForViewer altered public fields: %+v", got)
			}
		})
	}

	t.Run("anonymous viewer is not the submitter of an unsubmitted entry", func(t *testing.T) {
		// A seeded entry has no SubmittedBy; an anonymous viewer has no
		// UserID. The empty ids must not match as "owner".
		seeded := Entry{Slug: "s", ReviewedBy: "S", ReviewedAt: "2026-11-03T00:00:00Z", RejectReason: "why"}
		got := ForViewer(seeded, anon)
		if got.ReviewedBy != "" || got.ReviewedAt != "" || got.RejectReason != "" {
			t.Fatalf("anonymous read of an unsubmitted entry leaked moderation metadata: %+v", got)
		}
	})
	// ForViewer takes its Entry by value, so its caller's copy cannot
	// change; the guarantee that matters — a store's backing array is
	// never written through — is TestForViewerAll's.
}

func TestForViewerAll(t *testing.T) {
	anon := ViewerContext{Enforce: true}
	if got := ForViewerAll(nil, anon); got != nil {
		t.Fatalf("ForViewerAll(nil) = %v, want nil", got)
	}
	in := []Entry{
		{Slug: "a", SubmittedBy: "U", ReviewedBy: "S"},
		{Slug: "b", SubmittedBy: "W"},
	}
	got := ForViewerAll(in, anon)
	if len(got) != 2 || got[0].Slug != "a" || got[1].Slug != "b" {
		t.Fatalf("ForViewerAll shape = %+v", got)
	}
	for _, e := range got {
		if e.SubmittedBy != "" || e.ReviewedBy != "" {
			t.Fatalf("ForViewerAll leaked ids: %+v", e)
		}
	}
	// The input backing array is untouched (new slice, not in-place).
	if in[0].SubmittedBy != "U" || in[1].SubmittedBy != "W" {
		t.Fatalf("ForViewerAll wrote through its input: %+v", in)
	}
}
