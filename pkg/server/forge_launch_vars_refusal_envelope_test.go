package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// A launch_vars refusal is only actionable if the client can tell WHICH
// remedy it is looking at and WHICH pins are at stake (#2173). The prose
// carries neither readably — it is free to be reworded, and a 500's prose is
// replaced outright by internal/httpx's floor — so the envelope must carry
// the stable code, and the 400 must carry the keys as a list read from the
// typed cause.
func TestForgeProvisionError_LaunchVarsRefusalEnvelope(t *testing.T) {
	s := newForgeTestServer(t)

	keyed := func(sentinels ...error) error {
		err := fmt.Errorf("%w [gate_context review_tier] — the remedy", sentinels[0])
		for _, extra := range sentinels[1:] {
			err = fmt.Errorf("%w (%w)", err, extra)
		}
		return &forge.LaunchVarsRefusal{Keys: []string{"gate_context", "review_tier"}, Cause: err}
	}

	for _, tc := range []struct {
		name         string
		err          error
		wantStatus   int
		wantCode     string
		wantKeys     []string
		wantRetry    any
		keysMustStay bool
	}{
		{
			name: "drop names its keys", err: keyed(forge.ErrLaunchVarsDrop),
			wantStatus: http.StatusBadRequest, wantCode: "launch_vars_drop",
			wantKeys: []string{"gate_context", "review_tier"}, keysMustStay: true,
		},
		{
			// The deterministic divergence: only a re-submission with the
			// explicit gesture resolves it, so retryable must read false.
			name: "deterministic divergence", err: keyed(forge.ErrProvisionDiverged),
			wantStatus: http.StatusInternalServerError, wantCode: "provision_diverged",
			wantRetry: false,
		},
		{
			// The retry-convergent flavor: a plain re-run lands.
			name: "retry-convergent divergence", err: keyed(forge.ErrProvisionDiverged, forge.ErrProvisionDivergedRetry),
			wantStatus: http.StatusInternalServerError, wantCode: "provision_diverged_retry",
			wantRetry: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.writeForgeProvisionError(w, tc.err)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %s: %v", w.Body.String(), err)
			}
			if body["error_code"] != tc.wantCode {
				t.Fatalf("error_code = %v, want %q — the client branches on this, not on the message (body %s)",
					body["error_code"], tc.wantCode, w.Body.String())
			}
			if tc.wantRetry != nil && body["retryable"] != tc.wantRetry {
				t.Fatalf("retryable = %v, want %v: the two divergence flavors are two different remedies (body %s)",
					body["retryable"], tc.wantRetry, w.Body.String())
			}
			raw, listed := body["launch_vars_keys"]
			if tc.keysMustStay {
				if !listed {
					t.Fatalf("no launch_vars_keys in %s — an operator cannot echo pins nobody named", w.Body.String())
				}
				got, _ := raw.([]any)
				if len(got) != len(tc.wantKeys) {
					t.Fatalf("launch_vars_keys = %v, want %v", raw, tc.wantKeys)
				}
				for i, want := range tc.wantKeys {
					if got[i] != want {
						t.Fatalf("launch_vars_keys[%d] = %v, want %q (sorted, as the orchestrator names them)", i, got[i], want)
					}
				}
			} else if listed {
				// A 5xx must not leak the refusal's detail: internal/httpx
				// floors the payload to its code and retryability on purpose,
				// and a key list surviving that floor is the floor broken.
				t.Fatalf("launch_vars_keys survived the 5xx floor: %s", w.Body.String())
			}
		})
	}
}

// The keys travel on the TYPED cause, so the envelope keeps naming them after
// any rewording of the message — and errors.Is still answers for every
// sentinel in the chain, which is what the route's switch and the approval
// arm's deterministicProvisionRefusal both read.
func TestLaunchVarsRefusal_KeepsItsSentinelChain(t *testing.T) {
	refusal := &forge.LaunchVarsRefusal{
		Keys:  []string{"gate_context"},
		Cause: fmt.Errorf("%w: diverged (%w)", forge.ErrProvisionDiverged, forge.ErrProvisionDivergedRetry),
	}
	err := fmt.Errorf("provision: %w", refusal)
	if !errors.Is(err, forge.ErrProvisionDiverged) || !errors.Is(err, forge.ErrProvisionDivergedRetry) {
		t.Fatalf("the sentinel chain did not survive the typed refusal: %v", err)
	}
	var read *forge.LaunchVarsRefusal
	if !errors.As(err, &read) {
		t.Fatalf("errors.As cannot reach the typed refusal through a wrap: %v", err)
	}
	if len(read.Keys) != 1 || read.Keys[0] != "gate_context" {
		t.Fatalf("keys = %v, want [gate_context]", read.Keys)
	}
	if read.Error() != refusal.Cause.Error() {
		t.Fatalf("Error() = %q, want the cause's own words %q", read.Error(), refusal.Cause.Error())
	}
}
