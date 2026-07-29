package delegate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// asyncStub stands in for the executor's interaction store: it records posted
// questions and decides which are still unanswered.
type asyncStub struct {
	mu       sync.Mutex
	posted   []AsyncQuestion
	pending  []PendingAsync
	answers  string
	postErr  error
	pendErr  error
	collects int
}

func (s *asyncStub) bind(task *Task) {
	task.InteractionEnabled = true
	task.PostAsyncQuestion = func(q AsyncQuestion) (string, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.postErr != nil {
			return "", s.postErr
		}
		s.posted = append(s.posted, q)
		return fmt.Sprintf("q-%d", len(s.posted)), nil
	}
	task.PendingAsyncQuestions = func() ([]PendingAsync, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.pending, s.pendErr
	}
	task.CollectAsyncAnswers = func() (string, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.collects++
		return s.answers, nil
	}
}

func TestPiPostAsyncQuestion(t *testing.T) {
	t.Run("posts and returns the id plus the shared wording", func(t *testing.T) {
		stub := &asyncStub{}
		task := Task{}
		stub.bind(&task)

		got, err := piPostAsyncQuestion(task, []byte(`{"question":"Which region?","options":[{"id":"eu","label":"EU"}]}`))
		if err != nil {
			t.Fatalf("piPostAsyncQuestion: %v", err)
		}
		if got.InteractionID != "q-1" {
			t.Errorf("InteractionID = %q, want the store's id", got.InteractionID)
		}
		// The wording must come from iterion, not the extension: identical
		// prompting across backends is the point of ADR-081.
		if got.Message != AsyncQuestionPostedText {
			t.Errorf("Message = %q, want AsyncQuestionPostedText", got.Message)
		}
		if len(stub.posted) != 1 || stub.posted[0].Question != "Which region?" {
			t.Fatalf("posted = %+v, want the model's question", stub.posted)
		}
		if len(stub.posted[0].Options) != 1 || stub.posted[0].Options[0].ID != "eu" {
			t.Errorf("options lost on the way to the store: %+v", stub.posted[0].Options)
		}
	})

	t.Run("refuses when the node has no async wiring", func(t *testing.T) {
		if _, err := piPostAsyncQuestion(Task{}, []byte(`{"question":"hi"}`)); err == nil {
			t.Fatal("posted a question on a node with no interaction store")
		}
	})

	t.Run("refuses an empty question", func(t *testing.T) {
		task := Task{}
		(&asyncStub{}).bind(&task)
		if _, err := piPostAsyncQuestion(task, []byte(`{"question":"   "}`)); err == nil {
			t.Fatal("accepted an empty question — the operator would see nothing to answer")
		}
	})

	t.Run("surfaces a store failure instead of losing the question", func(t *testing.T) {
		stub := &asyncStub{postErr: errors.New("disk full")}
		task := Task{}
		stub.bind(&task)
		_, err := piPostAsyncQuestion(task, []byte(`{"question":"hi"}`))
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("err = %v, want the store's cause — a silently dropped question never gets answered", err)
		}
	})
}

func TestPiAwaitAnswers(t *testing.T) {
	t.Run("nothing pending returns the answers without pausing", func(t *testing.T) {
		stub := &asyncStub{answers: "q-1: EU"}
		task := Task{}
		stub.bind(&task)

		result, pause, err := piAwaitAnswers(task)
		if err != nil {
			t.Fatalf("piAwaitAnswers: %v", err)
		}
		if pause != nil {
			t.Fatal("paused the run although every question was answered — that costs a full resume for nothing")
		}
		if result.Escalated || result.Answers != "q-1: EU" {
			t.Errorf("result = %+v, want the answers inline", result)
		}
	})

	t.Run("pending escalates to a pause carrying the refs", func(t *testing.T) {
		stub := &asyncStub{pending: []PendingAsync{{InteractionID: "q-2", Question: "Which region?"}}}
		task := Task{}
		stub.bind(&task)

		result, pause, err := piAwaitAnswers(task)
		if err != nil {
			t.Fatalf("piAwaitAnswers: %v", err)
		}
		if pause == nil {
			t.Fatal("no pause although a question is unanswered — the agent would proceed without it")
		}
		if !result.Escalated || len(result.Pending) != 1 || result.Pending[0].InteractionID != "q-2" {
			t.Errorf("result = %+v, want the pending refs", result)
		}
		// Collecting is what the answered path does; doing it here too would
		// hand the agent a half-empty answer set right before it pauses.
		if stub.collects != 0 {
			t.Errorf("collected answers %d time(s) while escalating", stub.collects)
		}

		var ask *ErrAskUser
		if !errors.As(pause.err(), &ask) {
			t.Fatalf("pause.err() = %T, want *ErrAskUser", pause.err())
		}
		if len(ask.AwaitPending) != 1 || ask.AwaitPending[0].InteractionID != "q-2" {
			t.Errorf("AwaitPending = %+v — without it the resume path cannot fan the answers back out", ask.AwaitPending)
		}
		if !strings.Contains(ask.Question, "Which region?") {
			t.Errorf("Question = %q, want the pending question listed for the operator", ask.Question)
		}
	})

	t.Run("refuses when the node has no async wiring", func(t *testing.T) {
		if _, _, err := piAwaitAnswers(Task{}); err == nil {
			t.Fatal("awaited on a node with no interaction store")
		}
	})
}

// The async pair is registered only for a node the executor actually wired for
// it. The system prompt describes ask_user_async/await_answers on every backend
// for such a node, so the two must agree — otherwise the model is told to call
// tools that do not exist.
func TestPiInteractionModeMatchesWiring(t *testing.T) {
	sync := Task{NodeID: "n", InteractionEnabled: true}
	if got := piExtensionEnv(sync, nil)["ITERION_PI_INTERACTION"]; got != "sync" {
		t.Errorf("ITERION_PI_INTERACTION = %q for a blocking node, want sync", got)
	}

	async := Task{NodeID: "n", InteractionEnabled: true}
	(&asyncStub{}).bind(&async)
	if got := piExtensionEnv(async, nil)["ITERION_PI_INTERACTION"]; got != "async" {
		t.Errorf("ITERION_PI_INTERACTION = %q for an async node, want async", got)
	}

	if !strings.Contains(async.BuildSystemPrompt(), "ask_user_async") {
		t.Fatal("the async protocol is missing from the system prompt; this test's premise is stale")
	}
}

// TestPiRPCLiveAsyncQuestions proves the non-blocking pair end to end against a
// real pi: the model posts a question, keeps working, and syncs later.
//
// The two outcomes are asymmetric by design and both matter — everything
// answered must NOT cost a pause, while something outstanding must.
func TestPiRPCLiveAsyncQuestions(t *testing.T) {
	bin := requirePiBinary(t)
	ext := mockProviderPath(t)

	script := `[{"name":"ask_user_async","arguments":{"question":"Which region?"}},` +
		`{"name":"await_answers","arguments":{}}]`

	t.Run("answered: no pause, answers reach the model", func(t *testing.T) {
		t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "1")
		t.Setenv("ITERION_PI_MOCK_TOOLS", script)
		t.Setenv("ITERION_PI_MOCK_TEXT", "deploying to EU")

		stub := &asyncStub{answers: "q-1 (Which region?): EU"}
		dir := t.TempDir()
		var toolOut []string
		var mu sync.Mutex
		task := Task{
			NodeID: "async", WorkDir: dir, BaseDir: dir, StoreDir: dir,
			UserPrompt: "ship it", Model: "mock/scripted",
			Hooks: TaskHooks{OnToolCalled: func(name, id string, isErr bool, out string) {
				mu.Lock()
				toolOut = append(toolOut, name+"|"+out)
				mu.Unlock()
			}},
		}
		stub.bind(&task)

		rpc := &PiRPCBackend{Command: bin, Logger: testLogger(), ExtraArgs: []string{"-e", ext}}
		if _, err := rpc.Execute(context.Background(), task); err != nil {
			t.Fatalf("Execute: %v — an answered await must not pause or fail", err)
		}

		stub.mu.Lock()
		posted := append([]AsyncQuestion(nil), stub.posted...)
		stub.mu.Unlock()
		if len(posted) != 1 || posted[0].Question != "Which region?" {
			t.Fatalf("posted = %+v, want the model's question in the store", posted)
		}

		mu.Lock()
		defer mu.Unlock()
		joined := strings.Join(toolOut, " ")
		if !strings.Contains(joined, "q-1") {
			t.Errorf("the interaction id never reached the model: %q", joined)
		}
		if !strings.Contains(joined, "EU") {
			t.Errorf("the answers never reached the model: %q", joined)
		}
	})

	t.Run("still pending: the run pauses carrying the refs", func(t *testing.T) {
		t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "1")
		t.Setenv("ITERION_PI_MOCK_TOOLS", script)
		t.Setenv("ITERION_PI_MOCK_TEXT", "unreachable")

		stub := &asyncStub{pending: []PendingAsync{{InteractionID: "q-1", Question: "Which region?"}}}
		dir := t.TempDir()
		task := Task{
			NodeID: "async", WorkDir: dir, BaseDir: dir, StoreDir: dir,
			UserPrompt: "ship it", Model: "mock/scripted",
		}
		stub.bind(&task)

		rpc := &PiRPCBackend{Command: bin, Logger: testLogger(), ExtraArgs: []string{"-e", ext}}
		_, err := rpc.Execute(context.Background(), task)

		var ask *ErrAskUser
		if !errors.As(err, &ask) {
			t.Fatalf("err = %v (%T), want *ErrAskUser — the run must PAUSE", err, err)
		}
		if len(ask.AwaitPending) != 1 || ask.AwaitPending[0].InteractionID != "q-1" {
			t.Errorf("AwaitPending = %+v, want the refs the resume path needs", ask.AwaitPending)
		}
	})
}
