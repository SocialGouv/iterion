package mongo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The cloud (Mongo) store satisfies the run-note seam so the studio run
// header shows operator notes identically in cloud and local mode.
var _ store.RunNoteStore = (*Store)(nil)

// noteSeqField advances notes independently of event and plan sequences in
// the selected counter: historical tenant/run or strict tenant/namespace.
// Each field starts at zero within its selected run-data domain.
const noteSeqField = "next_note_seq"

// runNoteDoc is one persisted operator note, keyed by sequence within its
// selected run-data namespace. It is the cloud twin of filesystem notes.
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

// AppendRunNote seeds from the committed tail and publishes its counter and
// document in the same parent-WRITE transaction. Retry keeps the original format.
func (s *Store) AppendRunNote(ctx context.Context, runID string, note store.RunNote) (store.RunNote, error) {
	if note.Timestamp.IsZero() {
		note.Timestamp = time.Now().UTC()
	}
	tenantID, _ := store.TenantFromContext(ctx)
	var out store.RunNote
	err := s.withRunMetadataWrite(ctx, runID, func(sc context.Context, parent *store.Run) error {
		out = store.RunNote{}
		n, err := s.streamNamespace(sc, runID, parent)
		if err != nil {
			return err
		}
		prev, hasPrev, err := s.lastStreamRunNote(sc, n)
		if err != nil {
			return err
		}
		floor := int64(0)
		if hasPrev {
			if int64(prev.Seq) == math.MaxInt64 {
				return fmt.Errorf("store/mongo: note sequence exhausted")
			}
			floor = int64(prev.Seq) + 1
		}
		if err = s.seedStreamSeqField(sc, n, noteSeqField, floor); err != nil {
			return err
		}
		seq, err := s.allocStreamSeqField(sc, n, noteSeqField)
		if err != nil {
			return err
		}
		next := note
		next.Seq = int(seq)
		doc := runNoteDoc{TenantID: tenantID, RunID: runID, Seq: next.Seq, Author: next.Author, Body: next.Body, Timestamp: next.Timestamp}
		raw, e := bson.Marshal(doc)
		if e != nil {
			return e
		}
		row := bson.M{}
		if e = bson.Unmarshal(raw, &row); e != nil {
			return e
		}
		n.stamp(row)
		if _, err = n.notes.InsertOne(sc, row); err != nil {
			return fmt.Errorf("store/mongo: insert note %s/%d: %w", runID, seq, err)
		}
		out = next
		return nil
	})
	if err != nil {
		return store.RunNote{}, err
	}
	return out, nil
}

// lastStreamRunNote returns the highest-seq note for the run (the seq source),
// or (_, false, nil) when the run has none.
func (s *Store) lastStreamRunNote(ctx context.Context, n streamDataNamespace) (store.RunNote, bool, error) {
	filter := n.filter(ctx, nil)
	opts := options.FindOne().SetSort(bson.D{{Key: "seq", Value: -1}})
	var doc runNoteDoc
	if err := n.notes.FindOne(ctx, filter, opts).Decode(&doc); err != nil {
		if err == mongo.ErrNoDocuments {
			return store.RunNote{}, false, nil
		}
		return store.RunNote{}, false, fmt.Errorf("store/mongo: load last run note %s: %w", n.runID, err)
	}
	return doc.toNote(), true, nil
}

// ReattributeRunNotes sweeps both physical note domains, including orphaned
// and retired births, without a parent lookup or tenant filter. A failed domain
// does not hide known modifications in the other domain; callers retry erasure.
func (s *Store) ReattributeRunNotes(ctx context.Context, fromAuthor, toAuthor string) (int64, error) {
	from := strings.TrimSpace(fromAuthor)
	if from == "" {
		return 0, fmt.Errorf("store/mongo: reattribute run notes: no author to match")
	}
	filter := bson.M{"author": bson.M{"$regex": `^\s*` + regexp.QuoteMeta(from) + `\s*$`, "$options": "i"}}
	var modified int64
	var errs []error
	for _, coll := range []*mongo.Collection{s.runNotes, s.db.Collection(colIncarnationNotes)} {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("store/mongo: reattribute %s: %w", coll.Name(), err))
			break
		}
		res, err := coll.UpdateMany(ctx, filter, bson.M{"$set": bson.M{"author": toAuthor}})
		if res != nil && res.Acknowledged {
			modified += res.ModifiedCount
		}
		if res != nil && !res.Acknowledged {
			errs = append(errs, fmt.Errorf("store/mongo: reattribute %s: unacknowledged write outcome", coll.Name()))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("store/mongo: reattribute %s: %w", coll.Name(), err))
		}
	}
	return modified, errors.Join(errs...)
}

// ListRunNotes reads opaque notes and metadata within one selected snapshot.
// Historical absence/tombstones keep their opaque-reader behavior.
func (s *Store) ListRunNotes(ctx context.Context, runID string) ([]store.RunNote, error) {
	var out []store.RunNote
	err := s.withOpaqueStreamRead(ctx, runID, func(sc context.Context, n streamDataNamespace, _ *store.Run) error {
		out = nil
		cur, err := n.notes.Find(sc, n.filter(sc, nil), options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}))
		if err != nil {
			return fmt.Errorf("store/mongo: list run notes %s: %w", runID, err)
		}
		defer cur.Close(sc)
		for cur.Next(sc) {
			var doc runNoteDoc
			if err := cur.Decode(&doc); err != nil {
				return fmt.Errorf("store/mongo: decode run note %s: %w", runID, err)
			}
			out = append(out, doc.toNote())
		}
		return cur.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
