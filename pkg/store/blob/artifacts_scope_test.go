package blob

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestScopedArtifactsLateWriterAndCleanup(t *testing.T) {
	t.Parallel()
	_, srv := newFakeS3(t, "b")
	c, err := NewS3(t.Context(), Config{Bucket: "b", Region: "us-east-1", Endpoint: srv.URL, UsePathStyle: true, AccessKeyID: "test", SecretAccessKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := SelectScopedArtifacts(c, objectScope(t, "birth-a", store.ExactValueFormat))
	if err != nil {
		t.Fatal(err)
	}
	next, err := SelectScopedArtifacts(c, objectScope(t, "birth-a-extra", store.ExactValueFormat))
	if err != nil {
		t.Fatal(err)
	}
	otherFormat, err := SelectScopedArtifacts(c, objectScope(t, "birth-a-extra", store.LegacyValueFormat))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, version := range []int{1, 7} {
		if err := next.PutArtifact(ctx, "run", "node", version, []byte("successor")); err != nil {
			t.Fatal(err)
		}
	}
	if err := otherFormat.PutArtifact(ctx, "run", "node", 1, []byte("other-format")); err != nil {
		t.Fatal(err)
	}
	// A writer keeps its original view while the public ID is replaced.
	if err := old.PutArtifact(ctx, "run", "node", 1, []byte("late-old")); err != nil {
		t.Fatal(err)
	}
	got, err := next.GetArtifact(ctx, "run", "node", 1)
	if err != nil || !bytes.Equal(got, []byte("successor")) {
		t.Fatalf("late write reached successor: %s %v", got, err)
	}
	if err := old.DeleteRunArtifacts(ctx, "run"); err != nil {
		t.Fatal(err)
	}
	for _, format := range []int{1, 2} {
		if err := c.DeleteRunArtifactsForFormat(ctx, format, "run"); err != nil {
			t.Fatal(err)
		}
	}
	versions, err := next.ListArtifactVersions(ctx, "run", "node")
	if err != nil || !reflect.DeepEqual(versions, []int{1, 7}) {
		t.Fatal(versions, err)
	}
	got, err = otherFormat.GetArtifact(ctx, "run", "node", 1)
	if err != nil || !bytes.Equal(got, []byte("other-format")) {
		t.Fatalf("format scope crossed: %s %v", got, err)
	}
	if err := next.DeleteRunArtifacts(ctx, "run"); err != nil {
		t.Fatal(err)
	}
	if _, err := next.GetArtifact(ctx, "run", "node", 1); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("own cleanup did not delete: %v", err)
	}
	if _, err := otherFormat.GetArtifact(ctx, "run", "node", 1); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"Close", "Ping", "PutUpload", "PutIRBlob", "ArtifactsForScope", "RunObjectsForScope"} {
		if _, ok := reflect.TypeOf(next).MethodByName(method); ok {
			t.Fatalf("view exposes %s", method)
		}
	}
}

func TestScopedArtifactsRefuseRebindingAndMissingCapability(t *testing.T) {
	t.Parallel()
	scope := objectScope(t, "birth", store.ExactValueFormat)
	if _, err := SelectScopedArtifacts(struct{ Client }{&S3Client{}}, scope); !errors.Is(err, store.ErrRunDataCapability) {
		t.Fatal(err)
	}
	if _, err := SelectScopedArtifacts(&S3Client{}, store.IncarnationDataScope{}); !errors.Is(err, store.ErrRunDataBindingRequired) {
		t.Fatal(err)
	}
	view, err := SelectScopedArtifacts(&S3Client{}, scope)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for i, call := range []func() error{
		func() error { return view.PutArtifact(ctx, "other", "node", 1, nil) },
		func() error { _, err := view.GetArtifact(ctx, "other", "node", 1); return err },
		func() error { _, err := view.ListArtifactVersions(ctx, "other", "node"); return err },
		func() error { return view.DeleteRunArtifacts(ctx, "other") },
	} {
		if err := call(); !errors.Is(err, store.ErrRunDataIdentityChanged) {
			t.Fatalf("operation %d: %v", i, err)
		}
	}
}
