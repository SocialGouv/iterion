package cli

import (
	"bytes"
	"context"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndependentR53LegacyLogFailureKeepsFork(t *testing.T) {
	root := t.TempDir()
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRun(context.Background(), "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(root, "runs", "r", "run.log"), 0700); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	parent := iterlog.New(iterlog.LevelInfo, &buf)
	child, closer, err := teeRunLogContext(context.Background(), parent, iterlog.LevelInfo, root, "r")
	if err != nil || closer != nil || child == parent {
		t.Fatalf("best effort fork lost: err=%v closer=%v same=%v", err, closer, child == parent)
	}
	child.SetRedactor(func(s string) string { return strings.ReplaceAll(s, "sample-value", "REDACTED") })
	child.Info("child sample-value")
	parent.Info("parent sample-value")
	got := buf.String()
	if !strings.Contains(got, "child REDACTED") || !strings.Contains(got, "parent sample-value") {
		t.Fatalf("redactor ownership changed: %q", got)
	}
}
