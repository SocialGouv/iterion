package connection_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// Every Store implementation answers the SAME questions the same way.
//
// This suite exists because a durable seam with two implementations drifts at
// the edges nobody tests — the tenant check on the third method, the error a
// missing record returns, whether an update may move an owner. Each new
// implementation adds one entry here and inherits the whole contract, which
// is the only version of this that stays true. The Mongo twin's entry is
// gated on ITERION_TEST_MONGO_URI, like every mongo suite in this tree, and
// the mongo-conformance CI job is what runs it (its package list names
// ./pkg/connection/... — pkg/store/mongo's ci_mongo_gate_test holds that
// sync).

func stores(t *testing.T) map[string]func() connection.Store {
	return storesBuilt(t, time.Now)
}

// storesBuilt is the same triple of twins, each pinned to the clock given —
// the default wall clock for the suite, a frozen one for the tests that need
// two writes to land inside one clock tick.
func storesBuilt(t *testing.T, clock func() time.Time) map[string]func() connection.Store {
	t.Helper()
	out := map[string]func() connection.Store{
		"memory": func() connection.Store { return connection.NewMemoryStore().WithClock(clock) },
		"file": func() connection.Store {
			s, err := connection.NewFileStore(filepath.Join(t.TempDir(), "connections.json"))
			if err != nil {
				t.Fatalf("file store: %v", err)
			}
			return s.WithClock(clock)
		},
	}
	if uri := os.Getenv("ITERION_TEST_MONGO_URI"); uri != "" {
		client, err := mongo.Connect(options.Client().ApplyURI(uri))
		if err != nil {
			t.Fatalf("mongo connect: %v", err)
		}
		nonce := make([]byte, 4)
		_, _ = rand.Read(nonce)
		db := client.Database("iterion_connection_" + hex.EncodeToString(nonce))
		t.Cleanup(func() {
			drop, dropCancel := mongotest.TeardownCtx()
			defer dropCancel()
			_ = db.Drop(drop)
			_ = client.Disconnect(drop)
		})
		out["mongo"] = func() connection.Store {
			// A FRESH COLLECTION per build: the suite's contract is that every
			// store starts empty, and a shared one would leak a subtest's
			// records into the next.
			nonce := make([]byte, 6)
			_, _ = rand.Read(nonce)
			s := connection.NewMongoStoreIn(db, "connections_test_"+hex.EncodeToString(nonce))
			schemaCtx, schemaCancel := mongotest.Ctx(t)
			defer schemaCancel()
			if err := s.EnsureSchema(schemaCtx); err != nil {
				t.Fatalf("mongo schema: %v", err)
			}
			t.Cleanup(func() {
				drop, dropCancel := mongotest.TeardownCtx()
				defer dropCancel()
				_ = s.Drop(drop)
			})
			return s.WithClock(clock)
		}
	}
	return out
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

			// A store hands out COPIES, in both directions.
			//
			// FileStore is immune by construction — it re-decodes the file on
			// every access — so this row exists for the twins that do not:
			// MemoryStore held the caller's arrays, and the Mongo twin will
			// have its own answer. Left untested, the divergence is invisible
			// until a caller mutates a capability list and rewrites a grant
			// with no Update, no lock, and nothing in the audit.
			t.Run("a record shares no backing array with its caller", func(t *testing.T) {
				s := build()
				c := conn("c1", "tenant-a", "main")
				c.Capabilities = []connection.Capability{connection.CapAction, connection.CapAgent}
				c.GrantedScopes, c.ScopesKnown = []string{"read:issue"}, true
				c.SealedPayload = []byte("sealed")
				if err := s.Create(ctx, c); err != nil {
					t.Fatalf("create: %v", err)
				}

				// The WRITE direction: the caller still holds the slices it
				// passed in.
				c.Capabilities[0] = "root"
				c.GrantedScopes[0] = "admin"
				c.SealedPayload[0] = 'X'

				got, err := s.Get(ctx, "tenant-a", "c1")
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				if got.Capabilities[0] != connection.CapAction {
					t.Errorf("the caller's mutation reached the store: capability = %q", got.Capabilities[0])
				}
				if got.GrantedScopes[0] != "read:issue" {
					t.Errorf("the caller's mutation reached the store: scope = %q", got.GrantedScopes[0])
				}
				if string(got.SealedPayload) != "sealed" {
					t.Errorf("the caller's mutation reached the stored credential: %q", got.SealedPayload)
				}

				// The READ direction: mutating what a read returned.
				got.Capabilities[0] = "root"
				got.GrantedScopes[0] = "admin"
				got.SealedPayload[0] = 'X'

				again, err := s.Get(ctx, "tenant-a", "c1")
				if err != nil {
					t.Fatalf("get again: %v", err)
				}
				if again.Capabilities[0] != connection.CapAction {
					t.Errorf("a reader's mutation reached the store: capability = %q", again.Capabilities[0])
				}
				if again.GrantedScopes[0] != "read:issue" {
					t.Errorf("a reader's mutation reached the store: scope = %q", again.GrantedScopes[0])
				}
				if string(again.SealedPayload) != "sealed" {
					t.Errorf("a reader's mutation reached the stored credential: %q", again.SealedPayload)
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

			// #2761: what may be BELIEVED is decided when the connection is
			// entrusted, so the grant that decides it has to SURVIVE being
			// stored — by both implementations. A store that dropped the
			// third value would leave the record readable and the grant gone,
			// which is the shape an operator cannot see.
			t.Run("a connection carrying the trigger grant round-trips", func(t *testing.T) {
				s := build()
				c := conn("c1", "tenant-a", "main")
				c.Capabilities = []connection.Capability{connection.CapAction, connection.CapTrigger}
				// Through Create, which calls Validate: a capability this
				// build did not admit would be refused here rather than
				// stored and dropped.
				if err := s.Create(ctx, c); err != nil {
					t.Fatalf("create: %v", err)
				}
				got, err := s.Get(ctx, "tenant-a", "c1")
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				if len(got.Capabilities) != 2 || got.Capabilities[0] != connection.CapAction || got.Capabilities[1] != connection.CapTrigger {
					t.Fatalf("capabilities = %v, want [action trigger] in that order", got.Capabilities)
				}
				if !got.Has(connection.CapTrigger) {
					t.Error("Has(trigger) = false on a record that declares it")
				}
				// And through the LIST path too, which is a different read on
				// the file store.
				listed, err := s.List(ctx, "tenant-a", "probe")
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if len(listed) != 1 || !listed[0].Has(connection.CapTrigger) {
					t.Errorf("listed = %+v, want the trigger grant on it", listed)
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

// TestAnUpdateKeepsTheCredential — on EVERY store.
//
// `Connection.SealedPayload` is `json:"-"`, so a record rebuilt from the
// TRANSPORT shape — the studio's PATCH, which is what this exported interface
// exists for — carries no credential at all. Update being a full replace, it
// wrote the payload away: the connection survived, `connections list` still
// showed it active, and it 401'd at its next call with an error that reads
// like a bad token. The same omission was in BOTH twins, which is precisely
// why the suite could not see it — so the assertion belongs here.
//
// Rotation still works, and is what an update carrying a payload MEANS.
func TestAnUpdateKeepsTheCredential(t *testing.T) {
	for name, build := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := build()
			c := conn("c1", "tenant-a", "main")
			c.SealedPayload = []byte("sealed-original")
			if err := s.Create(ctx, c); err != nil {
				t.Fatalf("create: %v", err)
			}

			// The transport shape: everything but the credential. Read
			// first, because Update's precondition is the record's own
			// UpdatedAt — which is what the one caller (the PATCH route) has
			// and what a blind writer must not be allowed to skip.
			edit, err := s.Get(ctx, "tenant-a", "c1")
			if err != nil {
				t.Fatalf("read before update: %v", err)
			}
			edit.SealedPayload = nil
			edit.DisplayName = "renamed for display only"
			if err := s.Update(ctx, edit); err != nil {
				t.Fatalf("update: %v", err)
			}
			got, err := s.Get(ctx, "tenant-a", "c1")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if string(got.SealedPayload) != "sealed-original" {
				t.Errorf("after an edit the credential is %q, want the stored one kept — "+
					"a record rebuilt from the API shape carries none, and wiping it leaves a connection that 401s", got.SealedPayload)
			}

			// And an update that DOES carry one rotates it.
			rot, err := s.Get(ctx, "tenant-a", "c1")
			if err != nil {
				t.Fatalf("read before rotation: %v", err)
			}
			rot.SealedPayload = []byte("sealed-rotated")
			if err := s.Update(ctx, rot); err != nil {
				t.Fatalf("rotate: %v", err)
			}
			got, err = s.Get(ctx, "tenant-a", "c1")
			if err != nil {
				t.Fatalf("get after rotation: %v", err)
			}
			if string(got.SealedPayload) != "sealed-rotated" {
				t.Errorf("after a rotation the credential is %q, want the new one", got.SealedPayload)
			}
		})
	}
}

// TestARenameCannotCollideWithAnotherAlias — on EVERY store.
//
// Uniqueness was enforced at creation only, so renaming one connection onto
// another's alias left two answering the same name, and `ByAlias` returns
// whichever the map iteration reaches first. Identical workflow input then
// selected a different credential — or a different vendor instance — between
// two runs of the same `.bot`. A uniqueness rule enforced only at creation is
// not one.
func TestARenameCannotCollideWithAnotherAlias(t *testing.T) {
	for name, build := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := build()
			for _, c := range []connection.Connection{
				conn("c1", "tenant-a", "main"),
				conn("c2", "tenant-a", "secondary"),
			} {
				if err := s.Create(ctx, c); err != nil {
					t.Fatalf("create: %v", err)
				}
			}

			// The normalisation is part of the rule: " MAIN " and "main" are
			// the same name to `ByAlias`, so they must be the same name here.
			clash, err := s.Get(ctx, "tenant-a", "c2")
			if err != nil {
				t.Fatalf("read c2: %v", err)
			}
			clash.Alias = "  MAIN  "
			if err := s.Update(ctx, clash); !errors.Is(err, connection.ErrExists) {
				t.Errorf("rename onto an existing alias = %v, want ErrExists", err)
			}
			got, err := s.ByAlias(ctx, "tenant-a", "probe", "main")
			if err != nil || got.ID != "c1" {
				t.Errorf("`main` resolves to %q (%v), want the original c1", got.ID, err)
			}

			// A record may of course keep its OWN alias through an update.
			same, err := s.Get(ctx, "tenant-a", "c1")
			if err != nil {
				t.Fatalf("read c1: %v", err)
			}
			same.DisplayName = "renamed for display only"
			if err := s.Update(ctx, same); err != nil {
				t.Errorf("a connection must be able to keep its own alias: %v", err)
			}
		})
	}
}

// TestAnUpdateRefusesARecordThatMovedSinceItWasRead — on EVERY store.
//
// The window that matters belongs to the CALLER: an HTTP PATCH reads the whole
// record, applies its own fields and writes the whole thing back, so a lock
// held inside Update spans none of it. Last-write-wins there meant a
// revocation, a rotation and a capability narrowing were each reverted by a
// concurrent cosmetic edit, with both requests answering 200 — the operator
// revoked a leaked connection, was told it worked, and the credential kept
// authenticating.
//
// The precondition is the record's own UpdatedAt, which is why the suite also
// pins that a stale writer loses and the WINNER's change survives: a guard
// that refused both would be a different bug.
func TestAnUpdateRefusesARecordThatMovedSinceItWasRead(t *testing.T) {
	for name, build := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := build()
			if err := s.Create(ctx, conn("c1", "tenant-a", "main")); err != nil {
				t.Fatalf("create: %v", err)
			}
			// TWO readers of one record, as two requests are.
			first, err := s.Get(ctx, "tenant-a", "c1")
			if err != nil {
				t.Fatalf("first read: %v", err)
			}
			second, err := s.Get(ctx, "tenant-a", "c1")
			if err != nil {
				t.Fatalf("second read: %v", err)
			}

			// The security change lands.
			first.Status = connection.StatusRevoked
			first.StatusReason = "leaked"
			if err := s.Update(ctx, first); err != nil {
				t.Fatalf("the revocation must land: %v", err)
			}

			// The cosmetic edit, built on the stale read, must NOT.
			second.DisplayName = "renamed while the other request revoked it"
			if err := s.Update(ctx, second); !errors.Is(err, connection.ErrConflict) {
				t.Errorf("a stale update = %v, want ErrConflict — a 200 here is how a revocation is lost", err)
			}
			got, err := s.Get(ctx, "tenant-a", "c1")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Status != connection.StatusRevoked || got.StatusReason != "leaked" {
				t.Errorf("stored status = %q/%q, want the revocation kept", got.Status, got.StatusReason)
			}
			if got.DisplayName != "" {
				t.Errorf("the refused edit landed anyway: display_name = %q", got.DisplayName)
			}

			// And the loser succeeds once it re-reads — a conflict is "read
			// it again", not "this record is now unwritable".
			retry, err := s.Get(ctx, "tenant-a", "c1")
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			retry.DisplayName = "renamed after re-reading"
			if err := s.Update(ctx, retry); err != nil {
				t.Errorf("a re-read update = %v, want it to land", err)
			}
		})
	}
}

// TestTheAliasRuleIsOneNormalisationOnEveryStore — on EVERY store.
//
// The comparison in code (aliasEq) and the value behind the cloud twin's
// unique index (aliasNorm) were two different normalisations that agreed on
// ASCII: case FOLDING one side, a case MAPPING the other. So σ/ς and s/ſ
// collided in code and not in the index, while i/İ collided in the index and
// not in code — and `connection: İ` in a `.bot` resolved a DIFFERENT
// credential on the cloud tier than on the local one, which is the exact
// failure the uniqueness rule exists to prevent. The suite's only alias case
// was "main" vs "MAIN", so it could not see any of it.
func TestTheAliasRuleIsOneNormalisationOnEveryStore(t *testing.T) {
	for name, build := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for _, tc := range []struct {
				name   string
				first  string
				second string
				// same is whether the two are ONE name: the store must
				// refuse the second, and both must resolve to the first.
				same bool
			}{
				// The ASCII case the rule is written for.
				{"ascii case", "main", "MAIN", true},
				// A dotted capital I lowercases (simple mapping) to a plain
				// i, so the two ARE one name.
				{"dotted capital I", "i", "İ", true},
				// A final sigma folds to a non-final sigma but does not MAP
				// to one, so the two are two names.
				{"greek final sigma", "σ", "ς", false},
				// Likewise a long s.
				{"latin long s", "s", "ſ", false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					s := build()
					if err := s.Create(ctx, conn("c1", "tenant-a", tc.first)); err != nil {
						t.Fatalf("create %q: %v", tc.first, err)
					}
					err := s.Create(ctx, conn("c2", "tenant-a", tc.second))
					if tc.same {
						if !errors.Is(err, connection.ErrExists) {
							t.Fatalf("creating %q beside %q = %v, want ErrExists — one name, one credential", tc.second, tc.first, err)
						}
						// And the one name resolves to the one record, from
						// either spelling.
						for _, alias := range []string{tc.first, tc.second} {
							got, gerr := s.ByAlias(ctx, "tenant-a", "probe", alias)
							if gerr != nil || got.ID != "c1" {
								t.Errorf("ByAlias(%q) = %q (%v), want c1", alias, got.ID, gerr)
							}
						}
						return
					}
					if err != nil {
						t.Fatalf("creating %q beside %q = %v, want it accepted — two names", tc.second, tc.first, err)
					}
					// Two names, each resolving to its OWN record: the
					// divergence showed up as one spelling answering the
					// other's credential.
					for alias, want := range map[string]string{tc.first: "c1", tc.second: "c2"} {
						got, gerr := s.ByAlias(ctx, "tenant-a", "probe", alias)
						if gerr != nil || got.ID != want {
							t.Errorf("ByAlias(%q) = %q (%v), want %s", alias, got.ID, gerr, want)
						}
					}
				})
			}
		})
	}
}

// TestAWriteMovesTheVersionEvenWhenTheClockDoesNot — on EVERY store, under a
// FROZEN clock.
//
// The precondition is the record's own UpdatedAt, and the cloud twin stores
// it at millisecond precision: a bare clock read can repeat the stored value,
// and two writers that both read inside that tick would then both pass a
// precondition that never moved — the second silently overwriting the first,
// the exact shape the precondition exists to refuse. nextStamp forces the
// stamp past the stored one, so the loser's precondition is stale by
// construction.
//
// Mutation that reddens this: write the bare `s.clock()` back over
// nextStamp in any twin's Update.
func TestAWriteMovesTheVersionEvenWhenTheClockDoesNot(t *testing.T) {
	frozen := time.Now().Truncate(time.Millisecond)
	for name, build := range storesBuilt(t, func() time.Time { return frozen }) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := build()
			if err := s.Create(ctx, conn("c1", "tenant-a", "main")); err != nil {
				t.Fatalf("create: %v", err)
			}
			first, err := s.Get(ctx, "tenant-a", "c1")
			if err != nil {
				t.Fatalf("first read: %v", err)
			}
			second, err := s.Get(ctx, "tenant-a", "c1")
			if err != nil {
				t.Fatalf("second read: %v", err)
			}

			first.DisplayName = "writer A"
			if err := s.Update(ctx, first); err != nil {
				t.Fatalf("writer A must land: %v", err)
			}

			// Same carried version, same frozen clock: only a stamp that is
			// FORCED to move can tell A's write from B's.
			second.DisplayName = "writer B"
			if err := s.Update(ctx, second); !errors.Is(err, connection.ErrConflict) {
				t.Errorf("writer B = %v, want ErrConflict — a version that does not move lets both writers land", err)
			}
			got, err := s.Get(ctx, "tenant-a", "c1")
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if got.DisplayName != "writer A" {
				t.Errorf("display_name = %q, want writer A's edit kept — B overwrote it", got.DisplayName)
			}
		})
	}
}
