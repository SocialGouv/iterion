package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPreEngineRefusalRejectsCorruptFilesystemBinding(t *testing.T) {
	ctx := store.WithIdentity(context.Background(), "review", "owner")
	root := t.TempDir()
	s, e := store.New(root)
	if e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"source", "target"} {
		r, e := s.CreateRun(ctx, id, id, nil)
		if e != nil {
			t.Fatal(e)
		}
		at := store.QueuedFlipAt(time.Now())
		r.Name = id
		r.Status = store.RunStatusQueued
		r.TenantID = "review"
		r.QueuedAt = &at
		if e = s.SaveRun(ctx, r); e != nil {
			t.Fatal(e)
		}
	}
	sourcePath := filepath.Join(root, "runs", "source", "run.json")
	targetPath := filepath.Join(root, "runs", "target", "run.json")
	original, e := os.ReadFile(sourcePath)
	if e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(targetPath)
	if e != nil {
		t.Fatal(e)
	}
	var forged map[string]any
	if e = json.Unmarshal(original, &forged); e != nil {
		t.Fatal(e)
	}
	forged["id"] = "target"
	corrupt, e := json.Marshal(forged)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(sourcePath, corrupt, 0600); e != nil {
		t.Fatal(e)
	}
	q := store.PreEngineRefusalRequest{Phase: "mcp", Code: store.FailureExecutionFailed, Message: "refusal for source", PublishedAt: time.Now().Add(time.Second), Disposition: store.PreEngineRetryPending}
	result, err := s.RecordPreEngineRefusal(ctx, "source", q)
	after, e := os.ReadFile(targetPath)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("record source result=%+v err=%v target-bytes-changed=%v", result, err, !bytes.Equal(before, after))
	if result.Changed || !errors.Is(err, store.ErrInvalidPreEngineRefusal) || !bytes.Equal(before, after) {
		t.Error("recorder accepted mismatched durable run ID and wrote another run")
	}
	// Restoring the input's valid binding must allow its own refusal.
	if e = os.WriteFile(sourcePath, original, 0600); e != nil {
		t.Fatal(e)
	}
	result, err = s.RecordPreEngineRefusal(ctx, "source", q)
	if err != nil || !result.Changed {
		t.Fatal("valid binding refused", result, err)
	}
}
