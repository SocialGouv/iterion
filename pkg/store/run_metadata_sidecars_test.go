package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunMetadataChildrenFilesystemPublic(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"legacy", "exact", "future", "bound-other", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			format := 2
			if mode == "legacy" {
				format = 1
			}
			if _, err := s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", Format: ValueFormat(format)}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetRunTags(ctx, "r", []string{"before"}); err != nil {
				t.Fatal(err)
			}
			msg := QueuedUserMessage{ID: "old", Text: "before", SkillRefs: []string{"skill"}}
			if err := s.AppendQueuedMessage(ctx, "r", msg); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveRunGitMeta(ctx, "r", &RunGitMeta{BaseCommit: "before"}); err != nil {
				t.Fatal(err)
			}
			if ok, _, err := s.ClaimRouteDecision(ctx, RouteDecision{RunID: "r", OutcomeSeq: 0}, time.Time{}); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if err := s.WriteTurn(ctx, &TurnCheckpoint{RunID: "r", NodeID: "n", TextDigest: "before", Messages: []byte("before")}); err != nil {
				t.Fatal(err)
			}
			if err := s.AppendRunLog(ctx, "r", 0, []byte("before")); err != nil {
				t.Fatal(err)
			}

			snapshot := func() []byte {
				t.Helper()
				if mode == "future" {
					// Public readers now refuse unknown headers too. Inspect the
					// physical tree so refusal cannot hide any changed child bytes.
					files := map[string][]byte{}
					err := filepath.WalkDir(s.runDir("r"), func(path string, d fs.DirEntry, err error) error {
						if err != nil {
							return err
						}
						if d.IsDir() {
							return nil
						}
						data, err := os.ReadFile(path)
						if err != nil {
							return err
						}
						files[path] = data
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					raw, err := json.Marshal(files)
					if err != nil {
						t.Fatal(err)
					}
					return raw
				}
				tags, e := s.GetRunTags(ctx, "r")
				if e != nil {
					t.Fatal(e)
				}
				msgs, e := s.ListQueuedMessages(ctx, "r")
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
				log, e := s.ReadRunLogRange(ctx, "r", 0, 0)
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
				raw, err := os.ReadFile(s.runJSONPath("r"))
				if err != nil {
					t.Fatal(err)
				}
				raw = []byte(strings.Replace(string(raw), `"format_version": 2`, `"format_version": 99`, 1))
				if err = os.WriteFile(s.runJSONPath("r"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				want = ErrUnsupportedValueFormat
			case "bound-other":
				var err error
				callctx, err = WithRunValueFormat(ctx, RunValueFormat{RunID: "r", Format: 1})
				if err != nil {
					t.Fatal(err)
				}
				want = ErrValueFormatChanged
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
				{"message", func() error { return s.AppendQueuedMessage(callctx, "r", QueuedUserMessage{ID: "new", Text: "after"}) }},
				{"once", func() error {
					ok, e := s.AppendQueuedMessageOnce(callctx, "r", QueuedUserMessage{ID: "once", Text: "after"})
					if e == nil && !ok {
						t.Error("first insert-once returned false")
					}
					if e != nil && ok {
						t.Error("failed insert-once returned true")
					}
					return e
				}},
				{"status", func() error {
					return s.UpdateQueuedMessageStatus(callctx, "r", "old", QueuedMessageStatusDelivered, QueuedMessageStatusQueued)
				}},
				{"gitmeta", func() error { return s.SaveRunGitMeta(callctx, "r", &RunGitMeta{BaseCommit: "after"}) }},
			}
			actions = append(actions, struct {
				name string
				run  func() error
			}{"claim", func() error {
				ok, _, err := s.ClaimRouteDecision(callctx, RouteDecision{RunID: "r", OutcomeSeq: 1}, time.Time{})
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
				}{"finish", func() error { return s.FinishRouteDecision(callctx, "r", 0, RouteDecisionSucceeded, "") }},
				struct {
					name string
					run  func() error
				}{"turn", func() error {
					turn := &TurnCheckpoint{RunID: "r", NodeID: "n", TextDigest: "after", Messages: []byte("after")}
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
			if ok, err := s.AppendQueuedMessageOnce(ctx, "r", QueuedUserMessage{ID: "once", Text: "must not replace"}); err != nil || ok {
				t.Fatal(ok, err)
			}
			if err := s.UpdateQueuedMessageStatus(ctx, "r", "old", QueuedMessageStatusCancelled, QueuedMessageStatusQueued); !errors.Is(err, ErrQueuedMessageStatusConflict) {
				t.Fatal(err)
			}
			if err := s.UpdateQueuedMessageStatus(ctx, "r", "missing", QueuedMessageStatusCancelled); !errors.Is(err, ErrQueuedMessageNotFound) {
				t.Fatal(err)
			}
			if ok, existing, err := s.ClaimRouteDecision(ctx, RouteDecision{RunID: "r", OutcomeSeq: 0}, time.Time{}); err != nil || ok || existing == nil {
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
				if m.ID == "old" && m.Status != QueuedMessageStatusDelivered {
					t.Fatal(m)
				}
				if m.ID == "once" && m.Text != "after" {
					t.Fatal(m)
				}
			}
		})
	}
}
