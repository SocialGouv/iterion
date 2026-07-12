package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The cloud (Mongo) store satisfies the run-note seam so the studio run
// header shows operator notes identically in cloud and local mode.
var _ store.RunNoteStore = (*Store)(nil)

// appendNoteMaxRetries caps the seq-collision retry loop for notes.
// Mirrors appendPlanMaxRetries: two operators adding a note to the same
// run concurrently can race on the next per-run sequence, and the
// (run_id, seq) unique index is the safety net that forces a re-read +
// realloc rather than a silent collapse.
const appendNoteMaxRetries = 10

// noteSeqField is the run_seq counter field backing run-note seqs — a
// distinct field on the same {tenant_id, run_id} counter document so
// note seqs advance independently of event/plan seqs (all start at 0).
const noteSeqField = "next_note_seq"

// runNoteDoc is one persisted operator note, one document per (run_id,
// seq). The cloud twin of the filesystem store's runs/<id>/notes/<NNNN>.json.
// Tenant-stamped like run_gitmeta/run_plans so a tenant only ever reads
// its own notes.
type runNoteDoc struct {
	TenantID  string    `bson:"tenant_id,omitempty"`
	RunID     string    `bson:"run_id"`
	Seq       int       `bson:"seq"`
	Author    string    `bson:"author,omitempty"`
	Body      string    `bson:"body"`
	Timestamp time.Time `bson:"ts"`
}

func (d runNoteDoc) toNote() store.RunNote {
	return store.RunNote{
		Seq:       d.Seq,
		Author:    d.Author,
		Body:      d.Body,
		Timestamp: d.Timestamp,
	}
}

func (s *Store) allocNoteSeq(ctx context.Context, runID string) (int64, error) {
	return s.allocSeqField(ctx, runID, noteSeqField)
}

// AppendRunNote implements store.RunNoteStore over the run_notes
// collection. It assigns note.Seq itself from a per-run monotonic counter
// (allocNoteSeq, mirroring AppendPlanSnapshot) and stamps note.Timestamp
// when zero. The (run_id, seq) unique index is the safety net: a
// duplicate-key error (a server-runner handoff that desynced the counter,
// or a concurrent racer) re-seeds the counter to tail+1 and reallocs
// rather than surfacing a hard failure.
func (s *Store) AppendRunNote(ctx context.Context, runID string, note store.RunNote) (store.RunNote, error) {
	if note.Timestamp.IsZero() {
		note.Timestamp = time.Now().UTC()
	}
	tenantID, _ := store.TenantFromContext(ctx)

	var lastErr error
	for attempt := 0; attempt < appendNoteMaxRetries; attempt++ {
		if attempt > 0 {
			if err := backoffOrCancel(ctx, attempt); err != nil {
				return store.RunNote{}, err
			}
		}

		seq64, aerr := s.allocNoteSeq(ctx, runID)
		if aerr != nil {
			return store.RunNote{}, aerr
		}
		seq := int(seq64)
		note.Seq = seq
		doc := runNoteDoc{
			TenantID:  tenantID,
			RunID:     runID,
			Seq:       seq,
			Author:    note.Author,
			Body:      note.Body,
			Timestamp: note.Timestamp,
		}
		if _, err := s.runNotes.InsertOne(ctx, doc); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				lastErr = err
				if tail, hasTail, terr := s.lastRunNote(ctx, runID); terr == nil && hasTail && tail.Seq >= seq {
					if serr := s.seedSeqField(ctx, runID, noteSeqField, int64(tail.Seq)+1); serr != nil {
						return store.RunNote{}, serr
					}
				}
				continue
			}
			return store.RunNote{}, fmt.Errorf("store/mongo: insert run note %s/%d: %w", runID, seq, err)
		}
		return note, nil
	}
	return store.RunNote{}, fmt.Errorf("store/mongo: race on note seq for run %s after %d attempts: %w", runID, appendNoteMaxRetries, lastErr)
}

// lastRunNote returns the highest-seq note for the run (the seq source),
// or (_, false, nil) when the run has none.
func (s *Store) lastRunNote(ctx context.Context, runID string) (store.RunNote, bool, error) {
	filter := withTenantFilter(ctx, bson.M{"run_id": runID})
	opts := options.FindOne().SetSort(bson.D{{Key: "seq", Value: -1}})
	var doc runNoteDoc
	if err := s.runNotes.FindOne(ctx, filter, opts).Decode(&doc); err != nil {
		if err == mongo.ErrNoDocuments {
			return store.RunNote{}, false, nil
		}
		return store.RunNote{}, false, fmt.Errorf("store/mongo: load last run note %s: %w", runID, err)
	}
	return doc.toNote(), true, nil
}

// ListRunNotes implements store.RunNoteStore: every persisted note for
// the run in ascending Seq (chronological) order. A run with no notes
// yields (nil, nil).
func (s *Store) ListRunNotes(ctx context.Context, runID string) ([]store.RunNote, error) {
	filter := withTenantFilter(ctx, bson.M{"run_id": runID})
	opts := options.Find().SetSort(bson.D{{Key: "seq", Value: 1}})
	cur, err := s.runNotes.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list run notes %s: %w", runID, err)
	}
	defer cur.Close(ctx)

	var out []store.RunNote
	for cur.Next(ctx) {
		var doc runNoteDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("store/mongo: decode run note %s: %w", runID, err)
		}
		out = append(out, doc.toNote())
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("store/mongo: iterate run notes %s: %w", runID, err)
	}
	return out, nil
}
