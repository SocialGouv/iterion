package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// unsettledGitLab accepts the first provision's hook create and then answers
// the re-provision's hook UPDATE with a status that settles nothing: the write
// may have landed, so the stored config keeps the secret this attempt minted
// and the repo may be delivering with one the forge does not hold.
type unsettledGitLab struct {
	updateStatus int
	updateHeader map[string]string
	hookID       int
}

func (m *unsettledGitLab) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "username": "botuser"})
	})
	mux.HandleFunc("/api/v4/projects/group%2Fapi/hooks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{}) // the probe: none yet
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.hookID++
			body["id"] = m.hookID
			_ = json.NewEncoder(w).Encode(body)
		}
	})
	mux.HandleFunc("/api/v4/projects/group%2Fapi/hooks/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": m.hookID})
			return
		}
		for k, v := range m.updateHeader {
			w.Header().Set(k, v)
		}
		w.WriteHeader(m.updateStatus)
		_, _ = w.Write([]byte(`{"message":"502 Bad Gateway"}`))
	})
	return httptest.NewServer(mux)
}

// TestForgeEnable_IndeterminateHookWriteReachesOps: the state the type exists
// to name — the stored config may not match the forge's live hook, so the repo
// may have stopped authenticating — is one operator gesture away from being
// repaired (re-run the request) and completely invisible once that operator
// closes the tab. The answer goes to the tab; the deployment's log sink is
// what makes the repair happen at all.
func TestForgeEnable_IndeterminateHookWriteReachesOps(t *testing.T) {
	gl := &unsettledGitLab{updateStatus: http.StatusBadGateway}
	srv := gl.server()
	defer srv.Close()

	s := newForgeTestServer(t)
	s.forgeOrchestrator.Bots = gateBotLookup
	logs := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelWarn, logs)
	seedForgeConn(t, s, "c1", srv.URL, "")

	first := enableRepoBots(t, s, "c1", "plain-bot")
	if first.Code != http.StatusOK {
		t.Fatalf("the premise (a working repo) failed: code=%d body=%s", first.Code, first.Body.String())
	}
	var res struct {
		WebhookID string `json:"webhook_id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &res); err != nil || res.WebhookID == "" {
		t.Fatalf("enable answered no webhook id: %v (%s)", err, first.Body.String())
	}

	// The re-provision — the #2445 gesture, an admin adding a bot to a working
	// repo. The bot set changes, so the provision is a mutating one: the
	// stored hook id is known, the upsert UPDATES it, and the gateway in front
	// of the forge answers 502.
	w := enableRepoBots(t, s, "c1", "gate-bot")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s, want 502: the forge's own failure", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "provisioning failed") {
		t.Errorf("the answer does not name the failure: %s", w.Body.String())
	}
	warned := logs.String()
	for _, want := range []string{res.WebhookID, "re-run the request"} {
		if !strings.Contains(warned, want) {
			t.Errorf("the Warn sink is missing %q — a repo that may have stopped authenticating reached nobody "+
				"but the browser tab that asked:\n%s", want, warned)
		}
	}
}

// TestWriteForgeProvisionError_IndeterminateHookWriteLogsAndKeepsTheStatus:
// the arm logs, and answers with the status the forge's own failure maps to —
// a 5xx is a 502, a rate limit is a 429 carrying its Retry-After. A failure
// that is NOT typed indeterminate logs nothing here: the warn names a repo
// that may be on a secret the forge does not hold, which a settled failure is
// not.
func TestWriteForgeProvisionError_IndeterminateHookWriteLogsAndKeepsTheStatus(t *testing.T) {
	for _, tc := range []struct {
		name           string
		err            error
		wantCode       int
		wantRetryAfter bool
		wantWarn       bool
	}{{
		name:     "the forge fell over",
		err:      &forge.HookWriteIndeterminateError{WebhookID: "wh-1", Err: forge.StatusErr("gitlab", "update hook", http.StatusBadGateway)},
		wantCode: http.StatusBadGateway,
		wantWarn: true,
	}, {
		name: "the forge rate-limited the write",
		err: &forge.HookWriteIndeterminateError{WebhookID: "wh-1", Err: &forge.StatusError{
			Provider: forge.ProviderGitLab, Op: "update hook", Code: http.StatusTooManyRequests,
			ResetAt: time.Now().Add(42 * time.Second),
		}},
		wantCode:       http.StatusTooManyRequests,
		wantRetryAfter: true,
		wantWarn:       true,
	}, {
		name: "the answer never came",
		err: &forge.HookWriteIndeterminateError{WebhookID: "wh-1", Err: &url.Error{
			Op: "Put", URL: "https://gitlab.example.com/api/v4/projects/group%2Fapi/hooks/3",
			Err: errors.New("context deadline exceeded (Client.Timeout exceeded while awaiting headers)"),
		}},
		wantCode: http.StatusBadGateway,
		wantWarn: true,
	}, {
		name:     "a settled failure of the same status",
		err:      forge.StatusErr("gitlab", "list repos", http.StatusBadGateway),
		wantCode: http.StatusBadGateway,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newForgeTestServer(t)
			logs := &lockedBuffer{}
			s.logger = iterlog.New(iterlog.LevelWarn, logs)

			w := httptest.NewRecorder()
			s.writeForgeProvisionError(w, tc.err)
			if w.Code != tc.wantCode {
				t.Errorf("code=%d, want %d (the forge's own failure decides it): %s", w.Code, tc.wantCode, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); tc.wantRetryAfter != (got != "") {
				t.Errorf("Retry-After = %q, want present=%v", got, tc.wantRetryAfter)
			}
			warned := logs.String()
			switch {
			case tc.wantWarn && !strings.Contains(warned, "wh-1"):
				t.Errorf("the Warn sink does not name the webhook whose config may not match the forge's hook:\n%s", warned)
			case tc.wantWarn && !strings.Contains(warned, "re-run the request"):
				t.Errorf("the Warn line does not name the gesture that repairs it:\n%s", warned)
			case !tc.wantWarn && strings.Contains(warned, "outcome is unknown"):
				t.Errorf("a settled failure was logged as an unsettled hook write:\n%s", warned)
			}
		})
	}
}
