package auth

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// passCapture records the update command a revocation pass sends to the
// sessions collection, so the test can ask Mongo to explain THAT command.
type passCapture struct {
	mu      sync.Mutex
	command bson.Raw
}

func (c *passCapture) monitor() *event.CommandMonitor {
	return &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName != "update" {
			return
		}
		if coll, ok := e.Command.Lookup("update").StringValueOK(); !ok || coll != SessionsCollectionName {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.command = append(bson.Raw(nil), e.Command...)
	}}
}

// explainLast asks Mongo for the execution statistics of the last captured
// pass, and answers how many session documents it examined.
func (c *passCapture) explainLast(t *testing.T, st *MongoSessionStore) int64 {
	t.Helper()
	c.mu.Lock()
	cmd := c.command
	c.mu.Unlock()
	if cmd == nil {
		t.Fatal("no pass reached the sessions collection: the witness does not drive the command it explains")
	}
	var updates bson.A
	if err := cmd.Lookup("updates").Unmarshal(&updates); err != nil {
		t.Fatalf("read the captured updates: %v", err)
	}
	var out struct {
		ExecutionStats *struct {
			TotalDocsExamined int64 `bson:"totalDocsExamined"`
		} `bson:"executionStats"`
	}
	if err := st.db.RunCommand(context.Background(), bson.D{
		{Key: "explain", Value: bson.D{{Key: "update", Value: SessionsCollectionName}, {Key: "updates", Value: updates}}},
		{Key: "verbosity", Value: "executionStats"},
	}).Decode(&out); err != nil {
		t.Fatalf("explain the pass: %v", err)
	}
	if out.ExecutionStats == nil {
		t.Fatal("the explain answered no execution statistics")
	}
	return out.ExecutionStats.TotalDocsExamined
}

// TestMongoSessionStore_APassReadsTheLiveRowsNotTheHistory: the pass of a
// revocation reads the sessions that can still answer something — the live
// rows, and the rows written before lineages — through its indexes, never
// the account's or the lineage's retired history: thousands of retired rows
// (one per refresh, kept for the refresh TTL) cost the pass nothing.
func TestMongoSessionStore_APassReadsTheLiveRowsNotTheHistory(t *testing.T) {
	capture := &passCapture{}
	st := mongoSessionStoreWith(t, func(o *options.ClientOptions) { o.SetMonitor(capture.monitor()) })
	ctx := context.Background()
	const user, lineage, retired = "user-history", "lineage-history", 3000
	at := time.Now().UTC().Truncate(time.Millisecond)
	docs := make([]any, 0, retired+3)
	for i := 0; i < retired; i++ {
		revokedAt := at.Add(-time.Duration(retired-i) * time.Second)
		docs = append(docs, Session{
			ID: fmt.Sprintf("retired-%d", i), UserID: user, TokenHash: fmt.Sprintf("h-retired-%d", i),
			IssuedAt: revokedAt, ExpiresAt: at.Add(time.Hour), RevokedAt: &revokedAt,
			LineageID: lineage, RotationNonce: "n", RotationKeyID: "k0", SuccessorHash: fmt.Sprintf("h-retired-%d", i+1),
		})
	}
	docs = append(docs,
		Session{ID: "head", UserID: user, TokenHash: "h-head", IssuedAt: at, ExpiresAt: at.Add(time.Hour), LineageID: lineage},
		Session{ID: "other-device", UserID: user, TokenHash: "h-other", IssuedAt: at, ExpiresAt: at.Add(time.Hour), LineageID: "other-device"},
		Session{ID: "pre-lineage", UserID: user, TokenHash: "h-pre", IssuedAt: at, ExpiresAt: at.Add(time.Hour)},
	)
	if _, err := st.coll.InsertMany(ctx, docs); err != nil {
		t.Fatalf("seed the history: %v", err)
	}
	n, err := st.RevokeLineageSessions(ctx, user, lineage, at)
	if err != nil || n != 1 {
		t.Fatalf("the lineage pass matched %d row(s) (err %v), want its one live row", n, err)
	}
	if examined := capture.explainLast(t, st); examined > 5 {
		t.Fatalf("the lineage pass examined %d documents over a history of %d retired rows, want the live rows only", examined, retired)
	}
	generation, err := st.RecordAccountRevocation(ctx, user, at)
	if err != nil {
		t.Fatalf("RecordAccountRevocation: %v", err)
	}
	if n, err := st.RevokeAccountSessions(ctx, user, generation, at); err != nil || n != 2 {
		t.Fatalf("the account pass matched %d row(s) (err %v), want the other device and the pre-lineage row", n, err)
	}
	if examined := capture.explainLast(t, st); examined > 10 {
		t.Fatalf("the account pass examined %d documents over a history of %d retired rows, want the live rows only", examined, retired)
	}
}
