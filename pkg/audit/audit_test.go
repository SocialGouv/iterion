package audit

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func runStoreSuite(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	seed := []Event{
		{ID: "1", Scope: ScopeTenant, TenantID: "t1", ActorID: "u1", Action: "webhook.created", CreatedAt: base},
		{ID: "2", Scope: ScopeTenant, TenantID: "t1", ActorID: "u2", Action: "secret.created", CreatedAt: base.Add(time.Minute)},
		{ID: "3", Scope: ScopeTenant, TenantID: "t2", ActorID: "u1", Action: "webhook.created", CreatedAt: base.Add(2 * time.Minute)},
		{ID: "4", Scope: ScopePlatform, TenantID: "t1", ActorID: "root", Action: "org.status_changed", CreatedAt: base.Add(3 * time.Minute)},
	}
	for _, e := range seed {
		if err := s.Insert(ctx, e); err != nil {
			t.Fatalf("insert %s: %v", e.ID, err)
		}
	}

	t.Run("tenant scope + isolation", func(t *testing.T) {
		got, err := s.ListByTenant(ctx, "t1", Page{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("t1 events = %d, want 2 (no cross-tenant, no platform leak)", len(got))
		}
		if got[0].ID != "2" || got[1].ID != "1" {
			t.Fatalf("order = %s,%s want newest first 2,1", got[0].ID, got[1].ID)
		}
	})

	t.Run("platform scope", func(t *testing.T) {
		got, err := s.ListPlatform(ctx, Page{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != "4" {
			t.Fatalf("platform events = %+v, want only #4", got)
		}
	})

	t.Run("filters", func(t *testing.T) {
		byAction, _ := s.ListByTenant(ctx, "t1", Page{Action: "webhook.created"})
		if len(byAction) != 1 || byAction[0].ID != "1" {
			t.Fatalf("action filter = %+v", byAction)
		}
		byActor, _ := s.ListByTenant(ctx, "t1", Page{ActorID: "u2"})
		if len(byActor) != 1 || byActor[0].ID != "2" {
			t.Fatalf("actor filter = %+v", byActor)
		}
		byTime, _ := s.ListByTenant(ctx, "t1", Page{From: base.Add(30 * time.Second)})
		if len(byTime) != 1 || byTime[0].ID != "2" {
			t.Fatalf("from filter = %+v", byTime)
		}
	})

	t.Run("scrub", func(t *testing.T) {
		// Rows the erased account acted in or was acted on, each carrying
		// what identifies the person; a neighbour's row carrying the same.
		for _, e := range []Event{
			{ID: "s1", Scope: ScopeTenant, TenantID: "t9", ActorID: "erased", Action: "pat.created", IP: "10.0.0.1", UserAgent: "ua", Meta: map[string]any{"name": "ci", "email": "erased@example.org"}, CreatedAt: base.Add(10 * time.Minute)},
			{ID: "s2", Scope: ScopeTenant, TenantID: "t9", ActorID: "admin", Action: "member.added", Target: "member", TargetID: "erased", IP: "10.0.0.2", UserAgent: "ua", Meta: map[string]any{"role": "member", "email": "erased@example.org", "subject_hash": "abc"}, CreatedAt: base.Add(11 * time.Minute)},
			{ID: "s3", Scope: ScopePlatform, TenantID: "t9", ActorID: "erased", Action: "org.created", Meta: map[string]any{"name": "acme"}, CreatedAt: base.Add(12 * time.Minute)},
			{ID: "s4", Scope: ScopeTenant, TenantID: "t9", ActorID: "neighbour", Action: "pat.created", IP: "10.0.0.3", UserAgent: "ua", Meta: map[string]any{"email": "neighbour@example.org"}, CreatedAt: base.Add(13 * time.Minute)},
			// An invitation row names its invitee by address only; the IP
			// and user agent are the inviter's.
			{ID: "s5", Scope: ScopeTenant, TenantID: "t9", ActorID: "admin", Action: "invitation.created", Target: "invitation", TargetID: "inv-1", IP: "10.0.0.4", UserAgent: "ua-admin", Meta: map[string]any{"role": "member", "email": " Erased@Example.org "}, CreatedAt: base.Add(14 * time.Minute)},
			{ID: "s6", Scope: ScopeTenant, TenantID: "t9", ActorID: "admin", Action: "invitation.created", Target: "invitation", TargetID: "inv-2", IP: "10.0.0.5", Meta: map[string]any{"email": "erased@example.org.uk"}, CreatedAt: base.Add(15 * time.Minute)},
			// An administrator's act on the account, carrying nothing of the
			// person: the admin's IP and agent, nothing in Meta to scrub.
			{ID: "s7", Scope: ScopeTenant, TenantID: "t9", ActorID: "admin", Action: "user.password_reset", Target: "user", TargetID: "erased", IP: "10.0.0.7", UserAgent: "ua-admin", CreatedAt: base.Add(16 * time.Minute)},
			// Refused sign-ins (ADR-157): an anonymous attempt on the
			// account under a digest of a key the set dropped (found by its
			// target alone), one by address under a digest of the erased
			// address (no account id: found by the digest alone), one under
			// the digest of another address.
			{ID: "s8", Scope: ScopePlatform, Action: "auth.login.failed", Target: "user", TargetID: "erased", IP: "10.0.0.8", UserAgent: "ua-attempt", Meta: map[string]any{"reason": "bad_password", "subject_hash": "d-dropped"}, CreatedAt: base.Add(17 * time.Minute)},
			{ID: "s9", Scope: ScopePlatform, Action: "auth.login.failed", IP: "10.0.0.9", UserAgent: "ua-attempt", Meta: map[string]any{"reason": "unknown_account", "subject_hash": "d-older"}, CreatedAt: base.Add(18 * time.Minute)},
			{ID: "s10", Scope: ScopePlatform, Action: "auth.login.failed", IP: "10.0.0.10", UserAgent: "ua-other", Meta: map[string]any{"reason": "unknown_account", "subject_hash": "d-other"}, CreatedAt: base.Add(19 * time.Minute)},
		} {
			if err := s.Insert(ctx, e); err != nil {
				t.Fatalf("insert %s: %v", e.ID, err)
			}
		}
		digests := []string{"d-active", "d-older"}
		n, err := s.ScrubActor(ctx, "erased", "erased@example.org", digests)
		if err != nil || n != 5 {
			t.Fatalf("ScrubActor = %d (%v), want 5: s1 as actor, s2 as target, s5 by address, s8 as an anonymous attempt's target, s9 by digest (s3 carried nothing)", n, err)
		}
		rows := map[string]Event{}
		for _, e := range mustList(t, s.ListByTenant, "t9") {
			rows[e.ID] = e
		}
		platform, err := s.ListPlatform(ctx, Page{Action: "org.created"})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range platform {
			rows[e.ID] = e
		}
		refusals, err := s.ListPlatform(ctx, Page{Action: "auth.login.failed"})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range refusals {
			rows[e.ID] = e
		}
		if e := rows["s1"]; e.IP != "" || e.UserAgent != "" {
			t.Fatalf("the account's own row still carries ip %q / user agent %q", e.IP, e.UserAgent)
		}
		// s2's IP and agent are the administrator's who added the account:
		// the trace of that act stays.
		if e := rows["s2"]; e.IP != "10.0.0.2" || e.UserAgent != "ua" {
			t.Fatalf("the row the account is only the target of lost its actor's ip %q / user agent %q", e.IP, e.UserAgent)
		}
		for _, id := range []string{"s1", "s2"} {
			e := rows[id]
			for _, k := range ScrubbedMetaKeys {
				if _, ok := e.Meta[k]; ok {
					t.Fatalf("%s still carries meta.%s", id, k)
				}
			}
		}
		if rows["s1"].ActorID != "erased" || rows["s2"].TargetID != "erased" || rows["s1"].Meta["name"] != "ci" || rows["s2"].Meta["role"] != "member" {
			t.Fatalf("the scrub took more than the identifiers: s1=%+v s2=%+v", rows["s1"], rows["s2"])
		}
		if e := rows["s3"]; e.Meta["name"] != "acme" {
			t.Fatalf("a row carrying nothing to scrub changed: %+v", e)
		}
		if e := rows["s4"]; e.IP != "10.0.0.3" || e.Meta["email"] != "neighbour@example.org" {
			t.Fatalf("a neighbour's row was scrubbed: %+v", e)
		}
		if e := rows["s5"]; e.Meta["email"] != nil || e.Meta["role"] != "member" || e.IP != "10.0.0.4" || e.UserAgent != "ua-admin" || e.ActorID != "admin" {
			t.Fatalf("the invitation row addressed to the account: %+v, want its address gone and the inviter's IP and agent kept", e)
		}
		if e := rows["s7"]; e.IP != "10.0.0.7" || e.UserAgent != "ua-admin" {
			t.Fatalf("an administrator's act on the account lost its actor's ip and agent: %+v", e)
		}
		if e := rows["s6"]; e.Meta["email"] != "erased@example.org.uk" {
			t.Fatalf("a row addressed to a longer address was scrubbed: %+v", e)
		}
		for _, id := range []string{"s8", "s9"} {
			e := rows[id]
			if e.IP != "" || e.UserAgent != "" || e.Meta["subject_hash"] != nil {
				t.Fatalf("refused sign-in %s still carries ip %q / user agent %q / subject_hash %v", id, e.IP, e.UserAgent, e.Meta["subject_hash"])
			}
			if e.Meta["reason"] == nil {
				t.Fatalf("refused sign-in %s lost its reason: %+v", id, e)
			}
		}
		if e := rows["s10"]; e.IP != "10.0.0.10" || e.UserAgent != "ua-other" || e.Meta["subject_hash"] != "d-other" {
			t.Fatalf("another address's refused sign-in was scrubbed: %+v", e)
		}
		if n, err := s.ScrubActor(ctx, "erased", "erased@example.org", digests); err != nil || n != 0 {
			t.Fatalf("a second scrub = %d (%v), want 0", n, err)
		}
		if _, err := s.ScrubActor(ctx, "", "erased@example.org", nil); err == nil {
			t.Fatal("a scrub naming no account answered no error")
		}
	})

	t.Run("pagination", func(t *testing.T) {
		page1, _ := s.ListByTenant(ctx, "t1", Page{Limit: 1})
		if len(page1) != 1 || page1[0].ID != "2" {
			t.Fatalf("page1 = %+v", page1)
		}
		page2, _ := s.ListByTenant(ctx, "t1", Page{Offset: 1, Limit: 1})
		if len(page2) != 1 || page2[0].ID != "1" {
			t.Fatalf("page2 = %+v", page2)
		}
		empty, _ := s.ListByTenant(ctx, "t1", Page{Offset: 10})
		if len(empty) != 0 {
			t.Fatalf("past-the-end = %+v", empty)
		}
	})
}

// mustList lists every event of tenantID through list, failing the test on
// an error.
func mustList(t *testing.T, list func(context.Context, string, Page) ([]Event, error), tenantID string) []Event {
	t.Helper()
	out, err := list(context.Background(), tenantID, Page{})
	if err != nil {
		t.Fatalf("list %s: %v", tenantID, err)
	}
	return out
}

func TestMemoryStore(t *testing.T) { runStoreSuite(t, NewMemoryStore()) }

func TestClampLimit(t *testing.T) {
	for _, c := range []struct{ in, want int }{{0, 50}, {-1, 50}, {10, 10}, {501, 500}} {
		t.Run(fmt.Sprint(c.in), func(t *testing.T) {
			if got := ClampLimit(c.in); got != c.want {
				t.Fatalf("ClampLimit(%d) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// TestMemoryStore_OwnsItsMetaMaps: the store owns the Meta map of every event
// it holds — one scenario per point where a map could be shared.
//
// The mutations this reddens, one each: ScrubActor deleting from the stored
// map in place (a response still encoding a row it read sees the scrub land
// under it — a data race); list handing out the stored map; Insert storing
// the caller's map.
func TestMemoryStore_OwnsItsMetaMaps(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	seed := func(s *MemoryStore, meta map[string]any) {
		t.Helper()
		if err := s.Insert(ctx, Event{ID: "1", Scope: ScopePlatform, ActorID: "u1", Action: "org_invitation.created", CreatedAt: at, Meta: meta}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(s *MemoryStore) map[string]any {
		t.Helper()
		rows, err := s.ListPlatform(ctx, Page{})
		if err != nil || len(rows) != 1 {
			t.Fatalf("ListPlatform = %+v (%v), want the one row", rows, err)
		}
		return rows[0].Meta
	}

	t.Run("a scrub replaces the map a reader may still hold", func(t *testing.T) {
		s := NewMemoryStore()
		seed(s, map[string]any{"email": "ada@example.org", "role": "member"})
		held := s.events[0].Meta // what a list without its copy would have handed out
		if _, err := s.ScrubActor(ctx, "u1", "ada@example.org", nil); err != nil {
			t.Fatal(err)
		}
		if held["email"] != "ada@example.org" {
			t.Fatalf("the scrub deleted from a map a reader may hold: %+v", held)
		}
		if got := read(s); got["email"] != nil || got["role"] != "member" {
			t.Fatalf("the stored row after the scrub: %+v, want its address gone and the rest kept", got)
		}
	})

	t.Run("a reader's copy is its own", func(t *testing.T) {
		s := NewMemoryStore()
		seed(s, map[string]any{"role": "member"})
		read(s)["role"] = "owner"
		if got := read(s); got["role"] != "member" {
			t.Fatalf("a write to a listed row reached the store: %+v", got)
		}
	})

	t.Run("the inserted map is the store's own", func(t *testing.T) {
		s := NewMemoryStore()
		meta := map[string]any{"role": "member"}
		seed(s, meta)
		meta["role"] = "owner"
		if got := read(s); got["role"] != "member" {
			t.Fatalf("a write to the caller's map after the insert reached the store: %+v", got)
		}
	})
}
