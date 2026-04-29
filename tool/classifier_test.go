package tool

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/permissions"
)

type stubClassifier struct {
	decision permissions.Decision
	err      error
	calls    int
}

func (s *stubClassifier) Classify(_ context.Context, _ string, _ map[string]any) (permissions.Decision, error) {
	s.calls++
	return s.decision, s.err
}

type denyAllChecker struct{}

func (denyAllChecker) CheckContext(PolicyContext) error {
	return errors.New("base denies everything")
}

type allowAllChecker struct{}

func (allowAllChecker) CheckContext(PolicyContext) error { return nil }

func TestClassifierCheckerAllowSkipsBase(t *testing.T) {
	cc := &ClassifierChecker{
		Classifier: &stubClassifier{decision: permissions.DecisionAllow},
		Base:       denyAllChecker{},
	}
	if err := cc.CheckContext(PolicyContext{ToolName: "bash"}); err != nil {
		t.Errorf("expected Allow to skip base, got %v", err)
	}
}

func TestClassifierCheckerDenyShortCircuits(t *testing.T) {
	cc := &ClassifierChecker{
		Classifier: &stubClassifier{decision: permissions.DecisionDeny},
		Base:       allowAllChecker{},
	}
	err := cc.CheckContext(PolicyContext{ToolName: "bash"})
	if err == nil {
		t.Fatalf("expected Deny error, got nil")
	}
	if !errors.Is(err, ErrToolDenied) {
		t.Errorf("expected ErrToolDenied wrapping, got %v", err)
	}
}

func TestClassifierCheckerAskFallsThroughToBase(t *testing.T) {
	stub := &stubClassifier{decision: permissions.DecisionAsk}
	cc := &ClassifierChecker{Classifier: stub, Base: allowAllChecker{}}
	if err := cc.CheckContext(PolicyContext{ToolName: "bash"}); err != nil {
		t.Errorf("Ask should fall through to permissive base; got %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("expected classifier consulted once, got %d", stub.calls)
	}
}

func TestClassifierCheckerErrorFallsThroughToBase(t *testing.T) {
	stub := &stubClassifier{decision: permissions.DecisionAllow, err: errors.New("model down")}
	cc := &ClassifierChecker{Classifier: stub, Base: denyAllChecker{}}
	// Error path should NOT trust the classifier's decision; defer to base.
	if err := cc.CheckContext(PolicyContext{ToolName: "bash"}); err == nil {
		t.Errorf("expected base deny on classifier error; got nil")
	}
}

func TestClassifierCheckerNilFieldsAllow(t *testing.T) {
	var cc *ClassifierChecker
	if err := cc.CheckContext(PolicyContext{ToolName: "bash"}); err != nil {
		t.Errorf("nil receiver should allow, got %v", err)
	}

	cc = &ClassifierChecker{} // no classifier, no base
	if err := cc.CheckContext(PolicyContext{ToolName: "bash"}); err != nil {
		t.Errorf("empty checker should allow, got %v", err)
	}
}

func TestClassifierCheckerDecodesArgs(t *testing.T) {
	captured := map[string]any{}
	cc := &ClassifierChecker{
		Classifier: classifierFunc(func(_ context.Context, name string, args map[string]any) (permissions.Decision, error) {
			captured = args
			return permissions.DecisionAllow, nil
		}),
	}
	raw, _ := json.Marshal(map[string]any{"command": "echo hi"})
	if err := cc.CheckContext(PolicyContext{ToolName: "bash", Input: raw}); err != nil {
		t.Fatal(err)
	}
	if captured["command"] != "echo hi" {
		t.Errorf("classifier did not receive decoded args; got %v", captured)
	}
}

type classifierFunc func(ctx context.Context, name string, args map[string]any) (permissions.Decision, error)

func (f classifierFunc) Classify(ctx context.Context, name string, args map[string]any) (permissions.Decision, error) {
	return f(ctx, name, args)
}

// TestClassifierCheckerHonoursContextCancellation ensures the wrapper
// propagates the caller's context to the classifier so deadlines and
// cancellation reach LLM-backed implementations.
func TestClassifierCheckerHonoursContextCancellation(t *testing.T) {
	seenCtx := make(chan context.Context, 1)
	cc := &ClassifierChecker{
		Classifier: classifierFunc(func(ctx context.Context, _ string, _ map[string]any) (permissions.Decision, error) {
			seenCtx <- ctx
			return permissions.DecisionAllow, nil
		}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before invoking; the classifier receives a cancelled ctx.
	if err := cc.CheckContext(PolicyContext{Ctx: ctx, ToolName: "bash"}); err != nil {
		t.Fatalf("Allow should succeed regardless of ctx state: %v", err)
	}
	select {
	case got := <-seenCtx:
		if got.Err() == nil {
			t.Errorf("classifier did not receive the cancelled context; want non-nil ctx.Err()")
		}
	default:
		t.Fatal("classifier was not invoked")
	}
}

// TestClassifierCheckerConcurrentSafe stresses the wrapper with many
// concurrent calls; it must not race or panic. The classifier is
// stateless, but fan_out_all branches invoke CheckContext from separate
// goroutines so this contract matters.
func TestClassifierCheckerConcurrentSafe(t *testing.T) {
	var calls atomic.Int64
	cc := &ClassifierChecker{
		Classifier: classifierFunc(func(_ context.Context, _ string, _ map[string]any) (permissions.Decision, error) {
			calls.Add(1)
			return permissions.DecisionAllow, nil
		}),
	}
	const goroutines = 50
	const callsPerGoroutine = 20
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < callsPerGoroutine; j++ {
				if err := cc.CheckContext(PolicyContext{Ctx: context.Background(), ToolName: "bash"}); err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if want := int64(goroutines * callsPerGoroutine); calls.Load() != want {
		t.Errorf("classifier called %d times, want %d", calls.Load(), want)
	}
}

// TestClassifierCheckerLogsErrors verifies the optional logger receives
// classifier errors and JSON decoding failures.
func TestClassifierCheckerLogsErrors(t *testing.T) {
	rec := &recordingLogger{}
	cc := &ClassifierChecker{
		Classifier: classifierFunc(func(_ context.Context, _ string, _ map[string]any) (permissions.Decision, error) {
			return permissions.DecisionAsk, errors.New("model down")
		}),
		Base:   allowAllChecker{},
		Logger: rec,
	}
	if err := cc.CheckContext(PolicyContext{ToolName: "bash", Input: json.RawMessage("not-json")}); err != nil {
		t.Fatalf("base should allow, got %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.warns < 2 {
		t.Errorf("expected at least 2 warns (decode + classifier err), got %d", rec.warns)
	}
}

type recordingLogger struct {
	mu     sync.Mutex
	warns  int
	debugs int
}

func (r *recordingLogger) Warn(string, ...any) {
	r.mu.Lock()
	r.warns++
	r.mu.Unlock()
}

func (r *recordingLogger) Debug(string, ...any) {
	r.mu.Lock()
	r.debugs++
	r.mu.Unlock()
}
