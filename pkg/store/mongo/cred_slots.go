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
// cap, and the run documents the reconcile and the adoption read are loaded
// team-blind for the same reason.
//
// The reconcile is a read and a write on two collections, so it judges a
// holder from a run document that can move before its $pull lands: a run
// read paused, then flipped to queued and re-admitted by a resume on another
// replica. The pull therefore names the exact entry it judged ({run_id,
// at}), and a re-admission never leaves that entry in place — it moves the
// entry's instant forward in a conditional write (refreshCredSlotHolder),
// so the judged entry no longer exists when the pull lands. The filesystem
// twin needs neither: one mutex holds its whole decision. A release names
// its admission's entry the same way, on both twins: a failed resume's undo
// that lands after another resume re-admitted the run leaves that entry.

// credSlotHolderDoc is one admitted run and the instant of its latest
// admission — what store.CredSlotGrace is measured from when its document is
// missing, and the second half of the eviction's own CAS: a $pull names the
// entry it judged, and a run admitted again since holds an entry with a
// later instant, which the pull does not match.
type credSlotHolderDoc struct {
	RunID string    `bson:"run_id"`
	At    time.Time `bson:"at"`
}

type credSlotDoc struct {
	ID      string              `bson:"_id"`
	Holders []credSlotHolderDoc `bson:"holders"`
}

func (d credSlotDoc) holds(runID string) bool {
	_, ok := d.holder(runID)
	return ok
}

// holder returns runID's entry in the row, if it holds one.
func (d credSlotDoc) holder(runID string) (credSlotHolderDoc, bool) {
	for _, h := range d.Holders {
		if h.RunID == runID {
			return h, true
		}
	}
	return credSlotHolderDoc{}, false
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
	if err := s.adoptUnledgeredCredSlotHolders(ctx, fingerprint, runID, now); err != nil {
		return store.CredSlotAdmission{}, err
	}
	reconciled := false
	for round := 0; round < credSlotReserveRounds; round++ {
		var after credSlotDoc
		pushed := store.NextCredSlotInstant(time.Time{}, now)
		err := s.credSlots.FindOneAndUpdate(ctx,
			bson.M{
				"_id":            fingerprint,
				"holders.run_id": bson.M{"$ne": runID},
				// Fewer than max holders: the element at index max-1 is absent.
				fmt.Sprintf("holders.%d", max-1): bson.M{"$exists": false},
			},
			bson.M{
				"$push": bson.M{"holders": credSlotHolderDoc{RunID: runID, At: pushed}},
				"$set":  bson.M{"updated_at": now},
			},
			options.FindOneAndUpdate().SetReturnDocument(options.After),
		).Decode(&after)
		if err == nil {
			return store.CredSlotAdmission{Granted: true, Held: len(after.Holders), At: pushed}, nil
		}
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return store.CredSlotAdmission{}, fmt.Errorf("store/mongo: reserve cred slot %s for run %s: %w", fingerprint, runID, err)
		}
		// Not matched: the run already holds a slot, or the row is full.
		var current credSlotDoc
		if err := s.credSlots.FindOne(ctx, bson.M{"_id": fingerprint}).Decode(&current); err != nil {
			return store.CredSlotAdmission{}, fmt.Errorf("store/mongo: read cred slot row %s: %w", fingerprint, err)
		}
		if held, ok := current.holder(runID); ok && len(current.Holders) <= max {
			at, refreshed, err := s.refreshCredSlotHolder(ctx, fingerprint, held, max, now)
			if err != nil {
				return store.CredSlotAdmission{}, err
			}
			if refreshed {
				return store.CredSlotAdmission{Granted: true, Held: len(current.Holders), At: at}, nil
			}
			// The entry or the row moved since the read — pulled, admitted
			// again by another call, or grown past this cap: decide again
			// on a fresh read.
			continue
		}
		// Full of other runs — or, under a cap lowered below its holders,
		// over it with this run among them.
		if reconciled {
			return store.CredSlotAdmission{Granted: false, Held: len(current.Holders)}, nil
		}
		reconciled = true
		// Once, re-read the holders against their run documents: a slot
		// whose run is no longer spending is free, it just has not been
		// given back yet.
		evicted, err := s.evictStaleCredSlotHolders(ctx, fingerprint, current.Holders, now)
		if err != nil {
			return store.CredSlotAdmission{}, err
		}
		if evicted == 0 {
			return store.CredSlotAdmission{Granted: false, Held: len(current.Holders)}, nil
		}
	}
	return store.CredSlotAdmission{}, fmt.Errorf("store/mongo: reserve cred slot %s for run %s: the row moved under each of %d conditional writes — refused rather than spun on", fingerprint, runID, credSlotReserveRounds)
}

// credSlotReserveRounds bounds how many times one reservation decides again
// on a fresh read. A round is spent without an answer only when the row
// changed between the call's read and its conditional write — another
// writer's pull, push or re-admission — so a reservation that spends every
// round is contended far past any admission rate, or holds an entry its own
// write cannot match: an error either way, never a loop without an end.
const credSlotReserveRounds = 32

// refreshCredSlotHolder re-admits a holder in place: one conditional write
// that moves its entry's instant forward while the row still names that
// exact entry and holds no more than max runs — a cap lowered below its
// holders re-admits none of them. Reports the new instant and whether the
// write landed.
//
// The new instant is what an eviction already in flight cannot match: its
// $pull names the entry it judged, and the entry it would take is gone —
// and so is the one an earlier admission's undo names (ReleaseCredSlot).
// It is strictly later at the millisecond BSON keeps
// (store.NextCredSlotInstant), or the refreshed entry would be the judged
// one again.
func (s *Store) refreshCredSlotHolder(ctx context.Context, fingerprint string, held credSlotHolderDoc, max int, now time.Time) (time.Time, bool, error) {
	at := store.NextCredSlotInstant(held.At, now)
	judged := bson.M{"run_id": held.RunID, "at": held.At}
	res, err := s.credSlots.UpdateOne(ctx,
		bson.M{
			"_id":     fingerprint,
			"holders": bson.M{"$elemMatch": judged},
			// At most max holders: the element at index max is absent.
			fmt.Sprintf("holders.%d", max): bson.M{"$exists": false},
		},
		bson.M{"$set": bson.M{"holders.$[held].at": at, "updated_at": now}},
		options.UpdateOne().SetArrayFilters([]any{bson.M{"held.run_id": held.RunID, "held.at": held.At}}),
	)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store/mongo: re-admit run %s to cred slot %s: %w", held.RunID, fingerprint, err)
	}
	return at, res.MatchedCount == 1, nil
}

// adoptUnledgeredCredSlotHolders names in the row every run other than
// runID whose document spends the credential with no admission of its own
// still to come (credSlotAdoptableFilter) and that the row does not name —
// the adoption store.CredSlotStore's admission performs before it decides.
// Each push is conditional on the row still not naming the run, so two
// admissions adopting at once write one entry, and it carries no room check:
// the run already spends the key, and the row says so even past the cap.
func (s *Store) adoptUnledgeredCredSlotHolders(ctx context.Context, fingerprint, runID string, now time.Time) error {
	cur, err := s.runs.Find(ctx, credSlotAdoptableFilter(fingerprint, runID), options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return fmt.Errorf("store/mongo: reserve cred slot %s for run %s: list the runs that spend it: %w", fingerprint, runID, err)
	}
	var spenders []struct {
		ID string `bson:"_id"`
	}
	if err := cur.All(ctx, &spenders); err != nil {
		return fmt.Errorf("store/mongo: reserve cred slot %s for run %s: read the runs that spend it: %w", fingerprint, runID, err)
	}
	if len(spenders) == 0 {
		return nil
	}
	var row credSlotDoc
	if err := s.credSlots.FindOne(ctx, bson.M{"_id": fingerprint}).Decode(&row); err != nil {
		return fmt.Errorf("store/mongo: read cred slot row %s: %w", fingerprint, err)
	}
	for _, sp := range spenders {
		if row.holds(sp.ID) {
			continue
		}
		if _, err := s.credSlots.UpdateOne(ctx,
			bson.M{"_id": fingerprint, "holders.run_id": bson.M{"$ne": sp.ID}},
			bson.M{
				"$push": bson.M{"holders": credSlotHolderDoc{RunID: sp.ID, At: store.NextCredSlotInstant(time.Time{}, now)}},
				"$set":  bson.M{"updated_at": now},
			}); err != nil {
			return fmt.Errorf("store/mongo: adopt run %s into cred slot %s: %w", sp.ID, fingerprint, err)
		}
	}
	return nil
}

// credSlotAdoptableFilter selects the runs an admission adopts — the query
// form of store.Run.CredSlotAdoptable: the runs that spend the credential
// (spendsCredentialFilter) but a queued resume whose own admission is still
// to come (store.Run.AwaitsCredSlotAdmission).
func credSlotAdoptableFilter(fingerprint, excludeRunID string) bson.M {
	filter := spendsCredentialFilter(fingerprint, excludeRunID)
	filter["$nor"] = bson.A{bson.M{"status": string(store.RunStatusQueued), "cred_admission_pending": true}}
	return filter
}

// SettleCredSlotAdmission implements store.CredSlotStore: one conditional
// write that matches the run only while it is still queued for the flip at
// queuedAt, so a later flip's pending admission is never settled by an
// earlier one. Tenant-scoped like the flip it settles.
func (s *Store) SettleCredSlotAdmission(ctx context.Context, runID string, queuedAt time.Time) error {
	if runID == "" || queuedAt.IsZero() {
		return fmt.Errorf("store/mongo: settle the cred slot admission of run %q: run id and the flip's queued_at are required (queued_at=%v)", runID, queuedAt)
	}
	filter := notDeleted(withTenantFilter(ctx, bson.M{
		"_id":                    runID,
		"status":                 string(store.RunStatusQueued),
		"queued_at":              queuedAt,
		"cred_admission_pending": true,
	}))
	if _, err := s.runs.UpdateOne(ctx, filter, versionRunUpdate(bson.M{
		"$unset": bson.M{"cred_admission_pending": ""},
		"$set":   bson.M{"updated_at": time.Now().UTC()},
	})); err != nil {
		return fmt.Errorf("store/mongo: settle the cred slot admission of run %s: %w", runID, err)
	}
	return nil
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
		if s.beforeCredSlotEvict != nil {
			s.beforeCredSlotEvict(h.RunID)
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

// ReleaseCredSlot implements store.CredSlotStore: the $pull names the entry
// of runID admitted at `at`, so an entry written since by another admission
// of the run is not matched.
func (s *Store) ReleaseCredSlot(ctx context.Context, fingerprint, runID string, at time.Time) error {
	if fingerprint == "" || runID == "" {
		return fmt.Errorf("store/mongo: release cred slot: fingerprint and run id are required (fingerprint=%q run=%q)", fingerprint, runID)
	}
	if at.IsZero() {
		return fmt.Errorf("store/mongo: release cred slot %s of run %s: no admission instant — a release names the admission it undoes", fingerprint, runID)
	}
	if _, err := s.credSlots.UpdateOne(ctx, bson.M{"_id": fingerprint},
		bson.M{"$pull": bson.M{"holders": bson.M{"run_id": runID, "at": at}}, "$set": bson.M{"updated_at": time.Now().UTC()}}); err != nil {
		return fmt.Errorf("store/mongo: release cred slot %s of run %s: %w", fingerprint, runID, err)
	}
	return nil
}
