package store

import (
	"fmt"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIncarnationRunLogDescriptorSurvivesParentReplacement(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := directoryFixture(t, format)
			logger, closer, e := TeeRunLogContext(ctx, iterlog.Nop(), iterlog.LevelInfo, s.root, "r")
			if e != nil {
				t.Fatal(e)
			}
			if closer == nil {
				t.Fatal("no descriptor")
			}
			key, e := s.runPayloadPath(ctx, "r", "run.log")
			if e != nil {
				t.Fatal(e)
			}
			parent := filepath.Dir(key)
			if e = os.Rename(parent, parent+"-retired"); e != nil {
				t.Fatal(e)
			}
			if e = os.Mkdir(parent, 0700); e != nil {
				t.Fatal(e)
			}
			logger.Info("retained descriptor witness")
			if e = closer.Close(); e != nil {
				t.Fatal(e)
			}
			data, e := os.ReadFile(filepath.Join(parent+"-retired", "run.log"))
			if e != nil || !strings.Contains(string(data), "retained descriptor witness") {
				t.Fatal(string(data), e)
			}
			if _, e = os.Stat(key); !os.IsNotExist(e) {
				t.Fatal("logger reopened replacement", e)
			}
		})
	}
}

func TestIncarnationRunLogOperationsMustRefuseAncestorReplacement(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		for _, op := range []string{"append", "read", "size", "tee"} {
			t.Run(fmt.Sprintf("%d/%s", format, op), func(t *testing.T) {
				s, ctx := directoryFixture(t, format)
				if e := s.AppendRunLog(ctx, "r", 0, []byte("original")); e != nil {
					t.Fatal(e)
				}
				key, e := s.runPayloadPath(ctx, "r", "run.log")
				if e != nil {
					t.Fatal(e)
				}
				parent := filepath.Dir(key)
				if e = os.Rename(parent, parent+"-retired"); e != nil {
					t.Fatal(e)
				}
				outside := t.TempDir()
				if e = os.WriteFile(filepath.Join(outside, "run.log"), []byte("foreign"), 0600); e != nil {
					t.Fatal(e)
				}
				if e = os.Symlink(outside, parent); e != nil {
					t.Fatal(e)
				}
				switch op {
				case "append":
					e = s.AppendRunLog(ctx, "r", 0, []byte("escaped"))
				case "read":
					_, e = s.ReadRunLogRange(ctx, "r", 0, 0)
				case "size":
					_, e = s.RunLogSize(ctx, "r")
				case "tee":
					_, c, err := TeeRunLogContext(ctx, iterlog.Nop(), iterlog.LevelInfo, s.root, "r")
					e = err
					if c != nil {
						c.Close()
					}
				}
				if e == nil {
					t.Fatal("ancestor replacement accepted")
				}
			})
		}
	}
}
