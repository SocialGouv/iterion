package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIncarnationComposedSnapshotsArtifactsAndInteractions(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := directoryFixture(t, format)
			if _, e := s.AppendRunNote(ctx, "r", RunNote{Body: "first"}); e != nil {
				t.Fatal(e)
			}
			first, wrote, e := s.AppendPlanSnapshot(ctx, "r", PlanSnapshot{Todos: []PlanTodo{{Content: "same"}}})
			if e != nil || !wrote {
				t.Fatal(first, wrote, e)
			}
			second, wrote, e := s.AppendPlanSnapshot(ctx, "r", PlanSnapshot{Todos: []PlanTodo{{Content: "same"}}})
			if e != nil || wrote || second.Seq != first.Seq {
				t.Fatal(second, wrote, e)
			}
			data := map[string]any{"exact": json.Number("9007199254740993.0")}
			for _, v := range []int{2, 1} {
				if e = s.WriteArtifact(ctx, &Artifact{RunID: "r", NodeID: "n", Version: v, Data: data}); e != nil {
					t.Fatal(e)
				}
			}
			if e = s.WriteInteraction(ctx, &Interaction{RunID: "r", ID: "question", Questions: map[string]any{"prompt": "answer"}}); e != nil {
				t.Fatal(e)
			}
			resumed, e := New(s.root)
			if e != nil {
				t.Fatal(e)
			}
			notes, e := resumed.ListRunNotes(ctx, "r")
			if e != nil || len(notes) != 1 || notes[0].Body != "first" {
				t.Fatal(notes, e)
			}
			plans, e := resumed.ListPlanSnapshots(ctx, "r")
			if e != nil || len(plans) != 1 {
				t.Fatal(plans, e)
			}
			// Clear the cache so Latest must scan and decode from the same directory.
			r, e := resumed.LoadRun(ctx, "r")
			if e != nil {
				t.Fatal(e)
			}
			r.ArtifactIndex = nil
			if e = resumed.SaveRun(ctx, r); e != nil {
				t.Fatal(e)
			}
			a, e := resumed.LoadLatestArtifact(ctx, "r", "n")
			if e != nil || a.Version != 2 {
				t.Fatal(a, e)
			}
			if format == ExactValueFormat {
				if n, ok := a.Data["exact"].(json.Number); !ok || n.String() != "9007199254740993.0" {
					t.Fatalf("exact value changed: %#v", a.Data)
				}
			}
			versions, e := resumed.ListArtifactVersions(ctx, "r", "n")
			if e != nil || len(versions) != 2 || versions[0].Version != 1 || versions[1].Version != 2 {
				t.Fatal(versions, e)
			}
			ids, e := resumed.ListInteractions(ctx, "r")
			if e != nil || len(ids) != 1 || ids[0] != "question" {
				t.Fatal(ids, e)
			}
			if _, e = resumed.AnswerInteractionCAS(ctx, "r", "question", data); e != nil {
				t.Fatal(e)
			}
			if _, e = s.AnswerInteractionCAS(ctx, "r", "question", nil); !errors.Is(e, ErrInteractionAlreadyAnswered) {
				t.Fatal("duplicate answer accepted", e)
			}
			answer, e := s.LoadInteraction(ctx, "r", "question")
			if e != nil || answer.AnsweredAt == nil {
				t.Fatal(answer, e)
			}
			if format == ExactValueFormat {
				if n, ok := answer.Answers["exact"].(json.Number); !ok || n.String() != "9007199254740993.0" {
					t.Fatalf("exact answer changed: %#v", answer.Answers)
				}
			}
		})
	}
}

type directorySwapValue struct{ swap func() error }

func (v directorySwapValue) MarshalJSON() ([]byte, error) {
	if e := v.swap(); e != nil {
		return nil, e
	}
	return []byte(`"ok"`), nil
}

func TestIncarnationInteractionAnswerRetainsLoadedDirectory(t *testing.T) {
	s, ctx := directoryFixture(t, LegacyValueFormat)
	if e := s.WriteInteraction(ctx, &Interaction{RunID: "r", ID: "question"}); e != nil {
		t.Fatal(e)
	}
	d, e := s.openRunPayloadDirectory(ctx, "r", false, "interactions")
	if e != nil {
		t.Fatal(e)
	}
	path := d.parent.key
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(filepath.Join(path, "question.json"))
	if e != nil {
		t.Fatal(e)
	}
	swapped := false
	value := directorySwapValue{swap: func() error {
		if swapped {
			return nil
		}
		swapped = true
		if e := os.Rename(path, path+"-retained"); e != nil {
			return e
		}
		if e := os.Mkdir(path, 0700); e != nil {
			return e
		}
		return os.WriteFile(filepath.Join(path, "question.json"), before, 0600)
	}}
	if _, e = s.AnswerInteractionCAS(ctx, "r", "question", map[string]any{"swap": value}); e != nil {
		t.Fatal(e)
	}
	if !swapped {
		t.Fatal("swap callback did not execute")
	}
	data, e := os.ReadFile(filepath.Join(path+"-retained", "question.json"))
	if e != nil {
		t.Fatal(e)
	}
	var answer Interaction
	if e = DecodeValueRecordJSON(ctx, LegacyValueFormat, ExactInteraction, data, &answer); e != nil {
		t.Fatal(e)
	}
	if answer.AnsweredAt == nil || answer.Answers["swap"] != "ok" {
		t.Fatal("answer reopened the replacement directory", answer)
	}
	after, e := os.ReadFile(filepath.Join(path, "question.json"))
	if e != nil || string(after) != string(before) {
		t.Fatal("replacement row modified", e)
	}
}

func TestIncarnationInteractionAnswerRefusesStoredIdentityRedirect(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		for _, field := range []string{"run", "row"} {
			t.Run(fmt.Sprintf("%d/%s", format, field), func(t *testing.T) {
				s, ctx := directoryFixture(t, format)
				row := &Interaction{RunID: "r", ID: "question"}
				if e := s.WriteInteraction(ctx, row); e != nil {
					t.Fatal(e)
				}
				if field == "run" {
					row.RunID = "other-run"
				} else {
					row.ID = "other-question"
				}
				raw, e := marshalValueRecordIndent(ctx, format, ExactInteraction, row)
				if e != nil {
					t.Fatal(e)
				}
				d, e := s.openRunPayloadDirectory(ctx, "r", true, "interactions")
				if e != nil {
					t.Fatal(e)
				}
				defer d.Close()
				if e = d.WriteAtomic(ctx, "question.json", raw, 0600); e != nil {
					t.Fatal(e)
				}
				if _, e = s.AnswerInteractionCAS(ctx, "r", "question", map[string]any{"answer": "x"}); !errors.Is(e, ErrRunDataIdentityChanged) {
					t.Fatal("stored identity redirected answer", e)
				}
				after, e := d.ReadFile("question.json")
				if e != nil || string(after) != string(raw) {
					t.Fatal("refused row modified", e)
				}
				if _, e = d.ReadFile("other-question.json"); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("foreign row created", e)
				}
			})
		}
	}
}
