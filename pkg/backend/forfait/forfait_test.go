package forfait

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDecide(t *testing.T) {
	cases := []struct {
		name        string
		u           Usage
		cap         float64
		wantBlocked bool
	}{
		{"both low", Usage{FiveHour: 10, SevenDay: 20}, 85, false},
		{"5h over cap", Usage{FiveHour: 90, SevenDay: 20}, 85, true},
		{"7d over cap", Usage{FiveHour: 10, SevenDay: 88}, 85, true},
		{"exactly at cap blocks", Usage{FiveHour: 85, SevenDay: 0}, 85, true},
		{"just under cap ok", Usage{FiveHour: 84.9, SevenDay: 84.9}, 85, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decide(c.u, c.cap)
			if d.Blocked != c.wantBlocked {
				t.Errorf("Decide(%+v, %v).Blocked = %v, want %v", c.u, c.cap, d.Blocked, c.wantBlocked)
			}
			if d.Skipped {
				t.Errorf("Decide should never Skip, got Skipped=true")
			}
			if d.Reason == "" {
				t.Errorf("Decide must carry a Reason")
			}
		})
	}
}

// stubDoer serves a canned usage JSON so the fetch+decide path is testable
// without a live endpoint.
type stubDoer struct {
	status int
	body   string
	gotAB  string // captured anthropic-beta header
	gotTok string // captured Authorization header
}

func (s *stubDoer) Do(req *http.Request) (*http.Response, error) {
	s.gotAB = req.Header.Get("anthropic-beta")
	s.gotTok = req.Header.Get("Authorization")
	rec := httptest.NewRecorder()
	rec.WriteHeader(s.status)
	_, _ = rec.WriteString(s.body)
	return rec.Result(), nil
}

func TestCheck_StubbedUsage_Blocked(t *testing.T) {
	tmp := writeCreds(t, "tok-secret")
	doer := &stubDoer{status: 200, body: `{"five_hour":{"utilization":92},"seven_day":{"utilization":40}}`}

	d := check(context.Background(), 85, func() string { return tmp }, "", doer)
	if !d.Blocked || d.Skipped {
		t.Fatalf("expected Blocked, got %+v", d)
	}
	if doer.gotAB != oauthBetaHeader {
		t.Errorf("anthropic-beta header = %q, want %q", doer.gotAB, oauthBetaHeader)
	}
	if doer.gotTok != "Bearer tok-secret" {
		t.Errorf("Authorization header = %q, want Bearer tok-secret", doer.gotTok)
	}
}

func TestCheck_StubbedUsage_UnderCapProceeds(t *testing.T) {
	tmp := writeCreds(t, "tok")
	doer := &stubDoer{status: 200, body: `{"five_hour":{"utilization":10},"seven_day":{"utilization":12}}`}
	d := check(context.Background(), 85, func() string { return tmp }, "", doer)
	if d.Blocked || d.Skipped {
		t.Fatalf("expected proceed (not blocked, not skipped), got %+v", d)
	}
}

func TestCheck_DegradesToSkip(t *testing.T) {
	tmp := writeCreds(t, "tok")

	t.Run("disabled cap", func(t *testing.T) {
		d := check(context.Background(), 0, func() string { return tmp }, "", &stubDoer{status: 200, body: "{}"})
		if !d.Skipped {
			t.Fatalf("cap<=0 must Skip, got %+v", d)
		}
	})
	t.Run("api key set = not forfait", func(t *testing.T) {
		d := check(context.Background(), 85, func() string { return tmp }, "sk-ant-xxx", &stubDoer{status: 200, body: "{}"})
		if !d.Skipped {
			t.Fatalf("ANTHROPIC_API_KEY set must Skip, got %+v", d)
		}
	})
	t.Run("no token", func(t *testing.T) {
		d := check(context.Background(), 85, func() string { return t.TempDir() }, "", &stubDoer{status: 200, body: "{}"})
		if !d.Skipped {
			t.Fatalf("missing creds must Skip, got %+v", d)
		}
	})
	t.Run("endpoint 500 unreachable", func(t *testing.T) {
		d := check(context.Background(), 85, func() string { return tmp }, "", &stubDoer{status: 500, body: "boom"})
		if !d.Skipped {
			t.Fatalf("HTTP 500 must Skip (best-effort), got %+v", d)
		}
	})
}

func TestFetchUsage_ParsesLiveShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("missing Authorization header")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"five_hour": map[string]any{"utilization": 33.5},
			"seven_day": map[string]any{"utilization": 71.0},
		})
	}))
	defer srv.Close()

	// Point fetchUsage at the test server by temporarily swapping the doer's
	// target via a request rewrite is overkill; instead assert the parser on
	// a direct call using a doer that redirects to the test server URL.
	doer := &redirectDoer{target: srv.URL, client: srv.Client()}
	u, err := fetchUsage(context.Background(), "tok", doer)
	if err != nil {
		t.Fatalf("fetchUsage: %v", err)
	}
	if u.FiveHour != 33.5 || u.SevenDay != 71.0 {
		t.Errorf("parsed usage = %+v, want {33.5 71}", u)
	}
}

// redirectDoer rewrites the outbound request to the test server, preserving
// headers, so fetchUsage can hit an httptest server without exposing the real
// endpoint constant.
type redirectDoer struct {
	target string
	client *http.Client
}

func (d *redirectDoer) Do(req *http.Request) (*http.Response, error) {
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, d.target, req.Body)
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header
	return d.client.Do(newReq)
}

func writeCreds(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	body, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": token},
	})
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}
