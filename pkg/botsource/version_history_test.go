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
}

// TestMemoryStore_VersionHistory runs the shared by-version contract on the
// memory twin.
func TestMemoryStore_VersionHistory(t *testing.T) {
	assertVersionHistoryContract(t, NewMemoryStore(), context.Background(), "team-1")
}
