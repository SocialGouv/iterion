package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// NOTE: The runner's main loop wraps NATS deliveries + a Mongo store +
// the runtime engine. Full coverage of processOne / heartbeat /
// executeRun requires a NATS test broker + Mongo container + a
// recordable executor stub. The tests below cover the standalone bits
// that don't need that scaffolding:
//
//   - logDeliveryErr — log-on-error semantics
//   - New() input validation
//   - Shutdown — no-op when no in-flight run
//   - materializeOAuthCredentials — file permission + path semantics
//   - loadWorkflow — IR decode/compile error paths

// ---------------------------------------------------------------------------
// logDeliveryErr
// ---------------------------------------------------------------------------

func TestLogDeliveryErr_NoOpOnNilError(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelDebug, &buf)
	logDeliveryErr(logger, "ack", "run-x", nil)
	if buf.Len() != 0 {
		t.Errorf("expected no log output on nil err, got %q", buf.String())
	}
}

func TestLogDeliveryErr_LogsOnError(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelDebug, &buf)
	logDeliveryErr(logger, "nak-shutdown", "run-x", errors.New("boom"))
	out := buf.String()
	if !strings.Contains(out, "run-x") || !strings.Contains(out, "boom") || !strings.Contains(out, "nak-shutdown") {
		t.Errorf("expected runID + op + err in log; got %q", out)
	}
}

// ---------------------------------------------------------------------------
// New
// ---------------------------------------------------------------------------

func TestNew_RejectsMissingNATS(t *testing.T) {
	_, err := New(context.Background(), Config{})
	if err == nil || !strings.Contains(err.Error(), "NATS connection") {
		t.Errorf("expected NATS-required err, got %v", err)
	}
}

// Skipped: New() validates Store after NATS, but constructing a
// non-nil *natsq.Conn for the negative-Store path would require a live
// broker — covered indirectly by integration tests once the embedded
// NATS test server lands (tracked alongside the queue/nats tests).

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

func TestShutdown_NoInFlight_NoOp(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.New(iterlog.LevelDebug, nil)}}
	// current is nil; Shutdown returns nil without panicking.
	if err := r.Shutdown(context.Background()); err != nil {
		t.Errorf("expected nil err, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// materializeOAuthCredentials
// ---------------------------------------------------------------------------

func TestMaterializeOAuthCredentials_ClaudeCode_FilePermissions(t *testing.T) {
	dir, fname, err := materializeOAuthCredentials(string(secrets.OAuthKindClaudeCode), []byte(`{"k":"v"}`))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer os.RemoveAll(dir)

	if fname != ".credentials.json" {
		t.Errorf("expected .credentials.json, got %q", fname)
	}

	// Dir must be 0700 so a sibling local user can't read OAuth tokens.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", info.Mode().Perm())
	}

	// File must be 0600.
	finfo, err := os.Stat(filepath.Join(dir, fname))
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if finfo.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", finfo.Mode().Perm())
	}

	// Content must round-trip.
	got, err := os.ReadFile(filepath.Join(dir, fname))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != `{"k":"v"}` {
		t.Errorf("file content = %q, want %q", got, `{"k":"v"}`)
	}
}

func TestMaterializeOAuthCredentials_Codex_DifferentFilename(t *testing.T) {
	dir, fname, err := materializeOAuthCredentials(string(secrets.OAuthKindCodex), []byte("payload"))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer os.RemoveAll(dir)
	if fname != "auth.json" {
		t.Errorf("expected auth.json, got %q", fname)
	}
}

func TestMaterializeOAuthCredentials_UnknownKindRejected(t *testing.T) {
	_, _, err := materializeOAuthCredentials("unknown-kind", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "unknown oauth kind") {
		t.Errorf("expected unknown-kind err, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// loadWorkflow
// ---------------------------------------------------------------------------

func TestLoadWorkflow_RejectsEmptyIR(t *testing.T) {
	_, err := loadWorkflow(&queue.RunMessage{RunID: "r", WorkflowName: "wf"})
	if err == nil || !strings.Contains(err.Error(), "IRCompiled is empty") {
		t.Errorf("expected IRCompiled-empty err, got %v", err)
	}
}

func TestLoadWorkflow_RejectsMalformedIR(t *testing.T) {
	_, err := loadWorkflow(&queue.RunMessage{
		RunID:        "r",
		WorkflowName: "wf",
		IRCompiled:   []byte(`{not valid json`),
	})
	if err == nil || !strings.Contains(err.Error(), "decode IR") {
		t.Errorf("expected decode IR err, got %v", err)
	}
}
