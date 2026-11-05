package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

func TestR3ProviderErrorSurvivesSandboxIPC(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	const message = "reasoning_effort must be low, high, xhigh, max"
	var wire delegate.IOResult
	if err := json.Unmarshal([]byte(`{"error":"openai: API error 400: reasoning_effort must be low, high, xhigh, max","provider_error":{"StatusCode":400,"Provider":"openai","Message":"`+message+`"}}`), &wire); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := resultEnvelopeFile(t, dir, wire)
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{MaxAttempts: 1})
	_, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{NodeID: "n", Model: "openai/gpt-test", WorkDir: dir, Sandbox: &scriptedSandbox{script: awaitTaskEnvelope + "cat \"$1\"\nexit 1\n", args: []string{file}}})
	var typed *api.APIError
	if !errors.As(err, &typed) || typed.StatusCode != 400 || typed.Message != message {
		t.Fatalf("provider identity lost: %T %v", err, err)
	}
}

func TestR3ProviderStreamIdentitySurvivesSandboxIPC(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	const message = "upstream: connection reset by peer"
	var wire delegate.IOResult
	if err := json.Unmarshal([]byte(`{"error":"stream error","provider_error_stream":true,"provider_error":{"StatusCode":400,"Message":"`+message+`","Retryable":true}}`), &wire); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := resultEnvelopeFile(t, dir, wire)
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{MaxAttempts: 1})
	_, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{NodeID: "n", Model: "openai/gpt-test", WorkDir: dir, Sandbox: &scriptedSandbox{script: awaitTaskEnvelope + "cat \"$1\"\nexit 1\n", args: []string{file}}})
	var stream *APIError
	if !errors.As(err, &stream) || stream.StatusCode != 400 || stream.Message != message || !stream.IsRetryable {
		t.Fatalf("stream identity lost: %T %v", err, err)
	}
}
