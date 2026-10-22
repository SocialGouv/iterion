package forge

import (
	"context"
	"errors"
	"testing"
	"time"
)

// RevokeIfPayload is the compare-and-swap markRevoked relies on: the write
// lands only while the stored credential is the one the caller loaded, and
// it names only the fields a revocation owns.
func TestConnectionStore_RevokeIfPayloadIsAPartialCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryConnectionStore()
	exp := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	if err := st.Create(ctx, Connection{ID: "c1", TenantID: "t1", Status: StatusActive, SealedPayload: []byte("v1"), AccessTokenExpiresAt: &exp, DisplayName: "jo", WebhookBaseURL: "https://hook.example"}); err != nil {
		t.Fatal(err)
	}
	at := exp.Add(time.Hour)

	if ok, err := st.RevokeIfPayload(ctx, "c1", []byte("v0"), at, false); err != nil || ok {
		t.Fatalf("mismatching payload: written=%v err=%v, want false, nil", ok, err)
	}
	if got, _ := st.Get(ctx, "c1"); got.Status != StatusActive {
		t.Fatalf("a mismatching CAS wrote the row: status %q", got.Status)
	}
	if ok, err := st.RevokeIfPayload(ctx, "c1", []byte("v1"), at, false); err != nil || !ok {
		t.Fatalf("matching payload: written=%v err=%v, want true, nil", ok, err)
	}
	got, _ := st.Get(ctx, "c1")
	if got.Status != StatusNeedsReauth || !got.UpdatedAt.Equal(at) {
		t.Fatalf("a matching CAS did not flip the row: status %q updated_at %v", got.Status, got.UpdatedAt)
	}
	if got.DisplayName != "jo" || got.WebhookBaseURL != "https://hook.example" || got.AccessTokenExpiresAt == nil {
		t.Fatalf("the revoke touched fields it does not own: %+v", got)
	}
	if ok, _ := st.RevokeIfPayload(ctx, "c1", []byte("v1"), at, true); !ok {
		t.Fatal("clearExpiry: want written")
	}
	if got, _ := st.Get(ctx, "c1"); got.AccessTokenExpiresAt != nil {
		t.Fatal("clearExpiry must drop the recorded expiry")
	}
	if _, err := st.RevokeIfPayload(ctx, "nope", []byte("v1"), at, false); !errors.Is(err, ErrConnectionNotFound) {
		t.Fatalf("missing row: err = %v, want ErrConnectionNotFound", err)
	}
}
