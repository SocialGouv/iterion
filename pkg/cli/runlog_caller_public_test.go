package cli_test

import (
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewR53CLIPersistsOriginalLog(t *testing.T) {
	for _, protocol := range []store.RunDataProtocol{0, 1} {
		for _, format := range []store.ValueFormat{1, 2} {
			for _, resume := range []bool{false, true} {
				t.Run(fmt.Sprintf("p%d/f%d/resume%v", protocol, format, resume), func(t *testing.T) {
					hermeticSandbox(t)
					if protocol == 1 {
						if e := store.RequireRunDataStorage(protocol); e != nil {
							t.Skip("admission OFF")
						}
					}
					ctx := store.WithIdentity(context.Background(), "log-team", "log-owner")
					d := t.TempDir()
					bot := writeFixture(t, d, "main.bot", `tool r53_log_marker:
  command: `+"`"+`printf '{}'`+"`"+`
workflow main:
  worktree: none
  sandbox: none
  entry: r53_log_marker
  r53_log_marker -> done
`)
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
					p, _ := newTestPrinter(cli.OutputHuman)
					if resume {
						r.Status = store.RunStatusPausedOperator
						r.Checkpoint = &store.Checkpoint{NodeID: "r53_log_marker"}
						if e = s.SaveRun(ctx, r); e != nil {
							t.Fatal(e)
						}
						e = cli.RunResumeWithFile(ctx, bot, cli.ResumeOptions{RunID: r.ID, StoreDir: sd}, p)
					} else {
						e = cli.RunRun(ctx, cli.RunOptions{File: bot, RunID: r.ID, StoreDir: sd}, p)
					}
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
					if !resume {
						prior := info
						beforeLen := len(raw)
						after.Status = store.RunStatusPausedOperator
						after.Checkpoint = &store.Checkpoint{NodeID: "r53_log_marker"}
						if e = s.SaveRun(ctx, after); e != nil {
							t.Fatal(e)
						}
						if e = cli.RunResumeWithFile(ctx, bot, cli.ResumeOptions{RunID: r.ID, StoreDir: sd}, p); e != nil {
							t.Fatal("append resume", e)
						}
						now, e := os.Stat(logPath)
						if e != nil || !os.SameFile(prior, now) {
							t.Fatal("resume replaced log inode", e)
						}
						both, e := s.ReadRunLogRange(ctx, r.ID, 0, 0)
						if e != nil || len(both) <= beforeLen || !strings.Contains(string(both), "Run started") || !strings.Contains(string(both), "Run resumed") {
							t.Fatal("resume did not append both public phase markers", e)
						}
					}
				})
			}
		}
	}
}
