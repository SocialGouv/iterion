package runview

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

type crashBeforeHandoffStore struct {
	store.RunStore
	ready         string
	beforePublish bool
}

func (s *crashBeforeHandoffStore) Unwrap() store.RunStore { return s.RunStore }
func (s *crashBeforeHandoffStore) BeginRunBirth(ctx context.Context, id string) (store.RunBirthReservation, error) {
	birth, err := store.AsRunBirthStore(s.RunStore).BeginRunBirth(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.beforePublish {
		if err := os.WriteFile(s.ready, []byte(id), 0600); err != nil {
			_ = birth.Unlock()
			return nil, err
		}
		select {}
	}
	return &subbotTestBirth{RunBirthReservation: birth, after: s.afterPublish}, nil
}
func (s *crashBeforeHandoffStore) afterPublish(ctx context.Context, r *store.Run, e error) (*store.Run, error) {
	if e == nil && r.WorkflowName == "control_child" {
		if e = os.WriteFile(s.ready, []byte(r.ID), 0600); e != nil {
			return r, e
		}
		select {}
	}
	return r, e
}
func TestSubbotCrashProcessHelper(t *testing.T) {
	dir := os.Getenv("ITERION_TEST_SUBBOT_CRASH_DIR")
	if dir == "" {
		t.Skip("subprocess")
	}
	svc, e := NewService(filepath.Join(dir, "store"), WithLogger(iterlog.Nop()), WithWorkDir(os.Getenv("ITERION_TEST_SUBBOT_CRASH_REPO")))
	if e != nil {
		t.Fatal(e)
	}
	svc.store = &crashBeforeHandoffStore{RunStore: svc.store, ready: filepath.Join(dir, "ready"), beforePublish: os.Getenv("ITERION_TEST_SUBBOT_BEFORE_PUBLISH") == "1"}
	res, e := svc.Launch(t.Context(), LaunchSpec{FilePath: filepath.Join(dir, "parent.bot")})
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "parent-id"), []byte(res.RunID), 0600); e != nil {
		t.Fatal(e)
	}
	<-res.Done
	t.Fatal("child should block before handoff")
}
func awaitCrashWitnessFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, e := os.ReadFile(path)
		if e == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("never produced " + path)
	return ""
}
func TestSubbotCrashBeforeHandoffResumesWithNewChild(t *testing.T) {
	t.Setenv("ITERION_USAGE_CAP", "")
	t.Setenv("ITERION_USAGE_CAP_5H_PCT", "0")
	dir := t.TempDir()
	repo := gittest.SourceRepo(t)
	if e := os.WriteFile(filepath.Join(dir, "parent.bot"), []byte(subbotControlParent), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "control_child.bot"), []byte(strings.ReplaceAll(subbotControlChild, "sleep 30; ", "")), 0600); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSubbotCrashProcessHelper$")
	cmd.Env = append(os.Environ(), "ITERION_TEST_SUBBOT_CRASH_DIR="+dir, "ITERION_TEST_SUBBOT_CRASH_REPO="+repo)
	log, e := os.Create(filepath.Join(dir, "helper.log"))
	if e != nil {
		t.Fatal(e)
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	var waitOnce sync.Once
	var waitErr error
	waitProcess := func() error { waitOnce.Do(func() { waitErr = cmd.Wait() }); return waitErr }
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = waitProcess() })
	parentID := awaitCrashWitnessFile(t, filepath.Join(dir, "parent-id"))
	childID := awaitCrashWitnessFile(t, filepath.Join(dir, "ready"))
	peer, e := NewService(filepath.Join(dir, "store"), WithLogger(iterlog.Nop()), WithWorkDir(repo))
	if e != nil {
		t.Fatal(e)
	}
	defer stopService(t, peer)
	for _, id := range []string{parentID, childID} {
		r, e := peer.store.LoadRun(t.Context(), id)
		if e != nil {
			t.Fatal(e)
		}
		r.CreatedAt = time.Now().Add(-time.Hour)
		if e = peer.store.SaveRun(t.Context(), r); e != nil {
			t.Fatal(e)
		}
	}
	peer.reconcileOrphans(t.Context())
	live, e := peer.store.LoadRun(t.Context(), childID)
	if e != nil {
		t.Fatal(e)
	}
	if live.Status != store.RunStatusRunning {
		t.Fatalf("peer reaped live preparation: %s", live.Status)
	}
	if lock, e := peer.store.LockRun(t.Context(), childID); e == nil {
		lock.Unlock()
		t.Fatal("child birth exposed without lock")
	}
	if e = cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	e = waitProcess()
	var exited *exec.ExitError
	if !errors.As(e, &exited) {
		t.Fatalf("not actually killed: %v", e)
	}
	peer.reconcileOrphans(t.Context())
	dead, e := peer.store.LoadRun(t.Context(), childID)
	if e != nil {
		t.Fatal(e)
	}
	if dead.Status != store.RunStatusFailed {
		t.Fatalf("crashed preparation not reaped: %s", dead.Status)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	res, e := peer.Resume(ctx, ResumeSpec{RunID: parentID, FilePath: filepath.Join(dir, "parent.bot")})
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		cancel()
		_ = peer.Cancel(res.RunID)
		awaitRunCompletion(t, res.Done, "resumed run cleanup failed")
	}()
	awaitRunCompletion(t, res.Done, "resume failed to finish")
	parent, e := peer.store.LoadRun(t.Context(), parentID)
	if e != nil {
		t.Fatal(e)
	}
	children, e := peer.store.ListChildRuns(t.Context(), parentID)
	if e != nil {
		t.Fatal(e)
	}
	if parent.Status != store.RunStatusFinished || len(children) != 2 {
		t.Fatalf("resume parent=%s children=%v error=%s", parent.Status, children, parent.Error)
	}
	t.Logf("live birth protected; killed process reaped; public Resume finished with %d children", len(children))
}

type subbotUnlockBarrierStore struct {
	store.RunStore
	id      string
	entered chan struct{}
	release chan struct{}
}

func (s *subbotUnlockBarrierStore) Unwrap() store.RunStore { return s.RunStore }
func (s *subbotUnlockBarrierStore) BeginRunBirth(ctx context.Context, id string) (store.RunBirthReservation, error) {
	l, e := store.AsRunBirthStore(s.RunStore).BeginRunBirth(ctx, id)
	if e != nil {
		return nil, e
	}
	s.id = id
	return &subbotUnlockBarrierLock{RunBirthReservation: l, s: s}, nil
}

type subbotUnlockBarrierLock struct {
	store.RunBirthReservation
	s    *subbotUnlockBarrierStore
	once sync.Once
}

func (l *subbotUnlockBarrierLock) Unlock() error {
	l.once.Do(func() { close(l.s.entered); <-l.s.release })
	return l.RunBirthReservation.Unlock()
}
func TestSubbotLeaseReleasePrecedesManagerHandoff(t *testing.T) {
	t.Setenv("ITERION_USAGE_CAP", "")
	t.Setenv("ITERION_USAGE_CAP_5H_PCT", "0")
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.bot")
	if e := os.WriteFile(parent, []byte(subbotControlParent), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "control_child.bot"), []byte(subbotControlLoopChild), 0600); e != nil {
		t.Fatal(e)
	}
	svc, e := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if e != nil {
		t.Fatal(e)
	}
	defer stopService(t, svc)
	barrier := &subbotUnlockBarrierStore{RunStore: svc.store, entered: make(chan struct{}), release: make(chan struct{})}
	svc.store = barrier
	var release sync.Once
	defer release.Do(func() { close(barrier.release) })
	res, e := svc.Launch(t.Context(), LaunchSpec{FilePath: parent})
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		release.Do(func() { close(barrier.release) })
		_ = svc.Cancel(res.RunID)
		awaitRunCompletion(t, res.Done, "parent cleanup failed")
	}()
	child := waitForActiveChild(t, svc, res.RunID)
	if e = svc.Pause(child); e != nil {
		t.Fatal(e)
	}
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("unlock never reached")
	}
	if !svc.manager.Active(child) {
		t.Fatal("manager released before lease unlock completed")
	}
	release.Do(func() { close(barrier.release) })
	deadline := time.Now().Add(5 * time.Second)
	for svc.manager.Active(child) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if svc.manager.Active(child) {
		t.Fatal("manager not released")
	}
	p, e := svc.store.LoadRun(t.Context(), res.RunID)
	if e != nil {
		t.Fatal(e)
	}
	if p.Status != store.RunStatusRunning {
		t.Fatalf("parent not parked: %s", p.Status)
	}
	if e = svc.Cancel(res.RunID); e != nil {
		t.Fatal(e)
	}
	awaitRunCompletion(t, res.Done, "park ignored parent cancel")
}
func TestSubbotManagerRefusalPreventsBirth(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.bot")
	if e := os.WriteFile(parent, []byte(subbotControlParent), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "control_child.bot"), []byte(subbotControlChild), 0600); e != nil {
		t.Fatal(e)
	}
	svc, e := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if e != nil {
		t.Fatal(e)
	}
	defer stopService(t, svc)
	if _, e = svc.store.CreateRun(t.Context(), "parent", "parent", nil); e != nil {
		t.Fatal(e)
	}
	ctx, e := store.WithRunValueFormat(t.Context(), store.RunValueFormat{RunID: "parent", Format: store.LegacyValueFormat})
	if e != nil {
		t.Fatal(e)
	}
	svc.manager.Stop(t.Context())
	_, e = svc.subbotRunnerFor(parent, iterlog.Nop())(ctx, runtime.SubbotRequest{ParentRunID: "parent", NodeID: "child", ReattachKey: "child", Source: "control_child.bot"})
	if !errors.Is(e, ErrSubbotLivenessUnavailable) {
		t.Fatalf("manager refusal is not typed: %v", e)
	}
	t.Logf("manager refusal: %v", e)
	for _, team := range []string{"unrelated-team", "parent-team"} {
		ids, err := store.AsTeamAliveRunsReader(svc.store).ListAliveRunsForTeam(context.Background(), team)
		if err != nil || len(ids) != 0 {
			t.Fatalf("manager refusal polluted team %s: %v %v", team, ids, err)
		}
	}
	ids, e := svc.store.ListChildRuns(ctx, "parent")
	if e != nil || len(ids) != 0 {
		t.Fatalf("child born: %v %v", ids, e)
	}
	p, e := svc.store.LoadRun(ctx, "parent")
	if e != nil {
		t.Fatal(e)
	}
	if len(p.SubbotChildren) != 0 {
		t.Fatal("parent linked before manager admission")
	}
}

// subbotTestBirth injects the same post-publication faults as the old creator
// fixtures while retaining the real reservation's ownership and release.
type subbotTestBirth struct {
	store.RunBirthReservation
	after func(context.Context, *store.Run, error) (*store.Run, error)
}

func (b *subbotTestBirth) Publish(ctx context.Context, spec store.RunCreateSpec) (*store.Run, error) {
	r, err := b.RunBirthReservation.Publish(ctx, spec)
	return b.after(ctx, r, err)
}

func TestSubbotCrashBeforePublicationLeavesNoShell(t *testing.T) {
	t.Setenv("ITERION_USAGE_CAP", "")
	t.Setenv("ITERION_USAGE_CAP_5H_PCT", "0")
	dir := t.TempDir()
	repo := gittest.SourceRepo(t)
	for name, body := range map[string]string{"parent.bot": subbotControlParent, "control_child.bot": subbotControlChild} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSubbotCrashProcessHelper$")
	cmd.Env = append(os.Environ(), "ITERION_TEST_SUBBOT_CRASH_DIR="+dir, "ITERION_TEST_SUBBOT_CRASH_REPO="+repo, "ITERION_TEST_SUBBOT_BEFORE_PUBLISH=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	wait := func() { once.Do(func() { _ = cmd.Wait() }) }
	t.Cleanup(func() { _ = cmd.Process.Kill(); wait() })
	childID := awaitCrashWitnessFile(t, filepath.Join(dir, "ready"))
	peer, err := NewService(filepath.Join(dir, "store"), WithLogger(iterlog.Nop()), WithWorkDir(repo))
	if err != nil {
		t.Fatal(err)
	}
	defer stopService(t, peer)
	for _, phase := range []string{"preparing", "killed"} {
		if phase == "killed" {
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			wait()
		}
		if ids, e := store.AsTeamAliveRunsReader(peer.store).ListAliveRunsForTeam(context.Background(), "unrelated-team"); e != nil || len(ids) != 0 {
			t.Fatalf("%s polluted unrelated team: %v %v", phase, ids, e)
		}
		if _, err := peer.store.LoadRun(t.Context(), childID); !errors.Is(err, store.ErrRunNotFound) {
			t.Fatalf("%s child visible: %v", phase, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "store", "runs", childID)); !os.IsNotExist(err) {
			t.Fatalf("%s published a shell: %v", phase, err)
		}
	}
}
