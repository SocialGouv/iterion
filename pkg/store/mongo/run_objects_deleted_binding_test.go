package mongo

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestRunObjectsDeletedBinding(t *testing.T) {
	t.Parallel()
	for _, format := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			t.Parallel()
			s, ctx := valueFormatMongo(t, true)
			if _, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: format}); err != nil {
				t.Fatal(err)
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
				"list": func(c context.Context) error { _, e := s.ListRunFiles(c, "r"); return e },
			}
			bindings := map[string]context.Context{
				"wrong-format": mongoValueBind(t, ctx, "r", 3-format),
				"wrong-run":    mongoValueBind(t, ctx, "other", format),
			}
			for binding, c := range bindings {
				for name, read := range readers {
					if e := read(c); !errors.Is(e, store.ErrValueFormatChanged) {
						t.Fatalf("live %s/%s: %v", binding, name, e)
					}
				}
			}
			if err := s.DeleteRun(ctx, "r"); err != nil {
				t.Fatal(err)
			}
			for binding, c := range bindings {
				t.Run(binding, func(t *testing.T) {
					for name, read := range readers {
						t.Run(name, func(t *testing.T) {
							if e := read(c); !errors.Is(e, store.ErrValueFormatChanged) {
								t.Fatalf("contradictory binding softened after tombstone: %v", e)
							}
						})
					}
				})
			}
			for _, c := range []context.Context{ctx, mongoValueBind(t, ctx, "r", format)} {
				if rows, e := s.ListRunFiles(c, "r"); e != nil || len(rows) != 0 {
					t.Fatal("deleted list compatibility", rows, e)
				}
			}
		})
	}
}
