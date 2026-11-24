package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunMetadataFilesystemDeletionHeaders(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"delete", "delete-interrupted", "prune"} {
		for _, tt := range []struct {
			name, value string
			want        error
		}{{"known", "2", nil}, {"future", "99", ErrUnsupportedValueFormat}, {"duplicate", `2,"format_version":99`, ErrInvalidValueFormat}} {
			t.Run(op+"/"+tt.name, func(t *testing.T) {
				s, e := New(t.TempDir())
				if e != nil {
					t.Fatal(e)
				}
				ctx := t.Context()
				if _, e = s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", Format: 2}); e != nil {
					t.Fatal(e)
				}
				before, e := os.ReadFile(s.runJSONPath("r"))
				if e != nil {
					t.Fatal(e)
				}
				before = []byte(strings.Replace(string(before), `"format_version": 2`, `"format_version": `+tt.value, 1))
				if e = os.WriteFile(s.runJSONPath("r"), before, 0600); e != nil {
					t.Fatal(e)
				}
				if op != "delete" {
					if e = os.WriteFile(filepath.Join(s.runDir("r"), deletionMarkerName), []byte("interrupted"), 0600); e != nil {
						t.Fatal(e)
					}
				}
				if op == "prune" {
					var n int
					n, e = s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour))
					if (tt.want == nil && n != 1) || (tt.want != nil && n != 0) {
						t.Fatal(n, e)
					}
				} else {
					e = s.DeleteRun(ctx, "r")
				}
				if tt.want != nil {
					if !errors.Is(e, tt.want) {
						t.Fatalf("err=%v want=%v", e, tt.want)
					}
					after, err := os.ReadFile(s.runJSONPath("r"))
					if err != nil || !bytes.Equal(before, after) {
						t.Fatal("refusal changed run", err)
					}
				} else {
					if e != nil {
						t.Fatal(e)
					}
					if _, e = os.Stat(s.runJSONPath("r")); !os.IsNotExist(e) {
						t.Fatal("cleanup did not remove run", e)
					}
				}
			})
		}
	}
}
