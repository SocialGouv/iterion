package mongo

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

func (s *Store) putValueArtifact(ctx context.Context, f store.ValueFormat, r, n string, v int, p []byte) error {
	objects, err := s.scopedArtifacts(ctx, f, r)
	if err != nil {
		return err
	}
	if objects != nil {
		return objects.PutArtifact(ctx, r, n, v, p)
	}
	if f == store.LegacyValueFormat {
		return s.blob.PutArtifact(ctx, r, n, v, p)
	}
	cap, ok := s.blob.(blob.ValueFormatArtifacts)
	if !ok {
		return store.ErrValueFormatCapability
	}
	return cap.PutArtifactForFormat(ctx, int(f), r, n, v, p)
}
func (s *Store) getValueArtifact(ctx context.Context, f store.ValueFormat, r, n string, v int) ([]byte, error) {
	objects, err := s.scopedArtifacts(ctx, f, r)
	if err != nil {
		return nil, err
	}
	if objects != nil {
		return objects.GetArtifact(ctx, r, n, v)
	}
	if f == store.LegacyValueFormat {
		return s.blob.GetArtifact(ctx, r, n, v)
	}
	cap, ok := s.blob.(blob.ValueFormatArtifacts)
	if !ok {
		return nil, store.ErrValueFormatCapability
	}
	return cap.GetArtifactForFormat(ctx, int(f), r, n, v)
}
func (s *Store) listValueArtifacts(ctx context.Context, f store.ValueFormat, r, n string) ([]int, error) {
	objects, err := s.scopedArtifacts(ctx, f, r)
	if err != nil {
		return nil, err
	}
	if objects != nil {
		return objects.ListArtifactVersions(ctx, r, n)
	}
	if f == store.LegacyValueFormat {
		return s.blob.ListArtifactVersions(ctx, r, n)
	}
	cap, ok := s.blob.(blob.ValueFormatArtifacts)
	if !ok {
		return nil, store.ErrValueFormatCapability
	}
	return cap.ListArtifactVersionsForFormat(ctx, int(f), r, n)
}
func (s *Store) deleteValueArtifacts(ctx context.Context, f store.ValueFormat, r string) error {
	objects, err := s.scopedArtifacts(ctx, f, r)
	if err != nil {
		return err
	}
	if objects != nil {
		return objects.DeleteRunArtifacts(ctx, r)
	}
	if f == store.LegacyValueFormat {
		return s.blob.DeleteRun(ctx, r)
	}
	cap, ok := s.blob.(blob.ValueFormatArtifacts)
	if !ok {
		return store.ErrValueFormatCapability
	}
	return cap.DeleteRunArtifactsForFormat(ctx, int(f), r)
}

// The caller validates the durable header before external I/O. Selection then
// uses only the retained original binding, never a fresh metadata lookup.
func retainedBlobScope(ctx context.Context, id string, format store.ValueFormat) (store.IncarnationDataScope, bool, error) {
	if ctx == nil {
		return store.IncarnationDataScope{}, false, store.ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return store.IncarnationDataScope{}, false, err
	}
	binding, ok := store.RunDataBindingFrom(ctx)
	if !ok || binding.Header().Protocol == store.HistoricalRunData {
		return store.IncarnationDataScope{}, false, nil
	}
	header := binding.Header()
	if header.RunID != id || header.Format != format {
		return store.IncarnationDataScope{}, false, store.ErrRunDataIdentityChanged
	}
	scope, err := store.ResolveIncarnationDataScope(ctx, header)
	return scope, true, err
}

func (s *Store) scopedArtifacts(ctx context.Context, format store.ValueFormat, id string) (blob.ArtifactObjects, error) {
	scope, scoped, err := retainedBlobScope(ctx, id, format)
	if err != nil || !scoped {
		return nil, err
	}
	return blob.SelectScopedArtifacts(s.blob, scope)
}

func (s *Store) selectRunObjects(ctx context.Context, format store.ValueFormat, id string) (blob.RunObjects, error) {
	scope, scoped, err := retainedBlobScope(ctx, id, format)
	if err != nil {
		return nil, err
	}
	if scoped {
		return blob.SelectScopedRunObjects(s.blob, scope)
	}
	return blob.SelectRunObjects(s.blob, int(format))
}
