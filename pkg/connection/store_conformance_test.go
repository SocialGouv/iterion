package connection_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
)

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// Every Store implementation answers the SAME questions the same way.
//
// This suite exists because a durable seam with two implementations drifts at
// the edges nobody tests — the tenant check on the third method, the error a
// missing record returns, whether an update may move an owner. Each new
// implementation (the Mongo twin next) adds one line here and inherits the
// whole contract, which is the only version of this that stays true.

func stores(t *testing.T) map[string]func() connection.Store {
	t.Helper()
	return map[string]func() connection.Store{
		"memory": func() connection.Store { return connection.NewMemoryStore() },
		"file": func() connection.Store {
			s, err := connection.NewFileStore(filepath.Join(t.TempDir(), "connections.json"))
			if err != nil {
				t.Fatalf("file store: %v", err)
			}
			return s
		},
	}
}

func TestStoreConformance(t *testing.T) {
	for name, build := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			t.Run("a missing connection is ErrNotFound, not an empty value", func(t *testing.T) {
				s := build()
				if _, err := s.Get(ctx, "tenant-a", "nope"); !errors.Is(err, connection.ErrNotFound) {
					t.Errorf("Get = %v, want ErrNotFound", err)
				}
				if _, err := s.ByAlias(ctx, "tenant-a", "probe", "nope"); !errors.Is(err, connection.ErrNotFound) {
					t.Errorf("ByAlias = %v, want ErrNotFound", err)
				}
				if err := s.Delete(ctx, "tenant-a", "nope"); !errors.Is(err, connection.ErrNotFound) {
					t.Errorf("Delete = %v, want ErrNotFound", err)
				}
				if err := s.Update(ctx, conn("nope", "tenant-a", "x")); !errors.Is(err, connection.ErrNotFound) {
					t.Errorf("Update = %v, want ErrNotFound", err)
				}
			})

			t.Run("another tenant's connection is invisible on every method", func(t *testing.T) {
				s := build()
				if err := s.Create(ctx, conn("c1", "tenant-a", "main")); err != nil {
					t.Fatalf("create: %v", err)
				}
				if _, err := s.Get(ctx, "tenant-b", "c1"); !errors.Is(err, connection.ErrNotFound) {
					t.Errorf("Get across tenants = %v, want ErrNotFound", err)
				}
				if _, err := s.ByAlias(ctx, "tenant-b", "probe", "main"); !errors.Is(err, connection.ErrNotFound) {
					t.Errorf("ByAlias across tenants = %v, want ErrNotFound", err)
				}
				if err := s.Delete(ctx, "tenant-b", "c1"); !errors.Is(err, connection.ErrNotFound) {
					t.Errorf("Delete across tenants = %v, want ErrNotFound", err)
				}
				got, err := s.List(ctx, "tenant-b", "")
				if err != nil || len(got) != 0 {
					t.Errorf("List across tenants = %d records (%v), want none", len(got), err)
				}
			})

			t.Run("an update may not move a connection between tenants", func(t *testing.T) {
				s := build()
				if err := s.Create(ctx, conn("c1", "tenant-a", "main")); err != nil {
					t.Fatalf("create: %v", err)
				}
				if err := s.Update(ctx, conn("c1", "tenant-b", "main")); !errors.Is(err, connection.ErrNotFound) {
					t.Errorf("cross-tenant update = %v, want ErrNotFound", err)
				}
				kept, err := s.Get(ctx, "tenant-a", "c1")
				if err != nil || kept.TenantID != "tenant-a" {
					t.Errorf("the record must still belong to tenant-a: %+v (%v)", kept.TenantID, err)
				}
			})

			t.Run("a duplicate id or alias is ErrExists", func(t *testing.T) {
				s := build()
				if err := s.Create(ctx, conn("c1", "tenant-a", "main")); err != nil {
					t.Fatalf("create: %v", err)
				}
				if err := s.Create(ctx, conn("c1", "tenant-a", "other")); !errors.Is(err, connection.ErrExists) {
					t.Errorf("duplicate id = %v, want ErrExists", err)
				}
				if err := s.Create(ctx, conn("c2", "tenant-a", "MAIN")); !errors.Is(err, connection.ErrExists) {
					t.Errorf("duplicate alias (case-insensitively) = %v, want ErrExists", err)
				}
			})

			t.Run("an invalid connection is refused by the STORE, not only by Validate", func(t *testing.T) {
				s := build()
				bad := conn("c1", "", "main") // no tenant
				if err := s.Create(ctx, bad); err == nil {
					t.Error("a connection with no owner must not be storable")
				}
			})

			t.Run("List filters by connector and is ordered", func(t *testing.T) {
				s := build()
				a := conn("c1", "tenant-a", "one")
				b := conn("c2", "tenant-a", "two")
				b.Connector = "other"
				for _, c := range []connection.Connection{a, b} {
					if err := s.Create(ctx, c); err != nil {
						t.Fatalf("create: %v", err)
					}
				}
				got, err := s.List(ctx, "tenant-a", "probe")
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if len(got) != 1 || got[0].ID != "c1" {
					t.Errorf("List(connector=probe) = %v, want only c1", got)
				}
				all, err := s.List(ctx, "tenant-a", "")
				if err != nil || len(all) != 2 {
					t.Errorf("List(all) = %d, want 2 (%v)", len(all), err)
				}
			})

			t.Run("the sealed payload survives a round trip", func(t *testing.T) {
				s := build()
				c := conn("c1", "tenant-a", "main")
				c.SealedPayload = []byte{0x01, 0x02, 0xff, 0x00, 0x7f}
				if err := s.Create(ctx, c); err != nil {
					t.Fatalf("create: %v", err)
				}
				got, err := s.Get(ctx, "tenant-a", "c1")
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				if string(got.SealedPayload) != string(c.SealedPayload) {
					t.Errorf("sealed payload = %v, want %v — a credential that does not survive storage is a connection that cannot authenticate", got.SealedPayload, c.SealedPayload)
				}
			})
		})
	}
}

// TestTheFileStoreSurvivesAProcessBoundary is the property a memory store
// cannot have, so it is tested where it lives: what one process wrote, the
// next one reads — including the sealed bytes.
func TestTheFileStoreSurvivesAProcessBoundary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "connections.json")

	first, err := connection.NewFileStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c := conn("c1", connection.LocalTenant, "main")
	c.SealedPayload = []byte("sealed-bytes")
	if err := first.Create(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A second store over the same path is what another `iterion` invocation
	// sees.
	second, err := connection.NewFileStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := second.ByAlias(ctx, connection.LocalTenant, "probe", "main")
	if err != nil {
		t.Fatalf("the connection must survive the process: %v", err)
	}
	if string(got.SealedPayload) != "sealed-bytes" {
		t.Errorf("sealed payload = %q, want it intact", got.SealedPayload)
	}

	// And a write through the SECOND handle must be visible to a third, with
	// the first handle's stale copy unable to resurrect what was deleted —
	// the concurrent-process case the re-read under lock exists for.
	if err := second.Delete(ctx, connection.LocalTenant, "c1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := first.Create(ctx, conn("c2", connection.LocalTenant, "second")); err != nil {
		t.Fatalf("create through the stale handle: %v", err)
	}
	third, err := connection.NewFileStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	all, err := third.List(ctx, connection.LocalTenant, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].ID != "c2" {
		t.Errorf("store holds %v; the stale handle's write must not have resurrected the deleted record", all)
	}
}

// TestAFutureFormatIsRefusedByNAME. A decoder ignores what it does not know,
// so a file written by a newer iterion would load as this version with its new
// guarantees silently absent — and the failure would surface as a connection
// that works but not as its author meant.
func TestAFutureFormatIsRefusedByNAME(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.json")
	if err := writeFile(path, `{"version": 99, "connections": []}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := connection.NewFileStore(path)
	if err == nil {
		t.Fatal("a newer format must be refused, not half-understood")
	}
	if !contains(err.Error(), "newer iterion") {
		t.Errorf("error = %v, want it to name the cause", err)
	}
}
