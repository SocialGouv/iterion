package model

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestStoreEventHooks_RedactsSecretsAtSinks proves Layer 0: a real
// secret value — and its base64 encoding — never reach events.jsonl or
// run.log, while the placeholder does.
func TestStoreEventHooks_RedactsSecretsAtSinks(t *testing.T) {
	const secret = "sk-ant-SUPERSECRET-abcdef0123456789ABCDEF"
	b64 := base64.StdEncoding.EncodeToString([]byte(secret))
	const ph = "__ITERION_SECRET_api_key__"

	guard := secretguard.New(
		[]secretguard.Secret{{Name: "api_key", Value: secret}},
		secretguard.DefaultConfig(),
	)

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	const runID = "run-redact"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	var logBuf bytes.Buffer
	logger := iterlog.New(iterlog.LevelTrace, &logBuf)
	hooks := NewStoreEventHooks(ctx, st, runID, logger, guard, nil)

	// Fire the secret through every high-risk sink.
	hooks.OnLLMPrompt("n1", "system uses "+secret, "user sends "+b64)
	hooks.OnLLMStepFinish("n1", LLMStepInfo{Number: 1, Text: "the key is " + secret})
	hooks.OnToolCall("n1", LLMToolCallInfo{ToolName: "Bash", Output: "echo " + secret, Duration: time.Millisecond})
	hooks.OnToolNodeResult("n2", "curl", []byte("curl -H auth:"+secret), "response "+b64, time.Millisecond, nil)

	evts, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	blob, _ := json.Marshal(evts)
	events := string(blob)
	if strings.Contains(events, secret) {
		t.Error("raw secret leaked into events.jsonl")
	}
	if strings.Contains(events, b64) {
		t.Error("base64 secret leaked into events.jsonl")
	}
	if !strings.Contains(events, ph) {
		t.Errorf("placeholder missing from events.jsonl:\n%s", events)
	}

	log := logBuf.String()
	if strings.Contains(log, secret) {
		t.Error("raw secret leaked into run.log")
	}
	if strings.Contains(log, b64) {
		t.Error("base64 secret leaked into run.log")
	}
	if !strings.Contains(log, ph) {
		t.Errorf("placeholder missing from run.log:\n%s", log)
	}
}

// TestStoreEventHooks_NilGuardNoRedaction confirms the kill-switch
// (ITERION_SECRETS_REDACT=off ⇒ nil guard) leaves content untouched.
func TestStoreEventHooks_NilGuardNoRedaction(t *testing.T) {
	const secret = "sk-ant-PLAINTEXT-0123456789abcdefABCDEF"

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	const runID = "run-noredact"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	logger := iterlog.New(iterlog.LevelInfo, &bytes.Buffer{})
	hooks := NewStoreEventHooks(ctx, st, runID, logger, nil, nil)
	hooks.OnLLMPrompt("n1", "", "user sends "+secret)

	evts, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	blob, _ := json.Marshal(evts)
	if !strings.Contains(string(blob), secret) {
		t.Error("nil guard must not redact — secret should be present verbatim")
	}
}
