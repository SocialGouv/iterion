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

func TestReviewR53CLILogIOFailureDisposition(t *testing.T) {
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
					if e = os.MkdirAll(logPath, 0700); e != nil {
						t.Fatal(e)
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
					if protocol == 1 {
						if e == nil {
							t.Fatal("strict sink refusal swallowed")
						}
						events, ee := s.LoadEvents(ctx, r.ID)
						if ee != nil {
							t.Fatal(ee)
						}
						for _, event := range events {
							if event.Type == store.EventNodeStarted {
								t.Fatal("engine started after strict log refusal")
							}
						}
						lock, le := s.LockRun(ctx, r.ID)
						if le != nil {
							t.Fatal("lease leaked", le)
						}
						_ = lock.Unlock()
						if _, le = os.Stat(filepath.Join(sd, "runs", r.ID, "run.log")); !os.IsNotExist(le) {
							t.Fatal("strict legacy fallback", le)
						}
						return
					}
					if e != nil {
						t.Fatal("historical ordinary IO became fatal", e)
					}
					after, e := s.LoadRun(ctx, r.ID)
					if e != nil || after.Status != store.RunStatusFinished || after.BirthID != r.BirthID {
						t.Fatal("workflow not original finished", e)
					}
					info, err := os.Stat(logPath)
					if err != nil || !info.IsDir() {
						t.Fatal("sink refusal replaced directory", err)
					}
					_ = strings.Contains("", "")
				})
			}
		}
	}
}
