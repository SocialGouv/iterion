package forge_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// runBoardBindingStoreSuite exercises the forge.BoardBindingStore contract. It
// runs against the in-memory store (always — proving the suite) and the Mongo
// store (gated on ITERION_TEST_MONGO_URI), so the two implementations are held
// to an identical bar. The CAS claim in particular is the whole reason the
// cloud sync worker can run on N replicas.
func runBoardBindingStoreSuite(t *testing.T, store forge.BoardBindingStore) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	binding := func(tenant string, every time.Duration) forge.BoardBinding {
		return forge.BoardBinding{
			TenantID: tenant, Provider: forge.ProviderGitHub,
			Owner: "SocialGouv", OwnerKind: forge.ProjectOwnerOrg, Number: 203,
			ConnectionID: "conn-1", ProjectID: "PVT_p", ProjectTitle: "Iterion",
			StatusFieldID: "PVTSSF_status",
			StatusOptions: map[string]string{"ready": "opt_planned", "done": "opt_done"},
			StatusMapping: []forge.StatusMapping{{Status: "Planned", State: "ready"}, {Status: "Done", State: "done"}},
			LabelFields:   []forge.BoundLabelField{{FieldID: "PVTSSF_area", Name: "Area", Prefix: "area:"}},
			SyncEvery:     every,
			CreatedAt:     base, UpdatedAt: base,
		}
	}

	t.Run("missing binding is a typed error", func(t *testing.T) {
		if _, err := store.GetByTenant(ctx, "nobody"); !errors.Is(err, forge.ErrBoardBindingNotFound) {
			t.Fatalf("want ErrBoardBindingNotFound, got %v", err)
		}
		if err := store.Delete(ctx, "nobody"); !errors.Is(err, forge.ErrBoardBindingNotFound) {
			t.Fatalf("Delete of a missing binding: want ErrBoardBindingNotFound, got %v", err)
		}
	})

	t.Run("upsert then read back", func(t *testing.T) {
		if err := store.Upsert(ctx, binding("team-a", 10*time.Minute)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		got, err := store.GetByTenant(ctx, "team-a")
		if err != nil {
			t.Fatalf("GetByTenant: %v", err)
		}
		if got.Owner != "SocialGouv" || got.Number != 203 || got.ProjectID != "PVT_p" {
			t.Errorf("identity round-trip wrong: %+v", got)
		}
		if got.StatusOptions["ready"] != "opt_planned" {
			t.Errorf("status options round-trip wrong: %v", got.StatusOptions)
		}
		if len(got.StatusMapping) != 2 || got.StatusMapping[0].Status != "Planned" {
			t.Errorf("status mapping round-trip wrong: %+v", got.StatusMapping)
		}
		if len(got.LabelFields) != 1 || got.LabelFields[0].Prefix != "area:" {
			t.Errorf("label fields round-trip wrong: %+v", got.LabelFields)
		}
		if got.SyncEvery != 10*time.Minute {
			t.Errorf("SyncEvery = %v, want 10m", got.SyncEvery)
		}
		if got.OwnerKind != forge.ProjectOwnerOrg {
			t.Errorf("OwnerKind = %q", got.OwnerKind)
		}
	})

	t.Run("one binding per team: upsert replaces", func(t *testing.T) {
		next := binding("team-a", 5*time.Minute)
		next.Number = 204
		next.ProjectTitle = "Iterion v2"
		if err := store.Upsert(ctx, next); err != nil {
			t.Fatalf("Upsert (replace): %v", err)
		}
		got, err := store.GetByTenant(ctx, "team-a")
		if err != nil {
			t.Fatalf("GetByTenant: %v", err)
		}
		if got.Number != 204 || got.ProjectTitle != "Iterion v2" || got.SyncEvery != 5*time.Minute {
			t.Fatalf("a second bind must REPLACE, not duplicate: %+v", got)
		}
		all, err := store.ListAll(ctx)
		if err != nil {
			t.Fatalf("ListAll: %v", err)
		}
		n := 0
		for _, b := range all {
			if b.TenantID == "team-a" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("team-a has %d bindings, want exactly 1", n)
		}
	})

	t.Run("due bindings honour the interval and the off switch", func(t *testing.T) {
		// team-b: never synced, 10m interval → due.
		if err := store.Upsert(ctx, binding("team-b", 10*time.Minute)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		// team-off: sync disabled → never due, whatever the clock says.
		if err := store.Upsert(ctx, binding("team-off", 0)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		due, err := store.DueBindings(ctx, base.Add(time.Hour))
		if err != nil {
			t.Fatalf("DueBindings: %v", err)
		}
		byTenant := map[string]forge.BoardBinding{}
		for _, b := range due {
			byTenant[b.TenantID] = b
		}
		if _, ok := byTenant["team-b"]; !ok {
			t.Error("a never-synced binding must be due")
		}
		if _, ok := byTenant["team-off"]; ok {
			t.Error("sync_every=0 means OFF — it must never come up due")
		}
	})

	t.Run("ClaimSync is a CAS: exactly one replica wins", func(t *testing.T) {
		b := binding("team-cas", time.Minute)
		if err := store.Upsert(ctx, b); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		seen, err := store.GetByTenant(ctx, "team-cas")
		if err != nil {
			t.Fatalf("GetByTenant: %v", err)
		}
		at := base.Add(2 * time.Hour)
		won, err := store.ClaimSync(ctx, "team-cas", seen.LastSyncedAt, at)
		if err != nil {
			t.Fatalf("ClaimSync: %v", err)
		}
		if !won {
			t.Fatal("the first claimant must win")
		}
		// A second replica presenting the SAME stale watermark must lose.
		won2, err := store.ClaimSync(ctx, "team-cas", seen.LastSyncedAt, at)
		if err != nil {
			t.Fatalf("ClaimSync (second): %v", err)
		}
		if won2 {
			t.Fatal("a replica presenting a stale watermark must LOSE — otherwise N replicas each run the pass")
		}
		after, err := store.GetByTenant(ctx, "team-cas")
		if err != nil {
			t.Fatalf("GetByTenant: %v", err)
		}
		if !after.LastSyncedAt.Equal(at) {
			t.Errorf("LastSyncedAt = %v, want %v", after.LastSyncedAt, at)
		}
		// And it is no longer due before its next interval.
		due, err := store.DueBindings(ctx, at.Add(30*time.Second))
		if err != nil {
			t.Fatalf("DueBindings: %v", err)
		}
		for _, d := range due {
			if d.TenantID == "team-cas" {
				t.Error("a just-synced binding must not be due again inside its interval")
			}
		}
	})

	t.Run("the claim CAS survives a sub-millisecond watermark", func(t *testing.T) {
		// BSON keeps a datetime to the MILLISECOND, and a watermark reaches
		// the store two ways: written by ClaimSync (time.Now, nanosecond) or
		// carried in by an Upsert from a caller-supplied value. If the filter
		// compared an un-truncated in-memory instant against the truncated
		// stored one, the CAS would never match after the first pass and the
		// reconciliation would stop dead. Both shapes must claim.
		nsWatermark := base.Add(3 * time.Hour).Add(1234567 * time.Nanosecond)
		if sub := nsWatermark.Sub(nsWatermark.Truncate(time.Millisecond)); sub == 0 {
			t.Fatalf("fixture must carry sub-millisecond precision, got %v", sub)
		}
		b := binding("team-precision", time.Minute)
		b.LastSyncedAt = nsWatermark
		if err := store.Upsert(ctx, b); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		// (a) the value the CALLER holds — never round-tripped through the store.
		at := nsWatermark.Add(time.Hour)
		won, err := store.ClaimSync(ctx, "team-precision", nsWatermark, at)
		if err != nil || !won {
			t.Fatalf("claim with the caller's own watermark: won=%v err=%v", won, err)
		}
		if err := store.ReleaseSync(ctx, "team-precision"); err != nil {
			t.Fatalf("ReleaseSync: %v", err)
		}
		// (b) the value READ BACK — the shape the sync worker actually uses,
		// and the one a pass after the first always presents.
		seen, err := store.GetByTenant(ctx, "team-precision")
		if err != nil {
			t.Fatalf("GetByTenant: %v", err)
		}
		won2, err := store.ClaimSync(ctx, "team-precision", seen.LastSyncedAt, at.Add(time.Hour))
		if err != nil || !won2 {
			t.Fatalf("claim with the stored watermark: won=%v err=%v — the periodic pass would stop after its first run", won2, err)
		}
		if err := store.ReleaseSync(ctx, "team-precision"); err != nil {
			t.Fatalf("ReleaseSync (second): %v", err)
		}
		// And a watermark that is genuinely different still loses, so the
		// truncation has not turned the CAS into "always match".
		if won3, err := store.ClaimSync(ctx, "team-precision", nsWatermark, at.Add(2*time.Hour)); err != nil || won3 {
			t.Fatalf("a stale watermark must still lose: won=%v err=%v", won3, err)
		}
	})

	t.Run("a claimed pass holds a bounded lease", func(t *testing.T) {
		// The watermark CAS alone only elects one replica per TICK. A pass
		// slower than the binding's interval (floor 1 min) makes the binding
		// due again while it is still running, and the next tick's claim
		// presents the watermark THIS pass wrote — so it matches, and two
		// replicas reconcile the same board at once, issuing duplicate board
		// writes on the same cards. The lease is what makes the claim mean
		// "and nobody else is inside it".
		if err := store.Upsert(ctx, binding("team-lease", time.Minute)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		at := base.Add(time.Hour)
		won, err := store.ClaimSync(ctx, "team-lease", time.Time{}, at)
		if err != nil || !won {
			t.Fatalf("ClaimSync: won=%v err=%v", won, err)
		}
		// One interval later the binding is due again and the pass is still
		// running. The presented watermark MATCHES — only the lease refuses.
		overrun := at.Add(90 * time.Second)
		won2, err := store.ClaimSync(ctx, "team-lease", at, overrun)
		if err != nil {
			t.Fatalf("ClaimSync (overlapping): %v", err)
		}
		if won2 {
			t.Fatal("a second replica claimed a board whose pass is still running — that is the overlap the lease exists to refuse")
		}
		// The refusal must not have advanced the watermark: the running pass
		// still owns it, and a bumped watermark would hide the overrun.
		held, err := store.GetByTenant(ctx, "team-lease")
		if err != nil {
			t.Fatalf("GetByTenant: %v", err)
		}
		if !held.LastSyncedAt.Equal(at) {
			t.Errorf("LastSyncedAt = %v, want the running pass's %v", held.LastSyncedAt, at)
		}
		// A re-bind while a pass runs must not release it (the Mongo twin
		// never names the field in its $set; the memory twin must agree).
		if err := store.Upsert(ctx, binding("team-lease", time.Minute)); err != nil {
			t.Fatalf("Upsert during a pass: %v", err)
		}
		if wonRebind, err := store.ClaimSync(ctx, "team-lease", at, overrun); err != nil || wonRebind {
			t.Fatalf("a re-bind released a running pass's lease: won=%v err=%v", wonRebind, err)
		}
		// Releasing at pass end hands the board back immediately — the TTL is
		// only ever reached by a replica that died mid-pass.
		if err := store.ReleaseSync(ctx, "team-lease"); err != nil {
			t.Fatalf("ReleaseSync: %v", err)
		}
		won3, err := store.ClaimSync(ctx, "team-lease", at, overrun)
		if err != nil || !won3 {
			t.Fatalf("after ReleaseSync the next pass must claim: won=%v err=%v", won3, err)
		}
		// And a lease nobody released expires, so a dead replica costs one
		// TTL of staleness rather than the board forever.
		expired := overrun.Add(forge.BoardSyncLeaseTTL).Add(time.Second)
		won4, err := store.ClaimSync(ctx, "team-lease", overrun, expired)
		if err != nil || !won4 {
			t.Fatalf("an expired lease must be reclaimable: won=%v err=%v", won4, err)
		}
		if err := store.ReleaseSync(ctx, "team-lease"); err != nil {
			t.Fatalf("ReleaseSync (cleanup): %v", err)
		}
	})

	t.Run("ReleaseSync on a missing binding is a typed error", func(t *testing.T) {
		if err := store.ReleaseSync(ctx, "nobody"); !errors.Is(err, forge.ErrBoardBindingNotFound) {
			t.Fatalf("want ErrBoardBindingNotFound, got %v", err)
		}
	})

	t.Run("ClaimSync on a missing binding is a typed error", func(t *testing.T) {
		_, err := store.ClaimSync(ctx, "nobody", time.Time{}, base)
		if !errors.Is(err, forge.ErrBoardBindingNotFound) {
			t.Fatalf("want ErrBoardBindingNotFound, got %v", err)
		}
	})

	t.Run("delete removes it", func(t *testing.T) {
		if err := store.Upsert(ctx, binding("team-del", time.Minute)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := store.Delete(ctx, "team-del"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := store.GetByTenant(ctx, "team-del"); !errors.Is(err, forge.ErrBoardBindingNotFound) {
			t.Fatalf("want ErrBoardBindingNotFound after delete, got %v", err)
		}
	})

	t.Run("tenants are isolated", func(t *testing.T) {
		if err := store.Upsert(ctx, binding("team-x", time.Minute)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		other := binding("team-y", time.Minute)
		other.Owner = "OtherOrg"
		if err := store.Upsert(ctx, other); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		x, err := store.GetByTenant(ctx, "team-x")
		if err != nil {
			t.Fatalf("GetByTenant: %v", err)
		}
		if x.Owner != "SocialGouv" {
			t.Errorf("team-x binding leaked from team-y: %+v", x)
		}
	})
}

func TestMemoryBoardBindingStore_Conformance(t *testing.T) {
	runBoardBindingStoreSuite(t, forge.NewMemoryBoardBindingStore())
}

func TestMongoBoardBindingStore_Conformance(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo board-binding suite")
	}
	ctx := context.Background()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo.Connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_board_binding_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})

	store := forge.NewMongoBoardBindingStore(db)
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	// Idempotent re-run: a redeploy calls it again on an existing collection.
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema (second): %v", err)
	}
	runBoardBindingStoreSuite(t, store)
}
