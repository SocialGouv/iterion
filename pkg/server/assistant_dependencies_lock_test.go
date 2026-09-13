package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The dependency update reaches the network — botdeps.Update →
// botinstall.Fetch clones a remote repository. Holding s.stateMu.RLock
// across that stalls every other stateMu reader in the process, because
// sync.RWMutex blocks NEW readers as soon as one writer (a project switch)
// queues behind the clone.
//
// The test pins the handler at the point where the real work would start —
// it holds assistantDependencyMu, so the handler blocks on that Lock — and
// then asserts stateMu is writable throughout. Under the `defer
// s.stateMu.RUnlock()` form this fails on the first poll after the handler
// takes the read lock.
func TestAssistantDependencyUpdateDoesNotHoldStateMuAcrossTheFetch(t *testing.T) {
	s := New(Config{DisableAuth: true, SkipProjectRegistration: true, WorkDir: t.TempDir()}, iterlog.Nop())

	// Hold the action lock so the handler parks immediately after the
	// validation that used to sit under the read lock.
	s.assistantDependencyMu.Lock()

	body := `{"name":"shared","ref":"0123456789abcdef0123456789abcdef01234567","message":"bump shared"}`
	req := httptest.NewRequest(http.MethodPost, "/api/assistant/dependencies/bots/update", strings.NewReader(body))
	rec := httptest.NewRecorder()
	started := make(chan struct{})
	go func() {
		close(started)
		s.handleAssistantDependencyBotsUpdate(rec, req)
	}()
	<-started

	// Poll for longer than the handler needs to reach the parked Lock. Every
	// sample must find stateMu free: a single held read lock is the defect.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !s.stateMu.TryLock() {
			s.assistantDependencyMu.Unlock()
			t.Fatal("stateMu is held while the dependency update waits on its own lock — the read lock spans the network fetch")
		}
		s.stateMu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}

	s.assistantDependencyMu.Unlock()
}

// The snapshot must still refuse the two shapes the read lock guarded: a
// cloud deployment and a server with no project workspace.
func TestAssistantDependencyUpdateRefusesCloudAndWorkspacelessServers(t *testing.T) {
	body := `{"name":"shared","ref":"0123456789abcdef0123456789abcdef01234567","message":"bump shared"}`
	for _, tc := range []struct {
		name string
		cfg  Config
		want int
	}{
		{"cloud", Config{DisableAuth: true, SkipProjectRegistration: true, Mode: "cloud", WorkDir: t.TempDir()}, http.StatusForbidden},
		{"no workspace", Config{DisableAuth: true, SkipProjectRegistration: true}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.cfg, iterlog.Nop())
			req := httptest.NewRequest(http.MethodPost, "/api/assistant/dependencies/bots/update", strings.NewReader(body))
			rec := httptest.NewRecorder()
			s.handleAssistantDependencyBotsUpdate(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
