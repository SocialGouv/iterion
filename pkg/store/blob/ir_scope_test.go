package blob

import (
	"errors"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestScopedIRLateWriterAndCleanup(t *testing.T) {
	t.Parallel()
	_, srv := newFakeS3(t, "b")
	c, err := NewS3(t.Context(), Config{Bucket: "b", Region: "us-east-1", Endpoint: srv.URL, UsePathStyle: true, AccessKeyID: "test", SecretAccessKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	oldScope := objectScope(t, "birth-a", store.ExactValueFormat)
	nextScope := objectScope(t, "birth-a-extra", store.ExactValueFormat)
	old, err := SelectScopedIR(c, oldScope)
	if err != nil {
		t.Fatal(err)
	}
	next, err := SelectScopedIR(c, nextScope)
	if err != nil {
		t.Fatal(err)
	}
	for _, logical := range []string{"run", "run-bundle-digest"} {
		if err := next.PutIRBlob(t.Context(), logical, []byte("successor")); err != nil {
			t.Fatal(err)
		}
		if err := old.PutIRBlob(t.Context(), logical, []byte("late-old")); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.DeleteRunIR(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRunIR(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	for _, logical := range []string{"run", "run-bundle-digest"} {
		key, err := IRBlobKeyForScope(nextScope, logical)
		if err != nil {
			t.Fatal(err)
		}
		got, err := next.GetIRBlob(t.Context(), key)
		if err != nil || string(got) != "successor" {
			t.Fatalf("%s successor: %s %v", logical, got, err)
		}
		if _, err := old.GetIRBlob(t.Context(), key); !errors.Is(err, store.ErrRunDataIdentityChanged) {
			t.Fatal("old view read successor", err)
		}
		if _, err := c.GetIRBlob(t.Context(), key); err == nil {
			t.Fatal("legacy view accepted scoped key")
		}
	}
	for _, method := range []string{"Close", "PutArtifact", "IRForScope", "RunObjectsForScope"} {
		if _, ok := reflect.TypeOf(next).MethodByName(method); ok {
			t.Fatal("scope escape", method)
		}
	}
}

func TestScopedIRRefusesForeignKeysBeforeIO(t *testing.T) {
	t.Parallel()
	scope := objectScope(t, "birth", store.ExactValueFormat)
	// No network client: every invalid operation must refuse before reaching I/O.
	v, err := (&S3Client{}).IRForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"other", "run-bundle-", "../run"} {
		if err := v.PutIRBlob(t.Context(), id, nil); err == nil {
			t.Fatal("accepted foreign IR", id)
		}
	}
	for _, key := range []string{"ir/run.json", "run-data-v1/f2/run/birth/ir/other.json", "run-data-v1/f2/run/birth/ir/../run.json"} {
		if _, err := v.GetIRBlob(t.Context(), key); err == nil {
			t.Fatal("accepted foreign key", key)
		}
	}
	if err := v.DeleteRunIR(t.Context(), "other"); !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Fatal(err)
	}
	if _, err := SelectScopedIR(struct{ Client }{}, scope); !errors.Is(err, store.ErrRunDataCapability) {
		t.Fatal(err)
	}
}
