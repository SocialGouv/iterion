package mongo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
	"time"
)

func TestRunMetadataChildrenMongoPublic(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"legacy", "exact", "future", "bound-other", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s, ctx := valueFormatMongo(t, true)
			format := 2
			if mode == "legacy" {
				format = 1
			}
			if _, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: store.ValueFormat(format)}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetRunTags(ctx, "r", []string{"before"}); err != nil {
				t.Fatal(err)
			}
			msg := store.QueuedUserMessage{ID: "old", Text: "before", SkillRefs: []string{"skill"}}
			if err := s.AppendQueuedMessage(ctx, "r", msg); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveRunGitMeta(ctx, "r", &store.RunGitMeta{BaseCommit: "before"}); err != nil {
				t.Fatal(err)
			}
			if ok, _, err := s.ClaimRouteDecision(ctx, store.RouteDecision{RunID: "r", OutcomeSeq: 0}, time.Time{}); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if err := s.WriteTurn(ctx, &store.TurnCheckpoint{RunID: "r", NodeID: "n", TextDigest: "before", Messages: []byte("before")}); err != nil {
				t.Fatal(err)
			}
			if err := s.AppendRunLog(ctx, "r", 0, []byte("before")); err != nil {
				t.Fatal(err)
			}
			if data, e := s.ReadRunLogRange(ctx, "r", 0, 0); e != nil || string(data) != "before" {
				t.Fatalf("baseline log %q %v", data, e)
			}

			snapshot := func() []byte {
				t.Helper()
				tags, e := s.GetRunTags(ctx, "r")
				if e != nil {
					t.Fatal(e)
				}
				// Inspect physical rows even when a future header correctly refuses public reads.
				msgs, e := s.findUserMessages(ctx, withTenantFilter(ctx, bson.M{"run_id": "r"}))
				if e != nil {
					t.Fatal(e)
				}
				meta, e := s.LoadRunGitMeta(ctx, "r")
				if e != nil {
					t.Fatal(e)
				}
				routes, e := s.ListRouteDecisions(ctx, "r")
				if e != nil {
					t.Fatal(e)
				}
				turns, e := s.ListTurns(ctx, "r", "n", 0)
				if e != nil {
					t.Fatal(e)
				}
				// Preserve the mutation no-op oracle even when a future header
				// correctly refuses the public opaque log reader.
				cur, e := s.runLogs.Find(ctx, withTenantFilter(ctx, bson.M{"run_id": "r"}))
				if e != nil {
					t.Fatal(e)
				}
				var log []runLogDoc
				e = cur.All(ctx, &log)
				cur.Close(ctx)
				if e != nil {
					t.Fatal(e)
				}
				raw, e := json.Marshal([]any{tags, msgs, meta, routes, turns, log})
				if e != nil {
					t.Fatal(e)
				}
				return raw
			}
			callctx := ctx
			var want error
			switch mode {
			case "future":
				if _, err := s.runs.UpdateOne(ctx, bson.M{"_id": "r"}, bson.M{"$set": bson.M{"format_version": 99}}); err != nil {
					t.Fatal(err)
				}
				want = store.ErrUnsupportedValueFormat
				if data, e := s.ReadRunLogRange(ctx, "r", 0, 0); !errors.Is(e, store.ErrUnsupportedValueFormat) || len(data) != 0 {
					t.Fatalf("future header exposed logs: %q %v", data, e)
				}
				if size, e := s.RunLogSize(ctx, "r"); !errors.Is(e, store.ErrUnsupportedValueFormat) || size != 0 {
					t.Fatalf("future header exposed log size: %d %v", size, e)
				}
				for _, read := range []func(context.Context, string) ([]store.QueuedUserMessage, error){s.LoadPendingQueuedMessages, s.ListQueuedMessages} {
					rows, err := read(ctx, "r")
					if !errors.Is(err, store.ErrUnsupportedValueFormat) || len(rows) != 0 {
						t.Fatalf("future header exposed inbox: rows=%v err=%v", rows, err)
					}
				}
			case "bound-other":
				var err error
				callctx, err = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: "r", Format: 1})
				if err != nil {
					t.Fatal(err)
				}
				want = store.ErrValueFormatChanged
			case "cancelled":
				var cancel context.CancelFunc
				callctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			before := snapshot()
			actions := []struct {
				name string
				run  func() error
			}{
				{"tags", func() error { return s.SetRunTags(callctx, "r", []string{"after"}) }},
				{"message", func() error {
					return s.AppendQueuedMessage(callctx, "r", store.QueuedUserMessage{ID: "new", Text: "after"})
				}},
				{"once", func() error {
					ok, e := s.AppendQueuedMessageOnce(callctx, "r", store.QueuedUserMessage{ID: "once", Text: "after"})
					if e == nil && !ok {
						t.Error("first insert-once returned false")
					}
					if e != nil && ok {
						t.Error("failed insert-once returned true")
					}
					return e
				}},
				{"status", func() error {
					return s.UpdateQueuedMessageStatus(callctx, "r", "old", store.QueuedMessageStatusDelivered, store.QueuedMessageStatusQueued)
				}},
				{"gitmeta", func() error { return s.SaveRunGitMeta(callctx, "r", &store.RunGitMeta{BaseCommit: "after"}) }},
			}
			actions = append(actions, struct {
				name string
				run  func() error
			}{"claim", func() error {
				ok, _, err := s.ClaimRouteDecision(callctx, store.RouteDecision{RunID: "r", OutcomeSeq: 1}, time.Time{})
				if err == nil && !ok {
					t.Error("first claim returned false")
				}
				if err != nil && ok {
					t.Error("failed claim returned true")
				}
				return err
			}},
				struct {
					name string
					run  func() error
				}{"finish", func() error { return s.FinishRouteDecision(callctx, "r", 0, store.RouteDecisionSucceeded, "") }},
				struct {
					name string
					run  func() error
				}{"turn", func() error {
					turn := &store.TurnCheckpoint{RunID: "r", NodeID: "n", TextDigest: "after", Messages: []byte("after")}
					err := s.WriteTurn(callctx, turn)
					if err == nil && turn.Attempt != 2 {
						t.Errorf("attempt=%d want 2", turn.Attempt)
					}
					return err
				}},
				struct {
					name string
					run  func() error
				}{"log", func() error { return s.AppendRunLog(callctx, "r", 6, []byte("after")) }})

			for _, a := range actions {
				err := a.run()
				if want == nil {
					if err != nil {
						t.Fatalf("%s: %v", a.name, err)
					}
				} else if !errors.Is(err, want) {
					t.Fatalf("%s: got %v want %v", a.name, err, want)
				}
				if want != nil && !bytes.Equal(before, snapshot()) {
					t.Fatalf("%s changed child on refusal", a.name)
				}
			}
			if want != nil {
				return
			}
			parentBefore, err := s.LoadRun(ctx, "r")
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.AppendQueuedMessageOnce(ctx, "r", store.QueuedUserMessage{ID: "once", Text: "must not replace"}); err != nil || ok {
				t.Fatal(ok, err)
			}
			if err := s.UpdateQueuedMessageStatus(ctx, "r", "old", store.QueuedMessageStatusCancelled, store.QueuedMessageStatusQueued); !errors.Is(err, store.ErrQueuedMessageStatusConflict) {
				t.Fatal(err)
			}
			if err := s.UpdateQueuedMessageStatus(ctx, "r", "missing", store.QueuedMessageStatusCancelled); !errors.Is(err, store.ErrQueuedMessageNotFound) {
				t.Fatal(err)
			}
			if ok, existing, err := s.ClaimRouteDecision(ctx, store.RouteDecision{RunID: "r", OutcomeSeq: 0}, time.Time{}); err != nil || ok || existing == nil {
				t.Fatal(ok, existing, err)
			}
			if err := s.AppendRunLog(ctx, "r", 6, []byte("after")); err != nil {
				t.Fatal(err)
			}

			parentAfter, err := s.LoadRun(ctx, "r")
			if err != nil || parentAfter.CASVersion != parentBefore.CASVersion {
				t.Fatal("no-op changed parent", err)
			}
			msgs, err := s.ListQueuedMessages(ctx, "r")
			if err != nil || len(msgs) != 3 {
				t.Fatal(msgs, err)
			}
			for _, m := range msgs {
				if m.ID == "old" && m.Status != store.QueuedMessageStatusDelivered {
					t.Fatal(m)
				}
				if m.ID == "once" && m.Text != "after" {
					t.Fatal(m)
				}
			}
		})
	}
}
