package mongo

import (
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"time"
)

var _ store.RunValueFormatCreator = (*Store)(nil)

func (s *Store) CreateRunWithValueFormat(ctx context.Context, spec store.RunCreateSpec) (*store.Run, error) {
	spec, err := spec.Normalize(ctx)
	if err != nil {
		return nil, err
	}
	if spec.Format == store.ExactValueFormat {
		if _, ok := s.blob.(blob.ValueFormatArtifacts); !ok {
			return nil, store.ErrValueFormatCapability
		}
	}
	if spec.Status == "" {
		spec.Status = store.RunStatusQueued
	}
	var parent *store.Run
	if spec.ParentRunID != "" {
		parent, err = s.LoadRun(ctx, spec.ParentRunID)
		if err != nil {
			return nil, fmt.Errorf("%w: child run %s names parent %q: %w", store.ErrChildParentUnreadable, spec.ID, spec.ParentRunID, err)
		}
	}
	now := time.Now().UTC()
	r := &store.Run{ID: spec.ID, WorkflowName: spec.WorkflowName, ParentRunID: spec.ParentRunID, FilePath: spec.FilePath, BotID: spec.BotID, Inputs: spec.Inputs, Status: spec.Status, FormatVersion: int(spec.Format), DataProtocol: spec.DataProtocol, CreatedAt: now, UpdatedAt: now, LaunchEnv: store.CaptureLaunchEnv(), IterionVersion: appinfo.FullVersion(), BirthID: store.NewRunBirthID(), ExecutionEpoch: store.NewExecutionEpoch(), CASVersion: 1, SchemaVersion: SchemaVersion}
	store.ChildTrustFromParent(r, parent)
	if spec.Status == store.RunStatusQueued {
		r.QueuedAt = &now
	}
	stampTenant(ctx, r)
	raw, err := store.EncodeValueRecordBSON(ctx, spec.Format, store.ExactRun, r)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	var doc bson.M
	if err = bson.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if err = setInputPRURLProjection(ctx, doc, store.ValueFormat(r.FormatVersion)); err != nil {
		return nil, fmt.Errorf("store/mongo: project run PR URL: %w", err)
	}
	session, err := s.client.StartSession()
	if err != nil {
		return nil, err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc context.Context) (any, error) {
		if _, e := s.runs.InsertOne(sc, doc); e != nil {
			return nil, e
		}
		if spec.Format == store.ExactValueFormat {
			for _, collection := range []*mongo.Collection{s.events, s.interactions} {
				n, e := collection.CountDocuments(sc, withTenantFilter(sc, bson.M{"run_id": r.ID}))
				if e != nil {
					return nil, e
				}
				if n != 0 {
					return nil, fmt.Errorf("%w: preexisting %s payload", store.ErrInvalidValueFormat, collection.Name())
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil, fmt.Errorf("store/mongo: run %s already exists", r.ID)
		}
		return nil, fmt.Errorf("store/mongo: insert run: %w", err)
	}
	return r, nil
}
func (s *Store) loadRunValueRecord(ctx context.Context, id string, opts ...options.Lister[options.FindOneOptions]) (store.Run, error) {
	result := s.runs.FindOne(ctx, withTenantFilter(ctx, bson.M{"_id": id}), opts...)
	raw, err := result.Raw()
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return store.Run{}, fmt.Errorf("store/mongo: run %s: %w", id, store.ErrRunNotFound)
		}
		return store.Run{}, err
	}
	return decodeRunValueRecord(ctx, id, raw, result.Decode)
}

// decodeRunValueRecord is the one decode every run read crosses, whether the
// document came from a single FindOne or from a listing cursor: value format,
// data header, storage and binding checks, the strict tenant rule and its
// observation (checkRunDocumentTenant), then the legacy registry
// (decodeLegacy, the reader's own Decode, so the client's registry applies) or
// the exact codec. A listing that decoded more laxly than LoadRun would be a
// second door into the same documents.
func decodeRunValueRecord(ctx context.Context, id string, raw bson.Raw, decodeLegacy func(any) error) (store.Run, error) {
	// The document is already read: a failure to decode it answers the same
	// on every re-read, so it is reported as ErrRunMetadataUndecodable.
	f, err := store.RunValueFormatFromBSON(ctx, raw)
	if err != nil {
		return store.Run{}, store.RunMetadataUndecodable(id, err)
	}
	h, err := store.RunDataHeaderFromBSON(ctx, raw)
	if err != nil {
		return store.Run{}, store.RunMetadataUndecodable(id, err)
	}
	if err := store.RequireRunDataStorage(h.Protocol); err != nil {
		return store.Run{}, err
	}
	if h.RunID != id {
		return store.Run{}, store.ErrRunDataIdentityChanged
	}
	if err := store.CheckRunDataBinding(ctx, h); err != nil {
		return store.Run{}, err
	}
	if err := checkRunDocumentTenant(ctx, raw); err != nil {
		return store.Run{}, err
	}

	var r store.Run
	if f == store.LegacyValueFormat {
		err = decodeLegacy(&r)
	} else {
		var codecErr error
		codec, codecErr := store.ExactRecordCodec(store.ExactRun)
		if codecErr != nil {
			return r, codecErr
		}
		err = codec.DecodeBSON(ctx, raw, &r)
	}
	return r, store.RunMetadataUndecodable(id, err)
}
func valueFormatPredicate(f store.ValueFormat) bson.M {
	return canonicalValueFormatPredicate(f)
}
