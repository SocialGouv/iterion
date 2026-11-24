package dispatcher

import (
	"context"
	"fmt"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewR53DispatcherPersistsOriginalLog(t *testing.T) {
	for _, protocol := range []store.RunDataProtocol{0, 1} {
		for _, format := range []store.ValueFormat{1, 2} {
			for _, resume := range []bool{true} {
				t.Run(fmt.Sprintf("p%d/f%d/resume%v", protocol, format, resume), func(t *testing.T) {
					t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
					if protocol == 1 {
						if e := store.RequireRunDataStorage(protocol); e != nil {
							t.Skip("admission OFF")
						}
					}
					ctx := store.WithIdentity(context.Background(), "log-team", "log-owner")
					d := t.TempDir()
					bot := filepath.Join(d, "main.bot")
					if e := os.WriteFile(bot, []byte(`tool r53_log_marker:
  command: `+"`"+`printf '{}'`+"`"+`
workflow main:
  worktree: none
  sandbox: none
  entry: r53_log_marker
  r53_log_marker -> done
`), 0600); e != nil {
						t.Fatal(e)
					}
					sd := filepath.Join(d, "state")
					s, e := store.New(sd)
					if e != nil {
						t.Fatal(e)
					}
					r, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", WorkflowName: "main", Format: format, DataProtocol: protocol})
					if e != nil {
						t.Fatal(e)
					}
					h := store.RunDataHeader{RunID: r.ID, Format: format, Protocol: protocol, BirthID: r.BirthID}
					b, e := store.CaptureRunDataBinding(h)
					if e != nil {
						t.Fatal(e)
					}
					ctx, e = store.WithRunDataBinding(ctx, b)
					if e != nil {
						t.Fatal(e)
					}
					ctx, e = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: r.ID, Format: format})
					if e != nil {
						t.Fatal(e)
					}
					logPath := filepath.Join(sd, "runs", r.ID, "run.log")
					if protocol == 1 {
						scope, e := store.ResolveIncarnationDataScope(ctx, h)
						if e != nil {
							t.Fatal(e)
						}
						root, e := scope.FilesystemRoot(sd)
						if e != nil {
							t.Fatal(e)
						}
						logPath = filepath.Join(root, "run.log")
					}
					if _, e = os.Stat(logPath); !os.IsNotExist(e) {
						t.Fatal("fixture has preexisting log", e)
					}
					r.Status = store.RunStatusPausedOperator
					r.Checkpoint = &store.Checkpoint{NodeID: "r53_log_marker"}
					if e = s.SaveRun(ctx, r); e != nil {
						t.Fatal(e)
					}
					runner, e := NewEngineRunner(bot, iterlog.New(iterlog.LevelInfo, os.Stderr))
					if e != nil {
						t.Fatal(e)
					}
					defer runner.Close()
					e = runner.Dispatch(ctx, DispatchSpec{RunID: r.ID, ResumeFromRunID: r.ID, WorkspacePath: d, StoreDir: sd, Issue: &IssueRef{ID: "native:r", Identifier: "r", Title: "log proof"}})
					if e != nil {
						t.Fatal("real CLI execution", e)
					}
					after, e := s.LoadRun(ctx, r.ID)
					if e != nil || after.Status != store.RunStatusFinished || after.BirthID != r.BirthID {
						t.Fatal("workflow not original finished", e)
					}
					info, statErr := os.Stat(logPath)
					size, sizeErr := s.RunLogSize(ctx, r.ID)
					raw, readErr := s.ReadRunLogRange(ctx, r.ID, 0, 0)
					t.Logf("log inodePresent=%v size=%d sizeErr=%v readErr=%v containsMarker=%v", info != nil, size, sizeErr, readErr, strings.Contains(string(raw), "r53_log_marker"))
					if statErr != nil || sizeErr != nil || readErr != nil || !strings.Contains(string(raw), "r53_log_marker") {
						t.Errorf("finished CLI lost persisted log: stat=%v size=%d sizeErr=%v readErr=%v", statErr, size, sizeErr, readErr)
					}
				})
			}
		}
	}
}
