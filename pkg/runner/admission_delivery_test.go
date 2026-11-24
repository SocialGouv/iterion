package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/notify"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/nats-io/nats.go/jetstream"
)

type admissionConcurrentStore struct {
	store.RunStore
	target                        store.RunStatus
	attempts, conflicts, gitSaves int
	snapshot                      *store.Run
}

func (s *admissionConcurrentStore) Unwrap() store.RunStore { return s.RunStore }
func (s *admissionConcurrentStore) SaveRun(ctx context.Context, row *store.Run) error {
	if row.Admission != nil {
		s.attempts++
		if s.attempts == 1 && s.target != "" {
			fresh, err := s.LoadRun(ctx, row.ID)
			if err != nil {
				return err
			}
			fresh.Status = s.target
			fresh.Error = "concurrent operator state"
			if err := s.RunStore.SaveRun(ctx, fresh); err != nil {
				return err
			}
			s.snapshot, err = s.LoadRun(ctx, row.ID)
			if err != nil {
				return err
			}
		}
	}
	err := s.RunStore.SaveRun(ctx, row)
	if errors.Is(err, store.ErrRunConflict) {
		s.conflicts++
	}
	return err
}
func (s *admissionConcurrentStore) SaveRunGitMeta(ctx context.Context, id string, m *store.RunGitMeta) error {
	s.gitSaves++
	return store.AsRunGitMetaStore(s.RunStore).SaveRunGitMeta(ctx, id, m)
}
func (s *admissionConcurrentStore) LoadRunGitMeta(ctx context.Context, id string) (*store.RunGitMeta, error) {
	return store.AsRunGitMetaStore(s.RunStore).LoadRunGitMeta(ctx, id)
}
func admissionDeliveryFixture(t *testing.T, fs store.RunStore, resume bool) (*queue.RunMessage, string) {
	t.Helper()
	ctx := store.WithIdentity(t.Context(), "team-1", "u1")
	runID := fmt.Sprintf("admission-%d", time.Now().UnixNano())
	marker := filepath.Join(t.TempDir(), "executed")
	source := fmt.Sprintf("tool probe:\n  command: `printf ran > %s`\n\nworkflow main:\n  worktree: none\n  sandbox: none\n  entry: probe\n  probe -> done\n", marker)
	parsed := parsertest.Parse("main.bot", source)
	body, err := ast.MarshalFile(parsed.File)
	if err != nil {
		t.Fatal(err)
	}
	row, err := fs.CreateRun(ctx, runID, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	row.Status = store.RunStatusQueued
	row.TenantID = "team-1"
	row.OwnerID = "u1"
	row.QueuedAt = &now
	if resume {
		row.Checkpoint = &store.Checkpoint{NodeID: "probe"}
	}
	if err := fs.SaveRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	msg := &queue.RunMessage{V: queue.SchemaVersion, RunID: runID, WorkflowName: "main", IRCompiled: body, TenantID: "team-1", OwnerID: "u1", PublishedAtRFC: now.Add(time.Millisecond).Format(time.RFC3339Nano)}
	if resume {
		msg.Resume = &queue.ResumeSpec{PriorStatus: store.RunStatusFailedResumable}
	}
	return msg, marker
}
func assertAdmissionDeliveryStopped(t *testing.T, s *admissionConcurrentStore, id, marker string) {
	t.Helper()
	if s.conflicts != 1 || s.attempts != 1 || s.snapshot == nil {
		t.Fatalf("CAS not reached exactly once: conflicts=%d attempts=%d snapshot=%v", s.conflicts, s.attempts, s.snapshot)
	}
	after, err := s.LoadRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != s.target || after.Error != "concurrent operator state" || after.Admission != nil {
		t.Fatalf("concurrent state overwritten: status=%s error=%q admission=%+v", after.Status, after.Error, after.Admission)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workflow executed despite CAS stop: %v", err)
	}
}

type admissionOutcomeBus struct {
	eventbus.Bus
	published atomic.Int64
}

func (b *admissionOutcomeBus) Publish(ctx context.Context, ev trigger.Event) error {
	b.published.Add(1)
	return b.Bus.Publish(ctx, ev)
}

func TestProcessOneAdmissionCASStops(t *testing.T) {
	for _, resume := range []bool{false, true} {
		targets := []store.RunStatus{store.RunStatusCancelled, store.RunStatusFinished, store.RunStatusQueued}
		if resume {
			targets[2] = store.RunStatusRunning
		}
		for _, target := range targets {
			t.Run(fmt.Sprintf("resume=%v/%s", resume, target), func(t *testing.T) {
				conn, r, fs := windowConn(t)
				msg, marker := admissionDeliveryFixture(t, fs, resume)
				wrapped := &admissionConcurrentStore{RunStore: fs, target: target}
				r.cfg.Store = wrapped
				pool := newPoolHarnessForRun(t, credpool.Limits{MaxUSDPerDay: 10, MaxConcurrentRuns: 1}, msg.RunID)
				r.cfg.CredPool = pool.broker
				bus := &admissionOutcomeBus{Bus: eventbus.NewInProcBus(iterlog.Nop())}
				r.cfg.Events = bus
				var callbacks atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { callbacks.Add(1); w.WriteHeader(http.StatusNoContent) }))
				defer server.Close()
				row, err := fs.LoadRun(t.Context(), msg.RunID)
				if err != nil {
					t.Fatal(err)
				}
				row.CallbackURL = server.URL
				if err := fs.SaveRun(t.Context(), row); err != nil {
					t.Fatal(err)
				}
				r.completionNotifier = notify.New(iterlog.Nop(), time.Second, notify.WithAllowPrivate(true))
				payload, err := json.Marshal(msg)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := conn.JetStream().Publish(t.Context(), natsq.SubjectRuns, payload, jetstream.WithMsgID(msg.RunID)); err != nil {
					t.Fatal(err)
				}
				cons, err := conn.NewConsumer(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				d, err := cons.Fetch(t.Context(), 3*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				r.processOne(t.Context(), d)
				assertAdmissionDeliveryStopped(t, wrapped, msg.RunID, marker)
				name, err := conn.JetStream().StreamNameBySubject(t.Context(), natsq.SubjectRuns)
				if err != nil {
					t.Fatal(err)
				}
				stream, err := conn.JetStream().Stream(t.Context(), name)
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(time.Second)
				var pending uint64
				for {
					info, err := stream.Info(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					pending = info.State.Msgs
					if pending == 0 || time.Now().After(deadline) {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if pending != 0 {
					t.Errorf("stopped delivery not Acked: stream has %d message(s)", pending)
				}
				if depth, err := conn.DLQDepth(t.Context()); err != nil || depth != 0 {
					t.Errorf("unexpected DLQ: depth=%d err=%v", depth, err)
				}
				if callbacks.Load() != 0 || bus.published.Load() != 0 {
					t.Errorf("outcome emitted despite no execution: callback=%d events=%d", callbacks.Load(), bus.published.Load())
				}
				leases, err := pool.leases.ListByDonor(t.Context(), "donor", 10)
				if err != nil || len(leases) != 1 || leases[0].Closed {
					t.Errorf("lease not preserved: leases=%+v err=%v", leases, err)
				}
				day, _, err := pool.ledger.Usage(t.Context(), credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code", ""), time.Now().UTC())
				if err != nil || day.CostUSD != 0 {
					t.Errorf("donor accounting: %+v err=%v", day, err)
				}
				t.Logf("real CAS conflicts=%d; status=%s; pending=%d; callbacks=%d events=%d; lease closed=%v", wrapped.conflicts, target, pending, callbacks.Load(), bus.published.Load(), len(leases) > 0 && leases[0].Closed)
				// Positive observer controls run after the no-outcome assertions.
				if target == store.RunStatusFinished {
					r.fireCompletionNotifier(msg)
					if callbacks.Load() != 1 {
						t.Fatal("completion callback oracle was inert")
					}
				}
				r.fireOutcomeEvent(msg, nil)
				if bus.published.Load() != 1 {
					t.Fatal("event oracle was inert")
				}

			})
		}
	}
}

func TestExecuteRunAdmissionCASPreservesGit(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprintf("stop=%v", stop), func(t *testing.T) {
			t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
			t.Setenv("ITERION_RUNNER_CLONE_ALLOW_PRIVATE", "1")
			root := t.TempDir()
			origin := filepath.Join(root, "origin.git")
			seed := filepath.Join(root, "seed")
			gitOut(t, root, "init", "--bare", origin)
			gitOut(t, root, "clone", origin, seed)
			gitOut(t, seed, "config", "user.name", "admission")
			gitOut(t, seed, "config", "user.email", "admission@example.invalid")
			gitOut(t, seed, "commit", "--allow-empty", "-m", "baseline")
			gitOut(t, seed, "push", "origin", "HEAD")
			fakeURL := "https://127.0.0.1/admission.git"
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", "url.file://"+origin+".insteadOf")
			t.Setenv("GIT_CONFIG_VALUE_0", fakeURL)
			oldHosts := runnerHostsFile
			runnerHostsFile = filepath.Join(root, "hosts")
			if err := os.WriteFile(runnerHostsFile, []byte("127.0.0.1 localhost\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { runnerHostsFile = oldHosts })
			fs, err := store.New(filepath.Join(root, "store"))
			if err != nil {
				t.Fatal(err)
			}
			msg, marker := admissionDeliveryFixture(t, fs, false)
			msg.RepoURL = fakeURL
			wrapped := &admissionConcurrentStore{RunStore: fs}
			if stop {
				wrapped.target = store.RunStatusCancelled
			}
			initial := &store.RunGitMeta{HeadCommit: "concurrent-snapshot", BaseCommit: "kept-base", UpdatedAt: time.Unix(1, 0).UTC()}
			if err := store.AsRunGitMetaStore(fs).SaveRunGitMeta(t.Context(), msg.RunID, initial); err != nil {
				t.Fatal(err)
			}
			r := &Runner{cfg: Config{Store: wrapped, WorkDir: filepath.Join(root, "pod"), Logger: iterlog.Nop()}}
			var usage *metricsEmitter
			runErr := r.executeRun(store.WithIdentity(t.Context(), "team-1", "u1"), msg, &usage, nil)
			actual, err := store.AsRunGitMetaStore(fs).LoadRunGitMeta(t.Context(), msg.RunID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("executeRun error=%v conflicts=%d saves=%d snapshot=%+v usage=%v", runErr, wrapped.conflicts, wrapped.gitSaves, actual, usage != nil)
			if stop {
				if !errors.Is(runErr, runtime.ErrAdmissionStopped) {
					t.Fatalf("expected native admission stop, got %v", runErr)
				}
				assertAdmissionDeliveryStopped(t, wrapped, msg.RunID, marker)
				if wrapped.gitSaves != 0 || actual.HeadCommit != initial.HeadCommit || !actual.UpdatedAt.Equal(initial.UpdatedAt) {
					t.Fatal("stopped admission finalized/replaced git snapshot")
				}
			} else {
				if runErr != nil {
					t.Fatal(runErr)
				}
				if wrapped.gitSaves == 0 || actual.HeadCommit == initial.HeadCommit {
					t.Fatal("positive control never finalized git snapshot")
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("positive control workflow did not execute: %v", err)
				}
			}
		})
	}
}
