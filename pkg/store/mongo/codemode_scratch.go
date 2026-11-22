package mongo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The field is deliberately absent from store.Run. SaveRun preserves it
// from the live document without decoding it, and the version bump protects
// older savers that carry the field via unknown-field preservation. The
// whole bank is BSON binary: neither its keys nor its JSON enter BSON syntax.
const codemodeScratchField = "codemode_scratch"

var _ store.CodemodeScratchStore = (*Store)(nil)

func (s *Store) readCodemodeScratch(ctx context.Context, runID string) ([]byte, *store.Run, error) {
	// A secondary can return a stale missing/deleted/over-quota result
	// without ever entering the CAS retry loop.
	runs := s.runs.Clone(options.Collection().SetReadPreference(readpref.Primary()))
	projection := runFormatMetadataProjection()
	projection[codemodeScratchField] = 1
	projection["v"] = 1
	parent, raw, err := readRunMetadataProjection(ctx, runs, runID, false, false, projection)
	if err != nil {
		return nil, nil, err
	}
	if parent == nil {
		return nil, nil, store.ErrRunNotFound
	}
	if parent.SchemaVersion > SchemaVersion {
		return nil, nil, store.ErrRunMetadataUndecodable
	}
	if parent.DeletedAt != nil {
		return nil, nil, store.ErrRunDeleted
	}
	v := raw.Lookup(codemodeScratchField)
	if v.Type == 0 {
		return nil, parent, nil
	}
	subtype, data, ok := v.BinaryOK()
	if !ok || subtype != 0 || len(data) == 0 {
		return nil, nil, errors.New("store/mongo: invalid persisted codemode scratch bank")
	}
	return data, parent, nil
}

func (s *Store) PutCodemodeScratch(ctx context.Context, runID, key string, value json.RawMessage, limits store.CodemodeScratchLimits) error {
	put, err := store.PrepareCodemodeScratchPut(key, value, limits)
	if err != nil {
		return err
	}
	if err := store.SanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	for {
		data, parent, err := s.readCodemodeScratch(ctx, runID)
		if err != nil {
			return err
		}
		next, err := put.Apply(data)
		if err != nil {
			return err
		}
		observation, err := store.CaptureRunObservation(store.RunDataHeader{
			RunID: parent.ID, Format: store.ValueFormat(parent.FormatVersion), Protocol: parent.DataProtocol, BirthID: parent.BirthID,
		}, parent.TenantID, true)
		if err != nil {
			return err
		}
		ctx, err = store.BindRunObservation(ctx, observation)
		if err != nil {
			return err
		}
		format, err := store.CaptureRunValueFormat(parent)
		if err != nil {
			return err
		}
		guard := runMetadataFormat{original: format, bound: true, birth: parent.BirthID}
		filter := guard.filter(ctx, correctionVersionFilter(parent.CASVersion))
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := s.runs.UpdateOne(ctx, filter, versionRunUpdate(bson.M{"$set": bson.M{codemodeScratchField: next}}))
		if err != nil {
			return fmt.Errorf("store/mongo: persist codemode scratch bank: %w", err)
		}
		if result.MatchedCount != 0 {
			return nil
		}
		// A concurrent update or tombstone won; recheck before any retry.
	}
}

func (s *Store) LoadCodemodeScratch(ctx context.Context, runID, key string) (json.RawMessage, bool, error) {
	if err := store.ValidateCodemodeScratchKey(key); err != nil {
		return nil, false, err
	}
	if err := store.SanitizePathComponent("run ID", runID); err != nil {
		return nil, false, err
	}
	data, _, err := s.readCodemodeScratch(ctx, runID)
	if err != nil {
		return nil, false, err
	}
	return store.LoadCodemodeScratchValue(data, key)
}
