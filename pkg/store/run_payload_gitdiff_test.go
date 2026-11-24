package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIncarnationDiffBlobRestartAndDirectoryReplacement(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		for _, operation := range []string{"read", "write"} {
			t.Run(fmt.Sprintf("%d/%s", format, operation), func(t *testing.T) {
				s, ctx := directoryFixture(t, format)
				const ref = "référence with spaces"
				body := []byte(`{"before":"9007199254740993.0","after":"unchanged"}`)
				if err := s.PutRunDiffBlob(ctx, "r", ref, body); err != nil {
					t.Fatal(err)
				}
				restarted, err := New(s.root)
				if err != nil {
					t.Fatal(err)
				}
				got, err := restarted.GetRunDiffBlob(ctx, "r", ref)
				if err != nil || string(got) != string(body) {
					t.Fatalf("restart: %q %v", got, err)
				}
				key, err := s.runPayloadPath(ctx, "r", "gitdiffs")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(key, key+"-retired"); err != nil {
					t.Fatal(err)
				}
				outside := t.TempDir()
				victim := filepath.Join(outside, ref+".json")
				if err := os.WriteFile(victim, []byte("outside"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, key); err != nil {
					t.Fatal(err)
				}
				if operation == "read" {
					_, err = restarted.GetRunDiffBlob(ctx, "r", ref)
				} else {
					err = restarted.PutRunDiffBlob(ctx, "r", ref, []byte("overwritten"))
				}
				if err == nil {
					t.Fatal("accepted replacement symlink")
				}
				got, err = os.ReadFile(victim)
				if err != nil || string(got) != "outside" {
					t.Fatalf("outside changed: %q %v", got, err)
				}
			})
		}
	}
}
