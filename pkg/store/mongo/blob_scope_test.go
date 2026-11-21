package mongo

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

type scopeRecordingBlob struct {
	*inMemoryBlob
	headers []store.RunDataHeader
}
type scopeArtifactFixture struct{ *inMemoryBlob }

func (b scopeArtifactFixture) DeleteRunArtifacts(ctx context.Context, id string) error {
	return b.DeleteRun(ctx, id)
}
func (b *scopeRecordingBlob) ArtifactsForScope(scope store.IncarnationDataScope) (blob.ArtifactObjects, error) {
	b.headers = append(b.headers, scope.Header())
	return scopeArtifactFixture{b.inMemoryBlob}, nil
}
func (b *scopeRecordingBlob) RunObjectsForScope(scope store.IncarnationDataScope) (blob.RunObjects, error) {
	b.headers = append(b.headers, scope.Header())
	return b.inMemoryBlob, nil
}

func TestBlobSelectionRetainsOriginalDataScope(t *testing.T) {
	t.Parallel()
	header := store.RunDataHeader{RunID: "run", BirthID: "original", Protocol: store.IncarnationRunData, Format: store.ExactValueFormat}
	binding, err := store.CaptureRunDataBinding(header)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := store.WithRunDataBinding(t.Context(), binding)
	if err != nil {
		t.Fatal(err)
	}
	backend := &scopeRecordingBlob{inMemoryBlob: newInMemoryBlob()}
	s := &Store{blob: backend}
	if err := s.putValueArtifact(ctx, header.Format, header.RunID, "n", 1, []byte("value")); err != nil {
		t.Fatal(err)
	}
	got, err := s.getValueArtifact(ctx, header.Format, header.RunID, "n", 1)
	if err != nil || !bytes.Equal(got, []byte("value")) {
		t.Fatal(string(got), err)
	}
	versions, err := s.listValueArtifacts(ctx, header.Format, header.RunID, "n")
	if err != nil || len(versions) != 1 || versions[0] != 1 {
		t.Fatal(versions, err)
	}
	if err := s.deleteValueArtifacts(ctx, header.Format, header.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.selectRunObjects(ctx, header.Format, header.RunID); err != nil {
		t.Fatal(err)
	}
	if len(backend.headers) != 5 {
		t.Fatalf("selection fell back to format namespaces: %v", backend.headers)
	}
	for _, got := range backend.headers {
		if got != header {
			t.Fatalf("selection rebound: %+v", got)
		}
	}
	for _, tc := range []struct {
		id     string
		format store.ValueFormat
	}{{"other", header.Format}, {header.RunID, store.LegacyValueFormat}} {
		if _, err := s.scopedArtifacts(ctx, tc.format, tc.id); !errors.Is(err, store.ErrRunDataIdentityChanged) {
			t.Fatal(err)
		}
		if _, err := s.selectRunObjects(ctx, tc.format, tc.id); !errors.Is(err, store.ErrRunDataIdentityChanged) {
			t.Fatal(err)
		}
	}
	if len(backend.headers) != 5 {
		t.Fatal("invalid scope reached backend")
	}
}

func TestIncarnationBlobSelectionNeverFallsBack(t *testing.T) {
	t.Parallel()
	b, err := store.CaptureRunDataBinding(store.RunDataHeader{RunID: "run", BirthID: "original", Protocol: store.IncarnationRunData, Format: store.LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := store.WithRunDataBinding(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{blob: newInMemoryBlob()}
	for _, call := range []func() error{
		func() error {
			return s.putValueArtifact(ctx, store.LegacyValueFormat, "run", "n", 1, []byte("must-not-write"))
		},
		func() error { _, err := s.getValueArtifact(ctx, store.LegacyValueFormat, "run", "n", 1); return err },
		func() error { _, err := s.listValueArtifacts(ctx, store.LegacyValueFormat, "run", "n"); return err },
		func() error { return s.deleteValueArtifacts(ctx, store.LegacyValueFormat, "run") },
		func() error { _, err := s.selectRunObjects(ctx, store.LegacyValueFormat, "run"); return err },
	} {
		if err := call(); !errors.Is(err, store.ErrRunDataCapability) {
			t.Fatalf("unsupported backend silently used legacy bytes: %v", err)
		}
	}
}
