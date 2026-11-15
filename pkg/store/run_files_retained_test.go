package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestIncarnationRunFileReadersAcceptToolNames(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := directoryFixture(t, format)
			root, err := s.EnsureRunFilesDir(ctx, "r")
			if err != nil {
				t.Fatal(err)
			}
			rel := "part..name/résultat with spaces.txt"
			full := filepath.Join(root, filepath.FromSlash(rel))
			if err = os.MkdirAll(filepath.Dir(full), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(full, []byte("retained bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			rows, err := s.ListRunFiles(ctx, "r")
			if err != nil || len(rows) != 1 || rows[0].Path != rel || rows[0].Size != 14 {
				t.Fatal(rows, err)
			}
			f, info, err := s.OpenRunFile(ctx, "r", rel)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			body, err := io.ReadAll(f)
			if err != nil || string(body) != "retained bytes" || info.Path != rel {
				t.Fatal(string(body), info, err)
			}
			if err = os.Symlink("résultat with spaces.txt", filepath.Join(filepath.Dir(full), "link")); err != nil {
				t.Fatal(err)
			}
			if f, _, err = s.OpenRunFile(ctx, "r", "part..name/link"); err == nil {
				if f != nil {
					_ = f.Close()
				}
				t.Fatal("leaf symlink accepted")
			}
		})
	}
}

func TestIncarnationRunFileReadersRefuseAncestorReplacement(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		for _, op := range []string{"list", "open"} {
			t.Run(fmt.Sprintf("%d/%s", format, op), func(t *testing.T) {
				s, ctx := directoryFixture(t, format)
				root, err := s.EnsureRunFilesDir(ctx, "r")
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(root, "out.txt"), []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
				parent := filepath.Dir(root)
				if err = os.Rename(parent, parent+"-held"); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(parent+"-held", parent); err != nil {
					t.Fatal(err)
				}
				if op == "list" {
					_, err = s.ListRunFiles(ctx, "r")
				} else {
					var f io.ReadCloser
					f, _, err = s.OpenRunFile(ctx, "r", "out.txt")
					if f != nil {
						_ = f.Close()
					}
				}
				if err == nil {
					t.Fatal("ancestor replacement accepted")
				}
			})
		}
	}
}
