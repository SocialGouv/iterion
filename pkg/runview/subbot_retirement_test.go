package runview

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type invalidUsageAtChildBirth struct {
	store.RunStore
	failed                 bool
	acknowledgementFailure bool
}

func (s *invalidUsageAtChildBirth) Unwrap() store.RunStore { return s.RunStore }
func (s *invalidUsageAtChildBirth) BeginRunBirth(ctx context.Context, id string) (store.RunBirthReservation, error) {
	birth, err := store.AsRunBirthStore(s.RunStore).BeginRunBirth(ctx, id)
	if err != nil {
		return nil, err
	}
	return &subbotTestBirth{RunBirthReservation: birth, after: s.afterPublish}, nil
}
func (s *invalidUsageAtChildBirth) afterPublish(ctx context.Context, r *store.Run, e error) (*store.Run, error) {
	if e == nil && r.WorkflowName == "control_child" && !s.failed {
		s.failed = true
		if s.acknowledgementFailure {
			return r, errors.New("publication acknowledgement failure")
		}
		if setErr := os.Setenv("ITERION_USAGE_CAP_5H_PCT", "not-a-number"); setErr != nil {
			return r, setErr
		}
	}
	return r, e
}
func TestServiceResumeAfterChildBuilderFailure(t *testing.T) {
	testServiceResumeAfterChildSetupFailure(t, false)
}
func TestServiceResumeAfterChildPublicationFailure(t *testing.T) {
	testServiceResumeAfterChildSetupFailure(t, true)
}
func testServiceResumeAfterChildSetupFailure(t *testing.T, acknowledgementFailure bool) {
	t.Helper()
	expectedCause := "not-a-number"
	if acknowledgementFailure {
		expectedCause = "publication acknowledgement failure"
	}

	t.Setenv("ITERION_USAGE_CAP", "")
	t.Setenv("ITERION_USAGE_CAP_5H_PCT", "0")
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.bot")
	if e := os.WriteFile(parent, []byte(subbotControlParent), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "control_child.bot"), []byte(strings.ReplaceAll(subbotControlChild, "sleep 30; ", "")), 0600); e != nil {
		t.Fatal(e)
	}
	svc, e := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { stopService(t, svc) })
	svc.store = &invalidUsageAtChildBirth{RunStore: svc.store, acknowledgementFailure: acknowledgementFailure}
	svc.recoveryDispatch = func(context.Context, error, func(runtime.ErrorCode) int) (runtime.RecoveryAction, runtime.ErrorCode) {
		return runtime.RecoveryAction{Kind: runtime.RecoveryFailTerminal}, runtime.ErrCodeExecutionFailed
	}
	first, e := svc.Launch(t.Context(), LaunchSpec{FilePath: parent})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = svc.Cancel(first.RunID); awaitRunCompletion(t, first.Done, "child test run cleanup") })
	awaitRunCompletion(t, first.Done, "first setup failure failed to end")
	t.Setenv("ITERION_USAGE_CAP_5H_PCT", "0")
	failed, fe := svc.store.LoadRun(t.Context(), first.RunID)
	if fe != nil {
		t.Fatal(fe)
	}
	t.Logf("actual parent builder failure=%s", failed.Error)
	if acknowledgementFailure && len(failed.SubbotChildren) != 0 {
		t.Fatalf("unacknowledged child linked: %v", failed.SubbotChildren)
	}
	ids, e := svc.store.ListChildRuns(t.Context(), first.RunID)
	if e != nil || len(ids) != 1 {
		t.Fatalf("children=%v err=%v", ids, e)
	}
	childID := ids[0]
	child, e := svc.store.LoadRun(t.Context(), childID)
	if e != nil {
		t.Fatal(e)
	}
	child.CreatedAt = time.Now().Add(-time.Hour)
	if e = svc.store.SaveRun(t.Context(), child); e != nil {
		t.Fatal(e)
	}
	svc.reconcileOrphans(t.Context())
	child, e = svc.store.LoadRun(t.Context(), childID)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("after setup/reaper child=%s active=%v", child.Status, svc.manager.Active(childID))
	if child.Status != store.RunStatusFailed || !strings.Contains(child.Error, expectedCause) || svc.manager.Active(childID) {
		t.Fatalf("setup failure not retired accurately: %+v", child)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resumed, e := svc.Resume(ctx, ResumeSpec{RunID: first.RunID, FilePath: parent})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = svc.Cancel(resumed.RunID); awaitRunCompletion(t, resumed.Done, "child test run cleanup") })
	awaitRunCompletion(t, resumed.Done, "bounded public resume failed to end")
	after, e := svc.store.LoadRun(t.Context(), childID)
	if e != nil {
		t.Fatal(e)
	}
	all, e := svc.store.ListChildRuns(t.Context(), first.RunID)
	if e != nil {
		t.Fatal(e)
	}
	parentAfter, e := svc.store.LoadRun(t.Context(), first.RunID)
	if e != nil {
		t.Fatal(e)
	}
	if after.Status != store.RunStatusFailed || len(all) != 2 || parentAfter.Status != store.RunStatusFinished || ctx.Err() != nil {
		t.Fatalf("retry failed old=%s children=%v parent=%s ctx=%v", after.Status, all, parentAfter.Status, ctx.Err())
	}
}

func TestServiceChildExecutionFailureKeepsCause(t *testing.T) {
	t.Setenv("ITERION_USAGE_CAP", "")
	t.Setenv("ITERION_USAGE_CAP_5H_PCT", "0")
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.bot")
	if e := os.WriteFile(parent, []byte(subbotControlParent), 0600); e != nil {
		t.Fatal(e)
	}
	child := strings.ReplaceAll(subbotControlChild, "sleep 30; printf '{\"done\":true}'", "echo child-business-failure >&2; exit 1")
	if e := os.WriteFile(filepath.Join(dir, "control_child.bot"), []byte(child), 0600); e != nil {
		t.Fatal(e)
	}
	svc, e := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { stopService(t, svc) })
	svc.recoveryDispatch = func(context.Context, error, func(runtime.ErrorCode) int) (runtime.RecoveryAction, runtime.ErrorCode) {
		return runtime.RecoveryAction{Kind: runtime.RecoveryFailTerminal}, runtime.ErrCodeExecutionFailed
	}
	launched, e := svc.Launch(t.Context(), LaunchSpec{FilePath: parent})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = svc.Cancel(launched.RunID); awaitRunCompletion(t, launched.Done, "child test run cleanup") })
	awaitRunCompletion(t, launched.Done, "child execution failure failed to end")
	r, e := svc.store.LoadRun(t.Context(), launched.RunID)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("parent error: %s", r.Error)
	if !strings.Contains(r.Error, "child-business-failure") {
		t.Fatalf("execution cause replaced: %s", r.Error)
	}
}
