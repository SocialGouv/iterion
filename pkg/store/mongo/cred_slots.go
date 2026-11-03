package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Mongo half of store.CredSlotStore: the per-fingerprint ledger behind a
// credential's HARD concurrency cap.
//
// One row per fingerprint, `holders` the runs admitted under it. The
// admission is ONE conditional update — "add this run while the row names
// fewer than max holders and none of them is this run" — which Mongo matches
// and applies atomically on a single document, so two publisher replicas
// racing for the last slot cannot both match. The room check is the array-
// index idiom `holders.<max-1>` absent: it needs no aggregation expression in
// an update filter and reads the same on every server this store supports.
//
// Not tenant-scoped, deliberately: a platform key serves every tenant on one
// cap, and the run documents the reconcile reads are loaded team-blind for
// the same reason.

// credSlotHolderDoc is one admitted run and the instant it was admitted —
// what store.CredSlotGrace is measured from when its document is missing,
// and the second half of the eviction's own CAS (a $pull names the entry it
// judged, so a run that reserved again in between keeps its new entry).
type credSlotHolderDoc struct {
	RunID string    `bson:"run_id"`
	At    time.Time `bson:"at"`
}

type credSlotDoc struct {
	ID      string              `bson:"_id"`
	Holders []credSlotHolderDoc `bson:"holders"`
}

func (d credSlotDoc) holds(runID string) bool {
	for _, h := range d.Holders {
		if h.RunID == runID {
			return true
		}
	}
	return false
}

var _ store.CredSlotStore = (*Store)(nil)

// ReserveCredSlot implements store.CredSlotStore.
func (s *Store) ReserveCredSlot(ctx context.Context, fingerprint, runID string, max int) (store.CredSlotAdmission, error) {
	if fingerprint == "" || runID == "" {
		return store.CredSlotAdmission{}, fmt.Errorf("store/mongo: reserve cred slot: fingerprint and run id are required (fingerprint=%q run=%q)", fingerprint, runID)
	}
	if max <= 0 {
		return store.CredSlotAdmission{}, fmt.Errorf("store/mongo: reserve cred slot for run %s: max=%d — an uncapped key reserves nothing", runID, max)
	}
	now := time.Now().UTC()
	// The row exists before the conditional add. The add's filter carries
	// the room check, and an upsert on such a filter would create a SECOND
	// row for the fingerprint whenever the first is full.
	if _, err := s.credSlots.UpdateOne(ctx, bson.M{"_id": fingerprint},
		bson.M{"$setOnInsert": bson.M{"holders": bson.A{}, "created_at": now}},
		options.UpdateOne().SetUpsert(true)); err != nil {
		return store.CredSlotAdmission{}, fmt.Errorf("store/mongo: cred slot row %s: %w", fingerprint, err)
	}
	for attempt := 0; ; attempt++ {
		var after credSlotDoc
		err := s.credSlots.FindOneAndUpdate(ctx,
			bson.M{
				"_id":            fingerprint,
				"holders.run_id": bson.M{"$ne": runID},
				// Fewer than max holders: the element at index max-1 is absent.
				fmt.Sprintf("holders.%d", max-1): bson.M{"$exists": false},
			},
			bson.M{
				"$push": bson.M{"holders": credSlotHolderDoc{RunID: runID, At: now}},
				"$set":  bson.M{"updated_at": now},
			},
			options.FindOneAndUpdate().SetReturnDocument(options.After),
		).Decode(&after)
		if err == nil {
			return store.CredSlotAdmission{Granted: true, Held: len(after.Holders)}, nil
		}
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return store.CredSlotAdmission{}, fmt.Errorf("store/mongo: reserve cred slot %s for run %s: %w", fingerprint, runID, err)
		}
		// Not matched: the run already holds a slot, or the row is full.
		var current credSlotDoc
		if err := s.credSlots.FindOne(ctx, bson.M{"_id": fingerprint}).Decode(&current); err != nil {
			return store.CredSlotAdmission{}, fmt.Errorf("store/mongo: read cred slot row %s: %w", fingerprint, err)
		}
		if current.holds(runID) {
			return store.CredSlotAdmission{Granted: true, Held: len(current.Holders)}, nil
		}
		if attempt > 0 {
			return store.CredSlotAdmission{Granted: false, Held: len(current.Holders)}, nil
		}
		// Full. Once, re-read the holders against their run documents: a
		// slot whose run is no longer spending is free, it just has not
		// been given back yet.
		evicted, err := s.evictStaleCredSlotHolders(ctx, fingerprint, current.Holders, now)
		if err != nil {
			return store.CredSlotAdmission{}, err
		}
		if evicted == 0 {
			return store.CredSlotAdmission{Granted: false, Held: len(current.Holders)}, nil
		}
	}
}

// evictStaleCredSlotHolders pulls every holder whose run no longer spends
// the credential (store.CredSlotHolderStale), each by the exact entry it
// judged, and reports how many it pulled.
func (s *Store) evictStaleCredSlotHolders(ctx context.Context, fingerprint string, holders []credSlotHolderDoc, now time.Time) (int, error) {
	// Team-blind: a platform key's holders live in every tenant, and the
	// caller's own tenant would scope the read back down to its runs.
	blind := store.TeamBlind(ctx)
	evicted := 0
	for _, h := range holders {
		run, err := s.LoadRun(blind, h.RunID)
		if !store.CredSlotHolderStale(run, err, h.At, now, fingerprint) {
			continue
		}
		res, perr := s.credSlots.UpdateOne(ctx, bson.M{"_id": fingerprint},
			bson.M{"$pull": bson.M{"holders": bson.M{"run_id": h.RunID, "at": h.At}}, "$set": bson.M{"updated_at": now}})
		if perr != nil {
			return evicted, fmt.Errorf("store/mongo: evict stale holder %s of cred slot %s: %w", h.RunID, fingerprint, perr)
		}
		evicted += int(res.ModifiedCount)
	}
	return evicted, nil
}

// ReleaseCredSlot implements store.CredSlotStore.
func (s *Store) ReleaseCredSlot(ctx context.Context, fingerprint, runID string) error {
	if fingerprint == "" || runID == "" {
		return fmt.Errorf("store/mongo: release cred slot: fingerprint and run id are required (fingerprint=%q run=%q)", fingerprint, runID)
	}
	if _, err := s.credSlots.UpdateOne(ctx, bson.M{"_id": fingerprint},
		bson.M{"$pull": bson.M{"holders": bson.M{"run_id": runID}}, "$set": bson.M{"updated_at": time.Now().UTC()}}); err != nil {
		return fmt.Errorf("store/mongo: release cred slot %s of run %s: %w", fingerprint, runID, err)
	}
	return nil
}
