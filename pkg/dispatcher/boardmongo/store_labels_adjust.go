package boardmongo

import (
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// AdjustLabels is the relative label write (see native.BoardStore) as ONE
// pipeline update: the delta is computed by the server against the
// document as it is, so two replicas' adds cannot lose each other and a
// one-shot label the trigger spine consumed in between stays consumed.
// The pipeline mirrors native.AdjustLabelList exactly (existing order
// kept, missing adds appended in order, removes applied last), and the
// returned issue is described from the pre-write document with the same
// Go function — one definition of the delta on both twins.
func (s *Store) AdjustLabels(id string, add, remove []string) (*native.Issue, bool, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	add, remove = native.CleanLabels(add), native.CleanLabels(remove)
	if len(add) == 0 && len(remove) == 0 {
		iss, err := s.get(ctx, id)
		return iss, false, err
	}
	// $literal: pipeline $set evaluates values as EXPRESSIONS, and labels
	// are operator/bot text — a "$"-leading one would be read as a field
	// path (the AddComment lesson).
	cur := bson.M{"$ifNull": bson.A{"$issue.labels", bson.A{}}}
	next := bson.M{"$filter": bson.M{
		"input": bson.M{"$concatArrays": bson.A{
			cur,
			bson.M{"$filter": bson.M{
				"input": bson.M{"$literal": toBSONArray(add)},
				"as":    "candidate",
				"cond":  bson.M{"$not": bson.A{bson.M{"$in": bson.A{"$$candidate", cur}}}},
			}},
		}},
		"as":   "label",
		"cond": bson.M{"$not": bson.A{bson.M{"$in": bson.A{"$$label", bson.M{"$literal": toBSONArray(remove)}}}}},
	}}
	now := time.Now().UTC()
	res := s.issues.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "tenant_id": s.tenant},
		setPipeline(bson.M{
			"issue.labels": next,
			// UpdatedAt moves only when the labels do — the sweeps and the
			// board_events tail order on it, and a no-op must not churn it.
			"issue.updatedat": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{next, cur}}, "$issue.updatedat", now}},
		}),
		options.FindOneAndUpdate().SetReturnDocument(options.Before))
	if res.Err() != nil {
		if !isNoDocuments(res.Err()) {
			return nil, false, fmt.Errorf("boardmongo: adjust labels: %w", res.Err())
		}
		_, gerr := s.get(ctx, id)
		if gerr != nil {
			return nil, false, gerr
		}
		return nil, false, fmt.Errorf("boardmongo: adjust labels: %s vanished mid-write", id)
	}
	var before issueDoc
	if err := res.Decode(&before); err != nil {
		return nil, false, fmt.Errorf("boardmongo: adjust labels decode: %w", err)
	}
	out, added, removed := native.AdjustLabelList(before.Issue.Labels, add, remove)
	after := before.Issue
	after.Labels = out
	if len(added) == 0 && len(removed) == 0 {
		return &after, false, nil
	}
	after.UpdatedAt = now
	if err := s.emit(native.Event{Type: native.EvtIssueUpdated, IssueID: id,
		Payload: native.LabelsAdjustedPayload(added, removed)}); err != nil {
		return nil, false, err
	}
	return &after, true, nil
}

func toBSONArray(ss []string) bson.A {
	out := make(bson.A, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}

// setPipeline wraps a $set document as an update pipeline (the form that
// makes expressions legal), like leaseStampPipeline.
func setPipeline(set bson.M) mongo.Pipeline {
	return mongo.Pipeline{{{Key: "$set", Value: set}}}
}
