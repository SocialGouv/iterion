package errtrack_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sentry "github.com/getsentry/sentry-go"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/errtrack"
)

type captureTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (t *captureTransport) Configure(sentry.ClientOptions) {}
func (t *captureTransport) SendEvent(e *sentry.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, e)
}
func (t *captureTransport) Flush(time.Duration) bool              { return true }
func (t *captureTransport) FlushWithContext(context.Context) bool { return true }
func (t *captureTransport) Close()                                {}

// A tool node fails printing a registered secret; the error it returns is
// the one `iterion run` hands to errtrack.CaptureError when it fails. The
// tracker records every link of the chain it can unwrap: none carries the
// value.
func TestANodeErrorCapturedByTheTrackerCarriesNoSecret(t *testing.T) {
	const secret = "hunter2-9f8e7d6c5b4a"
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "creds.txt"), []byte("db_password: "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := secretguard.New([]secretguard.Secret{{Name: "DB", Value: secret}}, secretguard.DefaultConfig())
	exec := model.NewClawExecutor(nil, &ir.Workflow{}, model.WithWorkDir(ws), model.WithSecretGuard(guard))
	_, err := exec.Execute(context.Background(), &ir.ToolNode{BaseNode: ir.BaseNode{ID: "leak"}, Command: "cat creds.txt; exit 3"}, map[string]any{})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("scenario broken: err = %v", err)
	}
	tr := &captureTransport{}
	errtrack.Reset()
	if !errtrack.Init(errtrack.Config{DSN: "https://publickey@localhost/1", Transport: tr}) {
		t.Fatal("init")
	}
	t.Cleanup(func() {
		sentry.CurrentHub().BindClient(nil)
		errtrack.Reset()
	})
	errtrack.CaptureError(err, map[string]any{"command": "run"})
	errtrack.Flush()
	if len(tr.events) != 1 {
		t.Fatalf("events = %d", len(tr.events))
	}
	for i, ex := range tr.events[0].Exception {
		t.Logf("exception %d %s: %q", i, ex.Type, ex.Value)
		if strings.Contains(ex.Value, secret) {
			t.Errorf("the tracker event's exception %d (%s) carries the secret", i, ex.Type)
		}
	}
}
