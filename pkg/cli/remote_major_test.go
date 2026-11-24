package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

// setClientVersion pins the client's release for one test.
func setClientVersion(t *testing.T, v string) {
	t.Helper()
	prev := appinfo.Version
	t.Cleanup(func() { appinfo.Version = prev })
	appinfo.Version = v
}

// majorGateServer is an instance whose /api/server/info answer the test
// controls, recording every request it receives.
type majorGateServer struct {
	mu sync.Mutex
	// infoStatus/infoBody answer serverInfoPath.
	infoStatus int
	infoBody   string
	// apiStatus/apiBody answer every other path.
	apiStatus int
	apiBody   string
	probes    int
	apiHits   int
	headers   []string
}

func (s *majorGateServer) set(fn func(s *majorGateServer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

func (s *majorGateServer) counts() (probes, apiHits int, headers []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probes, s.apiHits, append([]string(nil), s.headers...)
}

func (s *majorGateServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.headers = append(s.headers, r.Header.Get(RemoteClientHeader))
	if r.URL.Path == serverInfoPath {
		s.probes++
		w.WriteHeader(s.infoStatus)
		_, _ = w.Write([]byte(s.infoBody))
		return
	}
	s.apiHits++
	w.WriteHeader(s.apiStatus)
	_, _ = w.Write([]byte(s.apiBody))
}

func serverInfoBody(version string) string {
	b, _ := json.Marshal(map[string]any{"mode": "cloud", "version": version, "commit": "abc1234"})
	return string(b)
}

func newMajorGateClient(t *testing.T, version string) (*majorGateServer, *RemoteClient) {
	t.Helper()
	t.Setenv(allowMajorMismatchEnv, "")
	t.Setenv(serverInfoTimeoutEnv, "")
	gs := &majorGateServer{infoStatus: http.StatusOK, infoBody: serverInfoBody(version), apiStatus: http.StatusOK, apiBody: `{}`}
	srv := httptest.NewServer(gs)
	t.Cleanup(srv.Close)
	return gs, &RemoteClient{cfg: RemoteConfig{BaseURL: srv.URL, Token: "tok"}, http: srv.Client()}
}

func TestClientHeaderOnEveryRequestAndOneProbe(t *testing.T) {
	setClientVersion(t, "v7.33.0")
	gs, c := newMajorGateClient(t, "v7.40.1")
	for i := 0; i < 3; i++ {
		if _, err := c.Call(context.Background(), "GET", "/api/runs", nil, nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	probes, apiHits, headers := gs.counts()
	if probes != 1 {
		t.Errorf("server info probed %d times across three calls, want 1", probes)
	}
	if apiHits != 3 {
		t.Errorf("API saw %d requests, want 3", apiHits)
	}
	if len(headers) != 4 {
		t.Fatalf("server saw %d requests, want 4 (probe + 3)", len(headers))
	}
	for i, h := range headers {
		if h != "v7.33.0" {
			t.Errorf("request %d carried %s=%q, want %q", i, RemoteClientHeader, h, "v7.33.0")
		}
	}
}

func TestMajorGateRefusesOtherMajorBeforeSending(t *testing.T) {
	cases := []struct {
		name, client, server string
		older                bool
	}{
		{"client older", "v7.33.0", "v8.0.0", true},
		{"client newer", "v9.1.0", "v8.2.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setClientVersion(t, tc.client)
			gs, c := newMajorGateClient(t, tc.server)
			_, err := c.Call(context.Background(), "POST", "/api/runs", map[string]string{"a": "b"}, nil)
			var mm *MajorMismatchError
			if !errors.As(err, &mm) {
				t.Fatalf("err = %v, want *MajorMismatchError", err)
			}
			if !errors.Is(err, ErrMajorMismatch) {
				t.Errorf("errors.Is(err, ErrMajorMismatch) = false")
			}
			if got := errors.Is(err, ErrClientMajorOlder); got != tc.older {
				t.Errorf("errors.Is(err, ErrClientMajorOlder) = %v, want %v", got, tc.older)
			}
			if got := errors.Is(err, ErrClientMajorNewer); got != !tc.older {
				t.Errorf("errors.Is(err, ErrClientMajorNewer) = %v, want %v", got, !tc.older)
			}
			msg := err.Error()
			for _, want := range []string{allowMajorMismatchEnv, tc.client, tc.server} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not name %q", msg, want)
				}
			}
			if _, apiHits, _ := gs.counts(); apiHits != 0 {
				t.Errorf("API saw %d requests, want 0: the refusal comes before the send", apiHits)
			}
		})
	}
}

func TestMajorGatePassesSameMajor(t *testing.T) {
	setClientVersion(t, "v8.0.0")
	gs, c := newMajorGateClient(t, "v8.3.1")
	if _, err := c.Call(context.Background(), "GET", "/api/runs", nil, nil); err != nil {
		t.Fatalf("same major refused: %v", err)
	}
	if _, apiHits, _ := gs.counts(); apiHits != 1 {
		t.Errorf("API saw %d requests, want 1", apiHits)
	}
}

func TestMajorGateBypassEnv(t *testing.T) {
	setClientVersion(t, "v7.33.0")
	gs, c := newMajorGateClient(t, "v8.0.0")
	t.Setenv(allowMajorMismatchEnv, "1")
	if _, err := c.Call(context.Background(), "GET", "/api/runs", nil, nil); err != nil {
		t.Fatalf("bypass set, request refused: %v", err)
	}
	if _, apiHits, _ := gs.counts(); apiHits != 1 {
		t.Errorf("API saw %d requests, want 1", apiHits)
	}

	t.Setenv(allowMajorMismatchEnv, "maybe")
	_, err := c.Call(context.Background(), "GET", "/api/runs", nil, nil)
	if err == nil || !strings.Contains(err.Error(), allowMajorMismatchEnv) {
		t.Fatalf("malformed bypass value: err = %v, want an error naming %s", err, allowMajorMismatchEnv)
	}
}

func TestMajorGatePassesWhatItCannotKnow(t *testing.T) {
	t.Run("server older than the info route", func(t *testing.T) {
		setClientVersion(t, "v8.0.0")
		gs, c := newMajorGateClient(t, "")
		gs.set(func(s *majorGateServer) { s.infoStatus, s.infoBody = http.StatusNotFound, `404 page not found` })
		for i := 0; i < 2; i++ {
			if _, err := c.Call(context.Background(), "GET", "/api/runs", nil, nil); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
		if probes, apiHits, _ := gs.counts(); probes != 1 || apiHits != 2 {
			t.Errorf("probes=%d apiHits=%d, want 1 and 2: a failed probe is remembered", probes, apiHits)
		}
	})
	t.Run("version-less answer", func(t *testing.T) {
		setClientVersion(t, "v8.0.0")
		gs, c := newMajorGateClient(t, "")
		if _, err := c.Call(context.Background(), "GET", "/api/runs", nil, nil); err != nil {
			t.Fatalf("version-less server refused: %v", err)
		}
		if _, apiHits, _ := gs.counts(); apiHits != 1 {
			t.Errorf("API saw %d requests, want 1", apiHits)
		}
	})
	t.Run("dev client", func(t *testing.T) {
		setClientVersion(t, "dev")
		gs, c := newMajorGateClient(t, "v8.0.0")
		if _, err := c.Call(context.Background(), "GET", "/api/runs", nil, nil); err != nil {
			t.Fatalf("dev client refused: %v", err)
		}
		probes, apiHits, headers := gs.counts()
		if probes != 0 || apiHits != 1 {
			t.Errorf("probes=%d apiHits=%d, want 0 and 1: a dev client is never gated", probes, apiHits)
		}
		if len(headers) != 1 || headers[0] != "dev" {
			t.Errorf("headers = %q, want [dev]", headers)
		}
	})
}

func TestMajorGateReprobesAfter404(t *testing.T) {
	setClientVersion(t, "v7.33.0")
	gs, c := newMajorGateClient(t, "v7.33.0")
	gs.set(func(s *majorGateServer) { s.apiStatus = http.StatusNotFound })
	_, err := c.Call(context.Background(), "GET", "/api/runs/r1", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want the 404 as *APIError", err)
	}
	// The URL now reaches an instance of another major.
	gs.set(func(s *majorGateServer) {
		s.infoBody = serverInfoBody("v8.0.0")
		s.apiStatus = http.StatusOK
	})
	_, err = c.Call(context.Background(), "GET", "/api/runs", nil, nil)
	if !errors.Is(err, ErrMajorMismatch) {
		t.Fatalf("after a 404 err = %v, want a major mismatch from the re-read version", err)
	}
	if probes, _, _ := gs.counts(); probes != 2 {
		t.Errorf("server info probed %d times, want 2", probes)
	}
}

func TestMajorGate426ClientTooOld(t *testing.T) {
	setClientVersion(t, "v7.33.0")
	gs, c := newMajorGateClient(t, "v7.33.0")
	gs.set(func(s *majorGateServer) {
		s.apiStatus = http.StatusUpgradeRequired
		s.apiBody = `{"error":"client too old","error_code":"client_too_old"}`
		s.infoBody = serverInfoBody("v7.40.0")
	})
	_, err := c.Call(context.Background(), "GET", "/api/runs", nil, nil)
	var tooOld *ClientTooOldError
	if !errors.As(err, &tooOld) {
		t.Fatalf("err = %v, want *ClientTooOldError", err)
	}
	if !errors.Is(err, ErrClientTooOld) {
		t.Errorf("errors.Is(err, ErrClientTooOld) = false")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUpgradeRequired {
		t.Errorf("the 426 answer does not unwrap as *APIError: %v", err)
	}
	if tooOld.Client != "v7.33.0" || tooOld.Server != "v7.40.0" {
		t.Errorf("ClientTooOldError client=%q server=%q, want v7.33.0 and the re-read v7.40.0", tooOld.Client, tooOld.Server)
	}
	if probes, _, _ := gs.counts(); probes != 2 {
		t.Errorf("server info probed %d times, want 2: a 426 re-reads it", probes)
	}

	// A 426 without the client_too_old code stays the server's answer.
	gs.set(func(s *majorGateServer) { s.apiBody = `{"error":"upgrade"}` })
	_, err = c.Call(context.Background(), "GET", "/api/runs", nil, nil)
	if errors.As(err, &tooOld) || !errors.As(err, &apiErr) || apiErr.Status != http.StatusUpgradeRequired {
		t.Errorf("426 without client_too_old: err = %v, want a plain *APIError", err)
	}
}

func TestReleaseMajor(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"v7.33.0", 7, true},
		{"7.33.0", 7, true},
		{"v8.0.0-rc.1", 8, true},
		{"v10", 10, true},
		{"v8+build", 8, true},
		{" v7.1.0 ", 7, true},
		{"dev", 0, false},
		{"vdev", 0, false},
		{"", 0, false},
		{"v7x.1", 0, false},
		{"v0.0.0", 0, false},
	}
	for _, tc := range cases {
		got, ok := releaseMajor(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("releaseMajor(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMajorGateCancelledProbeIsNotCached(t *testing.T) {
	setClientVersion(t, "v7.34.0")
	t.Setenv(allowMajorMismatchEnv, "")
	t.Setenv(serverInfoTimeoutEnv, "")
	var (
		mu     sync.Mutex
		hang   = true
		probes int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != serverInfoPath {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		mu.Lock()
		probes++
		h := hang
		mu.Unlock()
		if h {
			<-r.Context().Done()
		}
		_, _ = w.Write([]byte(serverInfoBody("v8.0.0")))
	}))
	t.Cleanup(srv.Close)
	c := &RemoteClient{cfg: RemoteConfig{BaseURL: srv.URL, Token: "tok"}, http: srv.Client()}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Call(ctx, "GET", "/api/runs", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first call err = %v, want the caller's own deadline", err)
	}
	mu.Lock()
	hang = false
	mu.Unlock()
	_, err := c.Call(context.Background(), "GET", "/api/runs", nil, nil)
	if !errors.Is(err, ErrMajorMismatch) {
		t.Fatalf("after a cancelled probe err = %v, want a major mismatch: the gate was not re-armed", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if probes != 2 {
		t.Errorf("server info probed %d times, want 2", probes)
	}
}

func TestMajorGateMalformedProbeTimeoutIsAnError(t *testing.T) {
	setClientVersion(t, "v7.34.0")
	gs, c := newMajorGateClient(t, "v8.0.0")
	t.Setenv(serverInfoTimeoutEnv, "bogus")
	_, err := c.Call(context.Background(), "GET", "/api/runs", nil, nil)
	if err == nil || !strings.Contains(err.Error(), serverInfoTimeoutEnv) {
		t.Fatalf("err = %v, want an error naming %s", err, serverInfoTimeoutEnv)
	}
	if _, apiHits, _ := gs.counts(); apiHits != 0 {
		t.Errorf("API saw %d requests, want 0: a malformed bound never waves the request through", apiHits)
	}
}

func TestMajorGatePassesPlaceholderMajorZero(t *testing.T) {
	setClientVersion(t, "v7.34.0")
	gs, c := newMajorGateClient(t, "v0.0.0")
	if _, err := c.Call(context.Background(), "GET", "/api/runs", nil, nil); err != nil {
		t.Fatalf("server built without VERSION (v0.0.0) refused: %v", err)
	}
	if _, apiHits, _ := gs.counts(); apiHits != 1 {
		t.Errorf("API saw %d requests, want 1", apiHits)
	}
}

func TestStatusAnswersUnderMajorMismatch(t *testing.T) {
	setClientVersion(t, "v7.34.0")
	gs, c := newMajorGateClient(t, "v8.0.0")
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status err = %v, want the status to answer with both versions", err)
	}
	if st.Server == nil || st.Server.Version != "v8.0.0" || st.ClientVersion != "v7.34.0" {
		t.Errorf("server = %+v client_version = %q, want v8.0.0 and v7.34.0", st.Server, st.ClientVersion)
	}
	if !strings.Contains(st.MajorMismatch, allowMajorMismatchEnv) {
		t.Errorf("major_mismatch = %q, want the gate's refusal", st.MajorMismatch)
	}
	if probes, apiHits, _ := gs.counts(); probes != 1 || apiHits != 0 {
		t.Errorf("probes=%d apiHits=%d, want 1 and 0: the account is never read from another major", probes, apiHits)
	}
}

func TestStatusPrintUnderMajorMismatch(t *testing.T) {
	setClientVersion(t, "v7.34.0")
	_, c := newMajorGateClient(t, "v8.0.0")
	var out strings.Builder
	if err := RemoteStatusPrint(context.Background(), c, &Printer{W: &out, Format: OutputHuman}); err != nil {
		t.Fatalf("RemoteStatusPrint: %v", err)
	}
	for _, want := range []string{"Account:  not read (remote: client v7.34.0 and server v8.0.0", "Server:   v8.0.0", "Client:   v7.34.0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q does not contain %q", out.String(), want)
		}
	}
}

func TestStatusReadsServerInfoOnce(t *testing.T) {
	setClientVersion(t, "v7.34.0")
	gs, c := newMajorGateClient(t, "v7.40.0")
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Server == nil || st.Server.Version != "v7.40.0" || st.MajorMismatch != "" {
		t.Errorf("server = %+v major_mismatch = %q, want v7.40.0 and none", st.Server, st.MajorMismatch)
	}
	if probes, apiHits, _ := gs.counts(); probes != 1 || apiHits != 1 {
		t.Errorf("probes=%d apiHits=%d, want 1 and 1: the status reads the server's info once", probes, apiHits)
	}
}
