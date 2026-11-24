package mongo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type namespaceInboxBlob struct{ *valueFormatTestBlob }

func (b *namespaceInboxBlob) RunObjectsForScope(store.IncarnationDataScope) (blob.RunObjects, error) {
	return newInMemoryBlob(), nil
}
func (b *namespaceInboxBlob) ArtifactsForScope(store.IncarnationDataScope) (blob.ArtifactObjects, error) {
	return scopeArtifactFixture{newInMemoryBlob()}, nil
}

// This lifecycle is also exercised before activation with the private qualification
// overlay. The production capability fence must remain closed until every data
// domain is qualified; once enabled, this test runs in Mongo conformance.
func TestIncarnationInboxPublicNamespaceQualification(t *testing.T) {
	if err := store.RequireRunDataStorage(store.IncarnationRunData); err != nil {
		if !errors.Is(err, store.ErrRunDataCapability) {
			t.Fatal(err)
		}
		t.Skip("incarnation payload admission is not enabled")
	}

	for _, format := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := valueFormatMongo(t, true)
			s.blob = &namespaceInboxBlob{&valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}}
			if e := s.AppendQueuedMessage(ctx, "same", store.QueuedUserMessage{ID: "shared", Text: "legacy"}); e != nil {
				t.Fatal(e)
			}
			create := func() (*store.Run, context.Context, string) {
				t.Helper()
				r, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "same", WorkflowName: "wf", Format: format, DataProtocol: store.IncarnationRunData})
				if e != nil {
					t.Fatal(e)
				}
				h := store.RunDataHeader{RunID: r.ID, Format: format, BirthID: r.BirthID, Protocol: r.DataProtocol}
				b, e := store.CaptureRunDataBinding(h)
				if e != nil {
					t.Fatal(e)
				}
				c, e := store.WithRunDataBinding(ctx, b)
				if e != nil {
					t.Fatal(e)
				}
				scope, e := store.ResolveIncarnationDataScope(c, h)
				if e != nil {
					t.Fatal(e)
				}
				prefix, e := scope.BlobPrefix()
				if e != nil {
					t.Fatal(e)
				}
				return r, c, prefix
			}
			old, oldctx, oldprefix := create()
			if e := s.AppendQueuedMessage(oldctx, old.ID, store.QueuedUserMessage{ID: "shared", Text: "incarnation-one"}); e != nil {
				t.Fatal(e)
			}
			var legacy userMessageDoc
			if e := s.userMessages.FindOne(ctx, bson.M{"_id": userMessageID{RunID: old.ID, MessageID: "shared"}}).Decode(&legacy); e != nil || legacy.Text != "legacy" {
				t.Fatalf("incarnation write touched historical namespace: %q %v", legacy.Text, e)
			}
			strict := s.db.Collection("run_data_v1_user_messages")
			if n, e := strict.CountDocuments(ctx, bson.M{"data_namespace": oldprefix}); e != nil || n != 1 {
				t.Fatalf("incarnation row missing from isolated collection: %d %v", n, e)
			}
			if n, e := s.userMessages.CountDocuments(ctx, bson.M{"run_id": old.ID}); e != nil || n != 1 {
				t.Fatalf("incarnation row entered historical cleanup domain: %d %v", n, e)
			}
			// An orphan from another birth of the SAME logical ID must be invisible
			// to current readers and survive this birth's public cleanup.
			foreignprefix := oldprefix + "other/"
			_, e := strict.InsertOne(ctx, bson.M{"_id": bson.D{{Key: "run_id", Value: old.ID}, {Key: "message_id", Value: "foreign"}, {Key: "namespace", Value: foreignprefix}}, "run_id": old.ID, "id": "foreign", "text": "foreign-birth", "data_namespace": foreignprefix, "tenant_id": "format-tenant", "status": "queued", "queued_at": time.Now()})
			if e != nil {
				t.Fatal(e)
			}
			for _, read := range []func(context.Context, string) ([]store.QueuedUserMessage, error){s.LoadPendingQueuedMessages, s.ListQueuedMessages} {
				rows, e := read(oldctx, old.ID)
				if e != nil || len(rows) != 1 || rows[0].Text != "incarnation-one" || rows[0].RunID != old.ID {
					t.Fatalf("mixed namespace/read identity: %+v %v", rows, e)
				}
			}
			msg := store.QueuedUserMessage{ID: "once", Text: "original-once"}
			if yes, e := s.AppendQueuedMessageOnce(oldctx, old.ID, msg); e != nil || !yes {
				t.Fatal(yes, e)
			}
			msg.Text = "replayed"
			if yes, e := s.AppendQueuedMessageOnce(oldctx, old.ID, msg); e != nil || yes {
				t.Fatal(yes, e)
			}
			if e = s.UpdateQueuedMessageStatus(oldctx, old.ID, "once", store.QueuedMessageStatusDelivered, store.QueuedMessageStatusQueued); e != nil {
				t.Fatal(e)
			}
			if e = s.UpdateQueuedMessageStatus(oldctx, old.ID, "once", store.QueuedMessageStatusConsumed, store.QueuedMessageStatusQueued); !errors.Is(e, store.ErrQueuedMessageStatusConflict) {
				t.Fatal(e)
			}
			if e = s.UpdateQueuedMessageStatus(oldctx, old.ID, "foreign", store.QueuedMessageStatusConsumed); !errors.Is(e, store.ErrQueuedMessageNotFound) {
				t.Fatalf("foreign birth existence leaked: %v", e)
			}
			if rows, e := s.LoadPendingQueuedMessages(oldctx, old.ID); e != nil || len(rows) != 1 || rows[0].ID != "shared" {
				t.Fatal(rows, e)
			}
			// Independent concurrent insert-once and wrong-context attacks.
			var wg sync.WaitGroup
			var inserted atomic.Int32
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					yes, err := s.AppendQueuedMessageOnce(oldctx, old.ID, store.QueuedUserMessage{ID: "concurrent", Text: "immutable"})
					if err != nil {
						t.Error(err)
					}
					if yes {
						inserted.Add(1)
					}
				}()
			}
			wg.Wait()
			if inserted.Load() != 1 {
				t.Fatalf("insert-once winners=%d", inserted.Load())
			}
			wrong := store.WithTenant(oldctx, "other-tenant")
			cancelled, cancel := context.WithCancel(oldctx)
			cancel()
			for label, bad := range map[string]context.Context{"unbound": ctx, "wrong-tenant": wrong, "cancelled": cancelled} {
				for _, read := range []func(context.Context, string) ([]store.QueuedUserMessage, error){s.LoadPendingQueuedMessages, s.ListQueuedMessages} {
					rows, err := read(bad, old.ID)
					if err == nil || len(rows) != 0 {
						t.Fatalf("%s read leaked: %+v %v", label, rows, err)
					}
				}
				if err := s.AppendQueuedMessage(bad, old.ID, store.QueuedUserMessage{ID: "bad", Text: "bad"}); err == nil {
					t.Fatalf("%s write admitted", label)
				}
				if yes, err := s.AppendQueuedMessageOnce(bad, old.ID, store.QueuedUserMessage{ID: "bad", Text: "bad"}); err == nil || yes {
					t.Fatalf("%s once admitted", label)
				}
				if err := s.UpdateQueuedMessageStatus(bad, old.ID, "shared", store.QueuedMessageStatusCancelled); err == nil {
					t.Fatalf("%s CAS admitted", label)
				}
			}
			if e = s.DeleteRun(oldctx, old.ID); e != nil {
				t.Fatal(e)
			}
			if n, e := strict.CountDocuments(ctx, bson.M{"data_namespace": oldprefix}); e != nil || n != 0 {
				t.Fatalf("own inbox retained: %d %v", n, e)
			}
			if n, e := strict.CountDocuments(ctx, bson.M{"data_namespace": foreignprefix}); e != nil || n != 1 {
				t.Fatalf("another birth swept: %d %v", n, e)
			}
			if e = s.userMessages.FindOne(ctx, bson.M{"_id": userMessageID{RunID: old.ID, MessageID: "shared"}}).Decode(&legacy); e != nil || legacy.Text != "legacy" {
				t.Fatalf("strict cleanup swept legacy: %v", e)
			}
			if n, e := s.PruneDeletionMarkers(oldctx, time.Now().Add(time.Hour)); e != nil || n != 1 {
				t.Fatal(n, e)
			}
			next, nextctx, nextprefix := create()
			if nextprefix == oldprefix || next.BirthID == old.BirthID {
				t.Fatal("birth namespace reused")
			}
			if e = s.AppendQueuedMessage(nextctx, next.ID, store.QueuedUserMessage{ID: "shared", Text: "incarnation-two"}); e != nil {
				t.Fatal(e)
			}
			if e = s.AppendQueuedMessage(oldctx, next.ID, store.QueuedUserMessage{ID: "late", Text: "old"}); !errors.Is(e, store.ErrRunDataIdentityChanged) {
				t.Fatal(e)
			}
			if e = s.DeleteRun(oldctx, next.ID); !errors.Is(e, store.ErrRunDataIdentityChanged) {
				t.Fatal(e)
			}
			rows, e := s.ListQueuedMessages(nextctx, next.ID)
			if e != nil || len(rows) != 1 || rows[0].Text != "incarnation-two" {
				t.Fatal(rows, e)
			}
		})
	}
}
