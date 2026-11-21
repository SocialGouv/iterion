package mongo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRunObjectsReadOnlyDeletedCompatibility(t *testing.T) {
	t.Parallel()
	for _, format := range []store.ValueFormat{1, 2} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := valueFormatMongo(t, true)
			if _, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: format}); e != nil {
				t.Fatal(e)
			}
			readers := map[string]func(context.Context) error{
				"session": func(c context.Context) error { _, e := s.GetBackendSession(c, "r", "ref"); return e },
				"tool":    func(c context.Context) error { _, _, _, e := s.ReadToolBlob(c, "r", "u", "output", 0, 0); return e },
				"scratch": func(c context.Context) error {
					r, e := s.OpenScratchBank(c, "r")
					if r != nil {
						_ = r.Close()
					}
					return e
				},
				"diff": func(c context.Context) error { _, e := s.GetRunDiffBlob(c, "r", "ref"); return e },
				"runfile": func(c context.Context) error {
					r, _, e := s.OpenRunFile(c, "r", "f")
					if r != nil {
						_ = r.Close()
					}
					return e
				},
			}
			wrong := mongoValueBind(t, ctx, "r", 3-format)
			for name, read := range readers {
				if e := read(wrong); !errors.Is(e, store.ErrValueFormatChanged) {
					t.Fatalf("%s wrong binding softened: %v", name, e)
				}
			}
			if e := s.DeleteRun(ctx, "r"); e != nil {
				t.Fatal(e)
			}
			for name, read := range readers {
				e := read(ctx)
				want := os.ErrNotExist
				if name == "diff" || name == "runfile" {
					want = blob.ErrArtifactNotFound
				}
				if !errors.Is(e, want) {
					t.Fatalf("%s deleted cause=%v want=%v", name, e, want)
				}
			}
			if rows, e := s.ListRunFiles(ctx, "r"); e != nil || len(rows) != 0 {
				t.Fatal(rows, e)
			}
			if e := s.PutBackendSession(ctx, "r", "ref", []byte("x")); !errors.Is(e, store.ErrRunDeleted) {
				t.Fatal("writer softened", e)
			}
			if e := s.PutScratchBank(ctx, "r", strings.NewReader(""), 0); !errors.Is(e, store.ErrRunDeleted) {
				t.Fatal("writer softened", e)
			}
			if _, e := s.runs.UpdateOne(ctx, bson.M{"_id": "r"}, bson.M{"$set": bson.M{"format_version": 99}}); e != nil {
				t.Fatal(e)
			}
			for name, read := range readers {
				if e := read(ctx); !errors.Is(e, store.ErrUnsupportedValueFormat) {
					t.Fatalf("%s future header softened: %v", name, e)
				}
			}
		})
	}
}
