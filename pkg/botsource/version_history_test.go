package botsource

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// assertVersionHistoryContract is the shared conformance check for the
// by-version accessor (#1381) — both twins (memory + mongo) run it, so the
// version-history contract never drifts:
//
//  1. every write snapshots its version: GetByVersion returns the exact
//     content each write carried, not the current row;
//  2. unknown or non-positive versions are ErrNotFound;
//  3. a foreign/scoped-mismatched tenant sees nothing;
//  4. history is retained across Delete — a pinned version outliving its
//     row is deliberate (the preview certified that content);
//  5. incarnations are isolated: a slug deleted and re-authored mints a
//     new row id, and each id serves ITS OWN content — the old pin can
//     never be answered by the new incarnation, the new pin never by the
//     old.
func assertVersionHistoryContract(t *testing.T, st Store, ctx context.Context, tenantID string) {
	t.Helper()
	scoped := store.WithTenant(ctx, tenantID)

	v1, err := st.Create(scoped, BotSource{
		TenantID: tenantID, Slug: "history-keeper",
		Files: map[string]string{MainBotFile: "workflow main:\n  start -> done\n"},
	})
	if err != nil {
		t.Fatalf("Create v1: %v", err)
	}
	v1Files := v1.Files[MainBotFile]

	v2, err := st.Update(scoped, BotSource{
		ID: v1.ID, TenantID: tenantID, Slug: v1.Slug, Version: v1.Version,
		Files: map[string]string{MainBotFile: "workflow main:\n  start -> second -> done\n"},
	})
	if err != nil {
		t.Fatalf("Update v2: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("want version 2 after update, got %d", v2.Version)
	}

	// (1) each version resolves to ITS OWN content.
	got1, err := st.GetByVersion(scoped, tenantID, v1.ID, 1)
	if err != nil {
		t.Fatalf("GetByVersion(1): %v", err)
	}
	if got1.Files[MainBotFile] != v1Files {
		t.Errorf("GetByVersion(1) main = %q; want the v1 content %q", got1.Files[MainBotFile], v1Files)
	}
	if got1.Version != 1 {
		t.Errorf("GetByVersion(1).Version = %d; want 1", got1.Version)
	}
	// The returned row carries the ROW id — the mongo snapshot stores a
	// composite _id, and a read that leaked it back would hand the caller a
	// row no Update can name (and diverge from the memory twin).
	if got1.ID != v1.ID {
		t.Errorf("GetByVersion(1).ID = %q; want the row id %q", got1.ID, v1.ID)
	}
	got2, err := st.GetByVersion(scoped, tenantID, v1.ID, 2)
	if err != nil {
		t.Fatalf("GetByVersion(2): %v", err)
	}
	if got2.Version != 2 || got2.Files[MainBotFile] == v1Files {
		t.Errorf("GetByVersion(2) = version %d main %q; want the v2 content", got2.Version, got2.Files[MainBotFile])
	}
	// And the CURRENT row is v2 — the accessor is not an alias for GetBySlug.
	cur, err := st.GetBySlug(scoped, tenantID, v1.Slug)
	if err != nil || cur.Version != 2 {
		t.Fatalf("GetBySlug after history writes: version %d err %v; want 2", cur.Version, err)
	}

	// (2) unknown ids and non-positive/unknown versions are ErrNotFound.
	for _, tc := range []struct {
		id      string
		version int
	}{{id: "no-such-row", version: 1}, {id: v1.ID, version: 0}, {id: v1.ID, version: -1}, {id: v1.ID, version: 42}} {
		if _, err := st.GetByVersion(scoped, tenantID, tc.id, tc.version); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetByVersion(%q, %d) = %v; want ErrNotFound", tc.id, tc.version, err)
		}
	}

	// (3) a scoped-mismatched read sees nothing.
	foreign := store.WithTenant(ctx, tenantID+"-other")
	if _, err := st.GetByVersion(foreign, tenantID, v1.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("foreign GetByVersion = %v; want ErrNotFound", err)
	}
	// And so does a read whose ctx carries NO tenant: the read FILTER (not
	// the ctx guard) must refuse it — the guard alone cannot.
	if _, err := st.GetByVersion(ctx, tenantID+"-other", v1.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("unscoped-ctx GetByVersion = %v; want ErrNotFound", err)
	}

	// (4) history is retained across Delete.
	if err := st.Delete(scoped, v1.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.GetBySlug(scoped, tenantID, v1.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("row still present after delete: %v", err)
	}
	got1, err = st.GetByVersion(scoped, tenantID, v1.ID, 1)
	if err != nil {
		t.Fatalf("GetByVersion(1) after delete: %v — a pinned version must outlive its row", err)
	}
	if got1.Files[MainBotFile] != v1Files {
		t.Errorf("post-delete GetByVersion(1) main = %q; want the certified v1 content", got1.Files[MainBotFile])
	}

	// (5) incarnations are isolated: the recreated slug mints a NEW row id
	// and its v1 must never answer the OLD row's pin — nor the old pin's
	// content serve the new row.
	reborn, err := st.Create(scoped, BotSource{
		TenantID: tenantID, Slug: "history-keeper",
		Files: map[string]string{MainBotFile: "workflow main:\n  REBORN -> done\n"},
	})
	if err != nil {
		t.Fatalf("Create reborn: %v", err)
	}
	if reborn.ID == v1.ID {
		t.Fatal("the recreated row reused the deleted row's id — incarnations can no longer be isolated")
	}
	rebornV1, err := st.GetByVersion(scoped, tenantID, reborn.ID, 1)
	if err != nil {
		t.Fatalf("GetByVersion(reborn, 1): %v", err)
	}
	if got := rebornV1.Files[MainBotFile]; got != "workflow main:\n  REBORN -> done\n" {
		t.Errorf("reborn row's v1 = %q; want the reborn content", got)
	}
	// And the OLD id still serves the OLD certified content — the recreate
	// neither overwrote it nor aliased it.
	old, err := st.GetByVersion(scoped, tenantID, v1.ID, 1)
	if err != nil {
		t.Fatalf("GetByVersion(old, 1) after the recreate: %v", err)
	}
	if old.Files[MainBotFile] != v1Files {
		t.Errorf("old incarnation after the recreate: %q; want its own v1 content", old.Files[MainBotFile])
	}

	// (6) identity is MINTED on create, never honored from the caller: a
	// recycled id must not alias the deleted row's history — on EITHER twin.
	crafted, err := st.Create(scoped, BotSource{
		ID: v1.ID, TenantID: tenantID, Slug: "crafted",
		Files: map[string]string{MainBotFile: "workflow main:\n  CRAFTED -> done\n"},
	})
	if err != nil {
		t.Fatalf("Create with a recycled id: %v", err)
	}
	if crafted.ID == v1.ID {
		t.Fatal("Create honored the caller-supplied id — a recycled id can alias a deleted row's version history")
	}
	craftedV1, err := st.GetByVersion(scoped, tenantID, crafted.ID, 1)
	if err != nil {
		t.Fatalf("GetByVersion(crafted, 1): %v", err)
	}
	if got := craftedV1.Files[MainBotFile]; got != "workflow main:\n  CRAFTED -> done\n" {
		t.Errorf("crafted row's v1 = %q; want the crafted content under its own minted id", got)
	}

	// (7) the fallback read (#1517): the NEWEST snapshot at or below the
	// asked ceiling — the nearest older version a missing pin resolves to.
	v2snap, err := st.GetByVersion(scoped, tenantID, v2.ID, 2)
	if err != nil {
		t.Fatalf("GetByVersion(2): %v", err)
	}
	// A ceiling below the oldest snapshot, an unknown row, and a foreign
	// tenant are ErrNotFound; the prefix-collision twin (an id extending
	// this one's) must not answer.
	fb, err := st.GetVersionAtOrBefore(scoped, tenantID, v2.ID, 1)
	if err != nil {
		t.Fatalf("GetVersionAtOrBefore(1): %v", err)
	}
	if fb.Version != 1 {
		t.Errorf("the nearest older of v2 at ceiling 1 = version %d, want 1", fb.Version)
	}
	if fb.Files[MainBotFile] != v1Files {
		t.Errorf("the fallback content is not v1's: %q", fb.Files[MainBotFile])
	}
	// The fallback read keys rows by their IDENTITY: a composite snapshot
	// id leaking out aliases nothing today and everything tomorrow.
	if fb.ID != v2.ID {
		t.Errorf("the fallback read leaks a composite id: %q, want the row id %q", fb.ID, v2.ID)
	}
	// A snapshot's created_at is its WRITE time (the TTL expires on it):
	// later writes carry later stamps, whatever the row's own age.
	if !v2snap.CreatedAt.After(got1.CreatedAt) {
		t.Errorf("v2's snapshot stamp (%v) is not after v1's (%v) — the snapshot ages with the row, and the TTL would sweep live versions", v2snap.CreatedAt, got1.CreatedAt)
	}
	if _, err := st.GetVersionAtOrBefore(scoped, tenantID, v2.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("a ceiling below every snapshot: %v, want ErrNotFound", err)
	}
	if _, err := st.GetVersionAtOrBefore(scoped, tenantID, "no-such-row", 9); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown row: %v, want ErrNotFound", err)
	}
	if _, err := st.GetVersionAtOrBefore(store.WithTenant(context.Background(), "other-team"), "other-team", v2.ID, 9); !errors.Is(err, ErrNotFound) {
		t.Errorf("a foreign tenant: %v, want ErrNotFound", err)
	}

	// (8) the purge (#1517): every snapshot of the row goes, the live row
	// stays, the count is what was there, and the purge of an already-purged
	// row is ErrNotFound.
	n, err := st.PurgeHistory(scoped, tenantID, v2.ID)
	if err != nil {
		t.Fatalf("PurgeHistory: %v", err)
	}
	if n != 2 {
		t.Errorf("PurgeHistory removed %d snapshots, want 2 (v1, v2)", n)
	}
	if _, err := st.GetByVersion(scoped, tenantID, v2.ID, 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("a purged snapshot: %v, want ErrNotFound", err)
	}
	if _, err := st.PurgeHistory(scoped, tenantID, v2.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a second purge: %v, want ErrNotFound", err)
	}
	// The purge never touches the LIVE row: crafted's row survives its own
	// history's purge.
	if _, err := st.Get(scoped, crafted.ID); err != nil {
		t.Fatalf("the live row must survive the purge: %v", err)
	}
	// An id extending another's ("v2.ID" vs a crafted prefix) reads and
	// purges nothing of the OTHER row: purge the crafted row only.
	n, err = st.PurgeHistory(scoped, tenantID, crafted.ID)
	if err != nil {
		t.Fatalf("PurgeHistory(crafted): %v", err)
	}
	if n == 0 {
		t.Error("the crafted row's snapshots did not purge")
	}
}

// TestMemoryStore_VersionHistory runs the shared by-version contract on the
// memory twin.
func TestMemoryStore_VersionHistory(t *testing.T) {
	assertVersionHistoryContract(t, NewMemoryStore(), context.Background(), "team-1")
}
