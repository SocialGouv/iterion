package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

// driveClawRunner runs runClawRunner against piped stdio, sends the
// given pre-task envelopes followed by a task envelope, and returns the
// terminal IOResult the runner emitted.
func driveClawRunner(t *testing.T, preTask []delegate.Envelope, task delegate.IOTask) delegate.IOResult {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = runClawRunner(context.Background(), stdinR, stdoutW, io.Discard)
		_ = stdoutW.Close()
	}()

	writer := delegate.NewEnvelopeWriter(stdinW)
	go func() {
		for _, env := range preTask {
			if err := writer.Write(env); err != nil {
				return
			}
		}
		taskEnv, err := delegate.NewTaskEnvelope(task)
		if err != nil {
			return
		}
		_ = writer.Write(taskEnv)
	}()

	// Read envelopes until the terminal result (the runner may emit
	// intermediate event envelopes first).
	resultCh := make(chan delegate.IOResult, 1)
	go func() {
		reader := delegate.NewEnvelopeReader(stdoutR)
		for {
			env, err := reader.Read()
			if err != nil {
				close(resultCh)
				return
			}
			if env.Type == delegate.EnvelopeResult {
				var res delegate.IOResult
				_ = json.Unmarshal(env.Data, &res)
				resultCh <- res
				return
			}
		}
	}()

	select {
	case res, ok := <-resultCh:
		if !ok {
			t.Fatal("runner closed stdout without a result envelope")
		}
		_ = stdinW.Close()
		wg.Wait()
		return res
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the runner's result envelope")
		return delegate.IOResult{}
	}
}

// TestClawRunner_MalformedPermissionPolicyFatalsBeforeModelCall: a gate
// the author declared must never silently not exist — an unparsable
// policy is a protocol-level fatal, emitted before any model call.
func TestClawRunner_MalformedPermissionPolicyFatalsBeforeModelCall(t *testing.T) {
	env := delegate.Envelope{
		Type: delegate.EnvelopePermissionPolicy,
		Data: json.RawMessage(`{"mode":"hrad"}`),
	}
	res := driveClawRunner(t, []delegate.Envelope{env}, delegate.IOTask{NodeID: "x", Model: "anthropic/claude-opus-5"})
	if res.Error == "" || !strings.Contains(res.Error, "permission") {
		t.Fatalf("expected a permission-policy fatal, got %q", res.Error)
	}
}

// TestClawRunner_UndecodablePermissionPolicyFatals: garbage bytes in the
// envelope are refused the same way.
func TestClawRunner_UndecodablePermissionPolicyFatals(t *testing.T) {
	env := delegate.Envelope{
		Type: delegate.EnvelopePermissionPolicy,
		Data: json.RawMessage(`"not-an-object"`),
	}
	res := driveClawRunner(t, []delegate.Envelope{env}, delegate.IOTask{NodeID: "x", Model: "anthropic/claude-opus-5"})
	if res.Error == "" || !strings.Contains(res.Error, "permission_policy") {
		t.Fatalf("expected a decode fatal, got %q", res.Error)
	}
}

// TestClawRunner_UnknownPreTaskEnvelopeFatals is the mixed-fleet canari:
// the fail-closed guarantee of the pre-task permission_policy envelope
// rests on an old runner treating ANY unknown pre-task envelope as
// fatal instead of ignoring it and running the gated node with an empty
// policy. If this behaviour ever relaxes, that guarantee is gone.
func TestClawRunner_UnknownPreTaskEnvelopeFatals(t *testing.T) {
	env := delegate.Envelope{
		Type: delegate.EnvelopeType("permission_policy_v99"),
		Data: json.RawMessage(`{}`),
	}
	res := driveClawRunner(t, []delegate.Envelope{env}, delegate.IOTask{NodeID: "x", Model: "anthropic/claude-opus-5"})
	if res.Error == "" || !strings.Contains(res.Error, "unexpected envelope") {
		t.Fatalf("expected an unexpected-envelope fatal, got %q", res.Error)
	}
}

// TestClawRunner_ValidPermissionPolicyIsAcceptedPreTask: a well-formed
// policy must NOT fatal the bootstrap — the runner proceeds to the model
// phase (which then fails on the unresolvable registry in this test
// env, an error that names the model resolution, not the policy).
func TestClawRunner_ValidPermissionPolicyIsAcceptedPreTask(t *testing.T) {
	cfg := permission.PolicyConfig{Mode: "deny", Allow: []string{"WebFetch(*)"}}
	env, err := delegate.NewPermissionPolicyEnvelope(cfg)
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	res := driveClawRunner(t, []delegate.Envelope{env}, delegate.IOTask{NodeID: "x", Model: "bogus-provider/nope"})
	if res.Error == "" {
		t.Fatal("expected a model-resolution error in this credential-less test env")
	}
	if strings.Contains(res.Error, "permission") {
		t.Fatalf("valid policy was blamed by the bootstrap: %q", res.Error)
	}
}
