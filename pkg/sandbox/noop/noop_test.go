package noop

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestNoopName(t *testing.T) {
	d, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.Name() != "noop" {
		t.Errorf("Name = %q, want noop", d.Name())
	}
}

func TestNoopCapabilitiesAllFalse(t *testing.T) {
	d, _ := New()
	caps := d.Capabilities()
	zero := sandbox.Capabilities{}
	if caps != zero {
		t.Errorf("Capabilities = %+v, want all-false", caps)
	}
}

func TestNoopPrepareInactive(t *testing.T) {
	d, _ := New()
	prepared, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeNone})
	if err != nil {
		t.Fatalf("Prepare(none): %v", err)
	}
	p, ok := prepared.(*Prepared)
	if !ok {
		t.Fatalf("PreparedSpec type = %T, want *Prepared", prepared)
	}
	if p.SkippedReason != "" {
		t.Errorf("SkippedReason for none = %q, want empty", p.SkippedReason)
	}
}

func TestNoopPrepareActiveProducesSkipReason(t *testing.T) {
	d, _ := New()
	prepared, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeAuto})
	if err != nil {
		t.Fatalf("Prepare(auto): %v", err)
	}
	p := prepared.(*Prepared)
	if p.SkippedReason == "" {
		t.Error("expected SkippedReason for active mode, got empty")
	}
	if !strings.Contains(p.SkippedReason, "auto") {
		t.Errorf("SkippedReason should mention the requested mode: %q", p.SkippedReason)
	}
}

func TestNoopPrepareRejectsInvalidSpec(t *testing.T) {
	d, _ := New()
	_, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.Mode("bogus")})
	if err == nil {
		t.Fatal("expected validation error for invalid mode")
	}
}

func TestNoopExecCapturesStdout(t *testing.T) {
	d, _ := New()
	prepared, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeNone})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	run, err := d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Cleanup(context.Background())

	res, err := run.Exec(context.Background(), []string{"sh", "-c", "printf hello"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if string(res.Stdout) != "hello" {
		t.Errorf("Stdout = %q, want %q", string(res.Stdout), "hello")
	}
}

func TestNoopExecPropagatesNonZeroExit(t *testing.T) {
	d, _ := New()
	prepared, _ := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeNone})
	run, _ := d.Start(context.Background(), prepared, sandbox.RunInfo{})
	defer run.Cleanup(context.Background())

	res, err := run.Exec(context.Background(), []string{"sh", "-c", "exit 42"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatalf("Exec returned err for non-zero exit: %v", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", res.ExitCode)
	}
}

func TestConstructorMatchesNew(t *testing.T) {
	a, _ := New()
	b, _ := Constructor()
	if a.Name() != b.Name() {
		t.Errorf("Constructor and New disagree: %q vs %q", a.Name(), b.Name())
	}
}
