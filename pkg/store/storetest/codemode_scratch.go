package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// CodemodeScratch runs against independently opened stores over one bank.
func CodemodeScratch(t *testing.T, s, peer store.RunStore) {
	ctx := testCtx()
	a, b := store.AsCodemodeScratchStore(s), store.AsCodemodeScratchStore(peer)
	if a == nil || b == nil {
		t.Fatal("codemode scratch capability not exposed")
	}
	mustCreate := func(t *testing.T, id string) *store.Run {
		t.Helper()
		r, err := s.CreateRun(ctx, id, "scratch", nil)
		if err != nil {
			t.Fatal(err)
		}
		// A local filesystem run has no tenant by default; bind its metadata
		// explicitly to exercise isolation for scoped filesystem runs too.
		if r.TenantID == "" {
			r.TenantID, _ = store.TenantFromContext(ctx)
			if err := s.SaveRun(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	mustLoad := func(t *testing.T, bank store.CodemodeScratchStore, ctx context.Context, id, key, want string, present bool, wantErr error) {
		t.Helper()
		value, ok, err := bank.LoadCodemodeScratch(ctx, id, key)
		if string(value) != want || ok != present || !errors.Is(err, wantErr) {
			t.Fatalf("load: present=%v, bytes=%d, error=%v; expected present=%v, bytes=%d, error=%v", ok, len(value), err, present, len(want), wantErr)
		}
	}
	mustPut := func(t *testing.T, bank store.CodemodeScratchStore, ctx context.Context, id, key, value string, limits store.CodemodeScratchLimits, wantErr error) {
		t.Helper()
		if err := bank.PutCodemodeScratch(ctx, id, key, json.RawMessage(value), limits); !errors.Is(err, wantErr) {
			t.Fatalf("put error=%v; expected %v", err, wantErr)
		}
	}
	defaults := store.CodemodeScratchLimits{}
	t.Run("PersistenceAndIsolation", func(t *testing.T) {
		const id = "scratch-persistence"
		const key = "../$key.é\x00"
		mustCreate(t, id)
		mustLoad(t, b, ctx, id, key, "", false, nil)
		mustPut(t, a, ctx, id, key, " null ", defaults, nil)
		mustLoad(t, b, ctx, id, key, "null", true, nil)
		const canonical = `{"$x":["<&>",9007199254740993],"a.b":null}`
		mustPut(t, b, ctx, id, key, ` {"$x": [ "<&>", 9007199254740993 ], "a.b": null } `, defaults, nil)
		r, err := peer.LoadRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		r.Name = "ordinary save"
		if err := peer.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		mustLoad(t, a, ctx, id, key, canonical, true, nil)
		const otherID = "scratch-other"
		mustCreate(t, otherID)
		mustLoad(t, b, ctx, otherID, key, "", false, nil)
		otherTenant := store.WithTenant(ctx, "other-tenant")
		mustLoad(t, b, otherTenant, id, key, "", false, store.ErrRunNotFound)
		mustPut(t, b, otherTenant, id, key, `false`, defaults, store.ErrRunNotFound)
		mustLoad(t, a, ctx, id, key, canonical, true, nil)
		if err := s.DeleteRun(ctx, id); err != nil {
			t.Fatal(err)
		}
		mustLoad(t, b, otherTenant, id, key, "", false, store.ErrRunNotFound)
		mustPut(t, b, otherTenant, id, key, `false`, defaults, store.ErrRunNotFound)
		mustLoad(t, b, ctx, id, key, "", false, store.ErrRunDeleted)
	})
	t.Run("OriginalRunBinding", func(t *testing.T) {
		const id, key = "scratch-origin", "key"
		r := mustCreate(t, id)
		observation, err := store.CaptureRunObservation(store.RunDataHeader{
			RunID: id, Format: store.ValueFormat(r.FormatVersion), Protocol: r.DataProtocol, BirthID: r.BirthID,
		}, r.TenantID, true)
		if err != nil {
			t.Fatal(err)
		}
		bound, err := store.BindRunObservation(ctx, observation)
		if err != nil {
			t.Fatal(err)
		}
		mustPut(t, a, bound, id, key, `true`, defaults, nil)
		stale := observation.Header()
		stale.BirthID = store.NewRunBirthID()
		binding, err := store.CaptureRunDataBinding(stale)
		if err != nil {
			t.Fatal(err)
		}
		obsolete, err := store.WithRunDataBinding(ctx, binding)
		if err != nil {
			t.Fatal(err)
		}
		mustLoad(t, b, obsolete, id, key, "", false, store.ErrRunDataIdentityChanged)
		mustPut(t, b, obsolete, id, key, `false`, defaults, store.ErrRunDataIdentityChanged)
		mustLoad(t, b, bound, id, key, `true`, true, nil)
	})
	t.Run("Validation", func(t *testing.T) {
		const id = "scratch-validation"
		mustCreate(t, id)
		const key = "k"
		mustPut(t, a, ctx, id, key, `true`, defaults, nil)
		for _, bad := range []string{"", strings.Repeat("é", 65), "\xff"} {
			mustPut(t, b, ctx, id, bad, `true`, defaults, store.ErrCodemodeScratchInvalid)
			mustLoad(t, a, ctx, id, bad, "", false, store.ErrCodemodeScratchInvalid)
		}
		for _, bad := range []string{"", `null false`, `{"private-sentinel":}`, "\x22\xff\x22", `"` + strings.Repeat("x", store.CodemodeScratchMaxValueBytes-1) + `"`} {
			mustPut(t, b, ctx, id, key, bad, defaults, store.ErrCodemodeScratchInvalid)
		}
		mustPut(t, b, ctx, id, key, `false`, store.CodemodeScratchLimits{Bytes: -1}, store.ErrCodemodeScratchInvalid)
		mustPut(t, b, ctx, id, key, `false`, store.CodemodeScratchLimits{Entries: store.CodemodeScratchMaxEntries + 1}, store.ErrCodemodeScratchInvalid)
		mustPut(t, b, ctx, id, key, `false`, store.CodemodeScratchLimits{Bytes: store.CodemodeScratchMaxBytes + 1}, store.ErrCodemodeScratchInvalid)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		mustPut(t, b, cancelled, id, key, `false`, defaults, context.Canceled)
		mustLoad(t, b, cancelled, id, key, "", false, context.Canceled)
		mustLoad(t, a, ctx, id, key, `true`, true, nil)
		edgeKey := strings.Repeat("é", 64)
		edgeValue := `"` + strings.Repeat("x", store.CodemodeScratchMaxValueBytes-2) + `"`
		mustPut(t, a, ctx, id, edgeKey, edgeValue, defaults, nil)
		mustLoad(t, b, ctx, id, edgeKey, edgeValue, true, nil)
	})
	t.Run("Quota", func(t *testing.T) {
		const id = "scratch-quota"
		mustCreate(t, id)
		// Key costs are UTF-8 bytes, and values use the compact stored form.
		limits := store.CodemodeScratchLimits{Entries: 2, Bytes: 10}
		mustPut(t, a, ctx, id, "é", `  true `, limits, nil)
		mustPut(t, b, ctx, id, "é", `  null `, limits, nil)
		mustPut(t, b, ctx, id, "z", `123`, limits, nil)
		mustPut(t, a, ctx, id, "z", `1234`, limits, store.ErrCodemodeScratchQuota)
		mustPut(t, b, ctx, id, "q", `0`, store.CodemodeScratchLimits{Entries: 2, Bytes: 100}, store.ErrCodemodeScratchQuota)
		mustLoad(t, a, ctx, id, "q", "", false, nil)
		mustLoad(t, b, ctx, id, "z", `123`, true, nil)
		lower := store.CodemodeScratchLimits{Entries: 1, Bytes: 3}
		mustPut(t, a, ctx, id, "é", `0`, lower, nil)
		mustPut(t, b, ctx, id, "é", `9`, lower, nil)
		mustPut(t, b, ctx, id, "é", `10`, store.CodemodeScratchLimits{Entries: 1, Bytes: 100}, store.ErrCodemodeScratchQuota)
		mustLoad(t, b, ctx, id, "é", `9`, true, nil)
	})
	t.Run("ContestedWriters", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			limits store.CodemodeScratchLimits
		}{
			{"entries", store.CodemodeScratchLimits{Entries: 1, Bytes: 100}},
			{"bytes", store.CodemodeScratchLimits{Entries: 10, Bytes: 4}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id := "scratch-race-" + tc.name
				mustCreate(t, id)
				start := make(chan struct{})
				var group sync.WaitGroup
				var results [10]error
				for i := range results {
					group.Go(func() {
						<-start
						bank := []store.CodemodeScratchStore{a, b}[i%2]
						results[i] = bank.PutCodemodeScratch(ctx, id, fmt.Sprintf("k%d", i), json.RawMessage(`42`), tc.limits)
					})
				}
				close(start)
				group.Wait()
				wins := 0
				for i, err := range results {
					key := fmt.Sprintf("k%d", i)
					if err == nil {
						wins++
						mustLoad(t, b, ctx, id, key, `42`, true, nil)
					} else if errors.Is(err, store.ErrCodemodeScratchQuota) {
						mustLoad(t, b, ctx, id, key, "", false, nil)
					} else {
						t.Fatalf("contested put: %v", err)
					}
				}
				if wins != 1 {
					t.Fatalf("acknowledged writes = %d, want exactly one", wins)
				}
			})
		}
	})
	t.Run("NoResurrection", func(t *testing.T) {
		const id, key = "scratch-lifecycle", "k"
		mustPut(t, a, ctx, id, key, `true`, defaults, store.ErrRunNotFound)
		mustLoad(t, b, ctx, id, key, "", false, store.ErrRunNotFound)
		mustCreate(t, id)
		mustLoad(t, a, ctx, id, key, "", false, nil)
		mustPut(t, a, ctx, id, key, `true`, defaults, nil)
		start := make(chan struct{})
		var group sync.WaitGroup
		var deleteErr, putErr error
		group.Go(func() { <-start; deleteErr = s.DeleteRun(ctx, id) })
		group.Go(func() { <-start; putErr = b.PutCodemodeScratch(ctx, id, key, json.RawMessage(`false`), defaults) })
		close(start)
		group.Wait()
		if deleteErr != nil || (putErr != nil && !errors.Is(putErr, store.ErrRunDeleted)) {
			t.Fatalf("delete/put: delete=%v, put=%v", deleteErr, putErr)
		}
		mustPut(t, b, ctx, id, key, `true`, defaults, store.ErrRunDeleted)
		mustLoad(t, b, ctx, id, key, "", false, store.ErrRunDeleted)
	})
}
