package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpaquePayloadReadersRetainOriginalBinding(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(t.Context(), "r", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("original")
	files, err := s.EnsureRunFilesDir(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files, "file"), body, filePerm); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRunLog(t.Context(), run.ID, 0, body); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteTurn(t.Context(), &TurnCheckpoint{RunID: run.ID, NodeID: "node", Model: "original", Messages: body}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBackendSession(t.Context(), run.ID, "session", body); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteToolBlob(t.Context(), run.ID, "tool", "output", body); err != nil {
		t.Fatal(err)
	}
	if err := s.PutScratchBank(t.Context(), run.ID, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAttachment(t.Context(), run.ID, AttachmentRecord{Name: "attachment", OriginalFilename: "body"}, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	readers := map[string]func(context.Context) ([]byte, error){
		"turn-messages": func(ctx context.Context) ([]byte, error) { return s.LoadTurnMessages(ctx, run.ID, "node", 0, 0) },
		"run-file": func(ctx context.Context) ([]byte, error) {
			r, _, err := s.OpenRunFile(ctx, run.ID, "file")
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(r)
		},
		"log": func(ctx context.Context) ([]byte, error) { return s.ReadRunLogRange(ctx, run.ID, 0, 0) },
		"log-size": func(ctx context.Context) ([]byte, error) {
			n, err := s.RunLogSize(ctx, run.ID)
			if err != nil {
				return nil, err
			}
			if n != int64(len(body)) {
				return nil, errors.New("incorrect log size")
			}
			return body, nil
		},
		"attachment": func(ctx context.Context) ([]byte, error) {
			r, _, err := s.OpenAttachment(ctx, run.ID, "attachment")
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(r)
		},
		"session": func(ctx context.Context) ([]byte, error) { return s.GetBackendSession(ctx, run.ID, "session") },
		"tool": func(ctx context.Context) ([]byte, error) {
			data, _, _, err := s.ReadToolBlob(ctx, run.ID, "tool", "output", 0, 0)
			return data, err
		},
		"scratch": func(ctx context.Context) ([]byte, error) {
			r, err := s.OpenScratchBank(ctx, run.ID)
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(r)
		},
	}
	bind := func(birth string) context.Context {
		b, err := CaptureRunDataBinding(RunDataHeader{RunID: run.ID, BirthID: birth, Format: LegacyValueFormat})
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := WithRunDataBinding(t.Context(), b)
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			got, err := read(bind(run.BirthID))
			if err != nil || !bytes.Equal(got, body) {
				t.Fatal(string(got), err)
			}
			if _, err := read(bind(run.BirthID + "-stale")); !errors.Is(err, ErrRunDataIdentityChanged) {
				t.Fatalf("stale binding read opaque payload: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		protocol string
		want     error
	}{{"1", ErrRunDataCapability}, {"99", ErrUnsupportedRunDataProtocol}} {
		raw := []byte(`{"id":"r","birth_id":"` + run.BirthID + `","data_protocol":` + tc.protocol + `,"inputs":{"n":1e400}}`)
		if err := os.WriteFile(s.runJSONPath(run.ID), raw, filePerm); err != nil {
			t.Fatal(err)
		}
		for name, read := range readers {
			if _, err := read(t.Context()); !errors.Is(err, tc.want) {
				t.Fatalf("%s read protocol %s: %v", name, tc.protocol, err)
			}
		}
	}
}

func TestRunPayloadPathRefusesCrossRunAndCancellationBeforeIO(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := CaptureRunDataBinding(RunDataHeader{RunID: "original", Format: LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := WithRunDataBinding(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.runPayloadPath(ctx, "other", "file"); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.runPayloadPath(canceled, "r", "file"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.runPayloadPath(t.Context(), "r", "../escape"); err == nil {
		t.Fatal("accepted path escape")
	}
}

// Structured sidecars must perform the same identity check as opaque bytes;
// valid JSON cannot make an old caller read a replacement run's metadata.
func TestStructuredPayloadReadersRetainOriginalBinding(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(t.Context(), "r", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AppendPlanSnapshot(t.Context(), run.ID, PlanSnapshot{Todos: []PlanTodo{{Content: "original"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRunGitMeta(t.Context(), run.ID, &RunGitMeta{BaseCommit: "original"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRunDiffBlob(t.Context(), run.ID, "diff", []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunTags(t.Context(), run.ID, []string{"original"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendRunNote(t.Context(), run.ID, RunNote{Body: "original"}); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := s.ClaimRouteDecision(t.Context(), RouteDecision{RunID: run.ID, OutcomeSeq: 1, Reason: "original"}, time.Time{}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	attempt, err := s.RecordAttempt(t.Context(), run.ID, &AttemptSnapshot{WorkflowHash: "original"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteTurn(t.Context(), &TurnCheckpoint{RunID: run.ID, NodeID: "node", Model: "original"}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteInteraction(t.Context(), &Interaction{RunID: run.ID, ID: "original"}); err != nil {
		t.Fatal(err)
	}
	readers := map[string]func(context.Context) ([]byte, error){
		"interaction": func(ctx context.Context) ([]byte, error) {
			v, err := s.LoadInteraction(ctx, run.ID, "original")
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},
		"interactions": func(ctx context.Context) ([]byte, error) {
			v, err := s.ListInteractions(ctx, run.ID)
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},
		"turn": func(ctx context.Context) ([]byte, error) {
			v, err := s.LoadTurn(ctx, run.ID, "node", 0, 0)
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},
		"turns": func(ctx context.Context) ([]byte, error) {
			v, err := s.ListTurns(ctx, run.ID, "node", 0)
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},
		"latest-turn": func(ctx context.Context) ([]byte, error) {
			v, err := s.LatestTurn(ctx, run.ID, "node")
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},
		"turn-at-index": func(ctx context.Context) ([]byte, error) {
			v, err := s.LoadTurnAtIndex(ctx, run.ID, "node", 0)
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},
		"attempt": func(ctx context.Context) ([]byte, error) {
			v, err := s.LoadAttempt(ctx, run.ID, attempt.ID)
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},
		"routes": func(ctx context.Context) ([]byte, error) {
			v, err := s.ListRouteDecisions(ctx, run.ID)
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},

		"tags": func(ctx context.Context) ([]byte, error) {
			v, err := s.GetRunTags(ctx, run.ID)
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},
		"notes": func(ctx context.Context) ([]byte, error) {
			v, err := s.ListRunNotes(ctx, run.ID)
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},

		"plans": func(ctx context.Context) ([]byte, error) {
			v, err := s.ListPlanSnapshots(ctx, run.ID)
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},
		"gitmeta": func(ctx context.Context) ([]byte, error) {
			v, err := s.LoadRunGitMeta(ctx, run.ID)
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},
		"gitdiff": func(ctx context.Context) ([]byte, error) { return s.GetRunDiffBlob(ctx, run.ID, "diff") },
	}
	for _, birth := range []string{run.BirthID, run.BirthID + "-stale"} {
		binding, err := CaptureRunDataBinding(RunDataHeader{RunID: run.ID, BirthID: birth, Format: LegacyValueFormat})
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := WithRunDataBinding(t.Context(), binding)
		if err != nil {
			t.Fatal(err)
		}
		for name, read := range readers {
			data, err := read(ctx)
			if birth == run.BirthID {
				if err != nil || !bytes.Contains(data, []byte("original")) {
					t.Fatalf("%s: %s %v", name, data, err)
				}
			} else if !errors.Is(err, ErrRunDataIdentityChanged) {
				t.Errorf("%s stale binding: %v", name, err)
			}
		}
	}
}
