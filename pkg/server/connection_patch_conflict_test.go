package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
)

// PATCH is a read-modify-write, and the window is the HANDLER's.
//
// handlePatchConnection reads the whole record (credential included), applies
// the body's fields and writes the whole thing back. A lock inside the store's
// Update spans none of that, so a cosmetic PATCH landing in the middle of a
// revocation, a rotation or a capability narrowing overwrote it — both
// requests answering 200, `connections list` still reading `active`, and the
// leaked credential still authenticating.
//
// Mutation that reddens these (producer): drop the `checkUnchanged(prev, c)`
// call from MemoryStore.Update (pkg/connection/store.go), or the ErrConflict
// arm from handlePatchConnection.

// parkingConnectionStore interleaves two requests deterministically: the
// first Get after arm() parks until the test releases it, which is the
// handler's own read-modify-write window held open.
//
// A park rather than two goroutines racing: a test that can only redden on one
// interleaving is flaky, not a witness.
type parkingConnectionStore struct {
	connection.Store
	mu     sync.Mutex
	parked chan struct{}
	done   chan struct{}
}

// arm installs one parking and hands back the handles. They travel as
// closures, not field reads, because Get nils the FIELDS when it consumes the
// parking (so the following request passes straight through) — a field read
// after that consumption is a data race with a nil close at the end of it.
func (s *parkingConnectionStore) arm() (waitParked func(), release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parked, done := make(chan struct{}), make(chan struct{})
	s.parked, s.done = parked, done
	return func() { <-parked }, func() { close(done) }
}

func (s *parkingConnectionStore) Get(ctx context.Context, tenantID, id string) (connection.Connection, error) {
	s.mu.Lock()
	parked, done := s.parked, s.done
	s.parked = nil
	s.mu.Unlock()
	c, err := s.Store.Get(ctx, tenantID, id)
	if parked != nil {
		close(parked)
		<-done
	}
	return c, err
}

func patchTenantConnection(t *testing.T, s *Server, ctx context.Context, teamID, connID, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handlePatchConnection(w, connReq(ctx, "PATCH", "/api/teams/"+teamID+"/connections/"+connID, body, teamID, connID))
	return w
}

func TestAConcurrentCosmeticPatchCannotUndoASecurityChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		// security is the PATCH an operator makes under pressure.
		security string
		// check reads the stored record and says what must have survived.
		check func(t *testing.T, c connection.Connection)
	}{
		{
			name:     "a revocation",
			security: `{"status":"revoked","status_reason":"leaked"}`,
			check: func(t *testing.T, c connection.Connection) {
				if c.Status != connection.StatusRevoked {
					t.Errorf("status = %q, want revoked — the leaked credential still authenticates", c.Status)
				}
			},
		},
		{
			name:     "a capability narrowing",
			security: `{"capabilities":["trigger"]}`,
			check: func(t *testing.T, c connection.Connection) {
				if c.Has(connection.CapAction) || !c.Has(connection.CapTrigger) {
					t.Errorf("capabilities = %v, want [trigger] — the token is spendable again", c.Capabilities)
				}
			},
		},
		{
			name:     "a rotation",
			security: `{"token":"the-rotated-token"}`,
			check: func(t *testing.T, c connection.Connection) {
				// The sealed bytes differ per seal, so the proof is that the
				// stored payload is NOT the one the create sealed.
				if len(c.SealedPayload) == 0 {
					t.Fatal("the record lost its credential")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newConnectionTestServer(t)
			park := &parkingConnectionStore{Store: s.connectionStore}
			s.connectionStore = park
			ctx := connUserCtx("admin1")
			created := createTestConnection(t, s, ctx, "t1")
			before, err := park.Store.Get(context.Background(), "t1", created.ID)
			if err != nil {
				t.Fatalf("read the created record: %v", err)
			}

			// The cosmetic request reads, then parks inside the handler.
			waitParked, release := park.arm()
			var cosmetic *httptest.ResponseRecorder
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				cosmetic = patchTenantConnection(t, s, ctx, "t1", created.ID, `{"display_name":"a label"}`)
			}()
			waitParked()

			// …while the security change lands whole.
			if w := patchTenantConnection(t, s, ctx, "t1", created.ID, tc.security); w.Code != http.StatusOK {
				t.Fatalf("the security PATCH must land: code=%d body=%s", w.Code, w.Body.String())
			}

			// Now let the stale writer through.
			release()
			wg.Wait()
			if cosmetic.Code != http.StatusConflict {
				t.Errorf("the stale cosmetic PATCH answered %d, want 409 — a 200 tells the operator a change landed that was overwritten", cosmetic.Code)
			}
			if body := cosmetic.Body.String(); !strings.Contains(body, "read it again") {
				t.Errorf("the refusal must name the retry: %s", body)
			}

			stored, err := park.Store.Get(context.Background(), "t1", created.ID)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			tc.check(t, stored)
			if stored.DisplayName != "" {
				t.Errorf("the refused edit landed anyway: display_name = %q", stored.DisplayName)
			}
			if tc.name == "a rotation" && string(stored.SealedPayload) == string(before.SealedPayload) {
				t.Error("the rotation was reverted: the stored credential is the pre-rotation one")
			}
		})
	}
}

// An explicit empty capability list is a request that asks for NOTHING, and
// the create-side default answered `action` to it: a connection created
// `[trigger]` came back spendable by a deterministic node, 200, from a body
// that named no capability at all.
func TestAPatchWithAnEmptyCapabilityListIsRefused(t *testing.T) {
	s := newConnectionTestServer(t)
	ctx := connUserCtx("admin1")
	created := createTestConnection(t, s, ctx, "t1")
	if w := patchTenantConnection(t, s, ctx, "t1", created.ID, `{"capabilities":["trigger"]}`); w.Code != http.StatusOK {
		t.Fatalf("narrowing to [trigger]: code=%d body=%s", w.Code, w.Body.String())
	}

	w := patchTenantConnection(t, s, ctx, "t1", created.ID, `{"capabilities":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("an empty capability list answered %d, want 400: %s", w.Code, w.Body.String())
	}
	stored, err := s.connectionStore.Get(context.Background(), "t1", created.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.Has(connection.CapAction) || !stored.Has(connection.CapTrigger) {
		t.Errorf("capabilities = %v, want the operator's [trigger] kept — `action` makes the token spendable by a deterministic node", stored.Capabilities)
	}

	// An ABSENT list still means "leave them alone", which is the intent the
	// empty one must not be read as.
	if w := patchTenantConnection(t, s, ctx, "t1", created.ID, `{"display_name":"untouched"}`); w.Code != http.StatusOK {
		t.Fatalf("a PATCH naming no capability: code=%d body=%s", w.Code, w.Body.String())
	}
	stored, err = s.connectionStore.Get(context.Background(), "t1", created.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !stored.Has(connection.CapTrigger) || stored.Has(connection.CapAction) {
		t.Errorf("capabilities = %v, want [trigger] untouched", stored.Capabilities)
	}
}

// The 200 body must carry the STORED record: `updated_at` is the stamp the
// caller's next PATCH sends back as its precondition, and the write advanced
// it. A body serving the pre-write value makes every following edit answer
// 409 against a version nothing still holds.
//
// Mutation that reddens this: serve `conn` (the record the handler built)
// instead of the read-back one.
func TestAPatchAnswerServesTheRecordAsStored(t *testing.T) {
	s := newConnectionTestServer(t)
	ctx := connUserCtx("admin1")
	created := createTestConnection(t, s, ctx, "t1")

	w := patchTenantConnection(t, s, ctx, "t1", created.ID, `{"display_name":"renamed"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch: code=%d body=%s", w.Code, w.Body.String())
	}
	var dto connectionDTO
	if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	stored, err := s.connectionStore.Get(context.Background(), "t1", created.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !dto.UpdatedAt.Equal(stored.UpdatedAt) {
		t.Errorf("the answer's updated_at = %s, the stored record's = %s — a caller echoing the served version would 409 on its next edit",
			dto.UpdatedAt.Format(time.RFC3339Nano), stored.UpdatedAt.Format(time.RFC3339Nano))
	}
	if dto.DisplayName != "renamed" {
		t.Errorf("display_name = %q, want the applied edit", dto.DisplayName)
	}
}
