package delegate

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// The typed class a runner recognised must survive the IPC envelope as the
// same typed error on the launcher side, with the process exit reachable.
func TestStampIOError_TypedClassRoundTrips(t *testing.T) {
	var r IOResult
	StampIOError(&r, fmt.Errorf("claw backend: %w", &ErrModelUnavailable{Provider: "claw/openai", Model: "openai/gpt-6-astra", Detail: "requires a newer version of Codex"}))
	if r.ErrorKind != IOErrorKindModelUnavailable || r.ErrorModel != "openai/gpt-6-astra" || r.Error == "" {
		t.Fatalf("stamped envelope = %+v", r)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back IOResult
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	exit := errors.New("exit status 1")
	typed := TypedIOError(back, exit)
	var unavailable *ErrModelUnavailable
	if !errors.As(typed, &unavailable) {
		t.Fatalf("got %T (%v), want *ErrModelUnavailable", typed, typed)
	}
	if unavailable.Model != "openai/gpt-6-astra" || unavailable.Detail != r.Error || unavailable.Provider != BackendClaw {
		t.Errorf("rebuilt error = %+v", unavailable)
	}
	if !errors.Is(typed, exit) {
		t.Errorf("the runner exit must stay reachable: %v", typed)
	}
}

func TestStampIOError_PlainErrorHasNoKind(t *testing.T) {
	var r IOResult
	StampIOError(&r, errors.New("boom"))
	if r.Error != "boom" || r.ErrorKind != "" || r.ErrorModel != "" {
		t.Errorf("plain error stamped as %+v", r)
	}
	if got := TypedIOError(r, nil); got != nil {
		t.Errorf("no kind must rebuild nothing, got %v", got)
	}
	var untouched IOResult
	StampIOError(&untouched, nil)
	if untouched.Error != "" {
		t.Errorf("nil error must leave the envelope untouched: %+v", untouched)
	}
}
