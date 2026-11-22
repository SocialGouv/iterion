package runview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/scripthost"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The run doc is the only carrier a resume surface that cannot re-state an
// override has (the studio's answers, the retry sweeper): the launch stamped
// its codemode choice on the doc, and resumeExecutorSpec replays it into the
// executor spec. A replay gone deaf here re-arms a capability the operator
// killed at launch — the fail-open direction (CM-5e, #3345; the hole the
// adversarial round graded high).
func TestTheRunDocReplaysTheCodemodeOverrideOnResume(t *testing.T) {
	t.Parallel()
	svc := &Service{workDir: "/service-root"}
	wf := &ir.Workflow{Name: "resume-codemode"}
	bounds := &scripthost.Limits{ToolCalls: 4096, Wall: 90 * time.Second}
	spec := svc.resumeExecutorSpec(wf, &store.Run{
		CodemodeOverride:       "off",
		CodemodeBoundsOverride: bounds,
	}, nil, "")
	if spec.Codemode != "off" {
		t.Errorf("replayed Codemode = %q, want the doc's %q — the resume would run the authored value", spec.Codemode, "off")
	}
	if spec.CodemodeBounds == nil || spec.CodemodeBounds.ToolCalls != 4096 || spec.CodemodeBounds.Wall != 90*time.Second {
		t.Errorf("replayed CodemodeBounds = %+v, want the doc's raise", spec.CodemodeBounds)
	}

	// A run launched without an override resumes without one — the replay
	// must not conjure a choice nobody made.
	plain := svc.resumeExecutorSpec(wf, &store.Run{}, nil, "")
	if plain.Codemode != "" || plain.CodemodeBounds != nil {
		t.Errorf("a doc with no override replayed Codemode=%q bounds=%+v — inherit must read absent", plain.Codemode, plain.CodemodeBounds)
	}
}

func TestResumeCodemodeQuotaSurfaces(t *testing.T) {
	for _, detached := range []bool{false, true} {
		name := "in process"
		if detached {
			name = "detached flags"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			botPath := filepath.Join(dir, "quota.bot")
			if err := os.WriteFile(botPath, []byte(resumeBudgetInProcBot), 0o600); err != nil {
				t.Fatal(err)
			}
			argsFile := filepath.Join(dir, "args")
			if detached {
				binary := filepath.Join(dir, "iterion")
				if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ITERION_TEST_ARGS\"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv(envDetached, "1")
				t.Setenv("ITERION_BIN", binary)
				t.Setenv("ITERION_TEST_ARGS", argsFile)
			} else {
				t.Setenv(envDetached, "0")
			}
			svc, err := NewService(dir, WithLogger(iterlog.Nop()))
			if err != nil {
				t.Fatal(err)
			}
			_, hash, err := CompileWorkflowWithHash(botPath, "", store.RunTrustDefault)
			if err != nil {
				t.Fatal(err)
			}
			const id = "resume-quota-surface"
			seedPausedOperatorRun(t, svc, id, hash)
			doc, err := svc.store.LoadRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			doc.CodemodeBoundsOverride = &ir.CodemodeBounds{StoreEntries: 12, StoreBytes: 500, ToolCalls: 17}
			if err := svc.store.SaveRun(ctx, doc); err != nil {
				t.Fatal(err)
			}
			result, err := svc.Resume(ctx, ResumeSpec{RunID: id, FilePath: botPath, CodemodeBounds: &ir.CodemodeBounds{StoreBytes: 800}})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-result.Done:
			case <-runWaitContext(t).Done():
				t.Fatal("resume did not terminate")
			}
			if detached {
				args, err := os.ReadFile(argsFile)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(args), "--codemode-store-bytes\n800\n") || strings.Contains(string(args), "--codemode-store-entries\n") || strings.Contains(string(args), "--codemode-tool-calls\n") {
					t.Fatalf("detached args = %q, want only the restated bytes flag", args)
				}
				return
			}
			doc, err = svc.store.LoadRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got := doc.CodemodeBoundsOverride; doc.Status != store.RunStatusFinished || got == nil || got.StoreEntries != 12 || got.StoreBytes != 800 || got.ToolCalls != 17 {
				t.Fatalf("resume status = %s; bounds = %+v, want finished with unchanged entries and tool calls, updated bytes", doc.Status, got)
			}
		})
	}
}
