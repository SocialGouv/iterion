package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
// then asserts stateMu is writable after the short snapshot has completed.
// Under the `defer s.stateMu.RUnlock()` form the writer stays blocked.
func TestAssistantDependencyUpdateDoesNotHoldStateMuAcrossTheFetch(t *testing.T) {
	s := New(Config{DisableAuth: true, SkipProjectRegistration: true, WorkDir: t.TempDir()}, iterlog.Nop())

	// Hold the action lock so the handler parks immediately after the
	// validation that used to sit under the read lock.
	s.assistantDependencyMu.Lock()

	// Reading the request body happens after the brief configuration snapshot.
	// Signal there instead of racing TryLock against that legitimate RLock.
	body := &dependencyRequestBodyBarrier{Reader: strings.NewReader(`{"name":"shared","ref":"0123456789abcdef0123456789abcdef01234567","message":"bump shared"}`), read: make(chan struct{})}
	req := httptest.NewRequest(http.MethodPost, "/api/assistant/dependencies/bots/update", body)
	rec := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() { s.handleAssistantDependencyBotsUpdate(rec, req); close(finished) }()
	defer func() {
		s.assistantDependencyMu.Unlock()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("dependency handler did not finish after releasing its action lock")
		}
	}()
	select {
	case <-body.read:
	case <-time.After(5 * time.Second):
		t.Fatal("dependency handler did not read its body")
	}
	writable := make(chan struct{})
	go func() { s.stateMu.Lock(); close(writable); s.stateMu.Unlock() }()
	select {
	case <-writable:
	case <-time.After(time.Second):
		t.Fatal("stateMu remains held while the dependency action is blocked")
	}
	select {
	case <-finished:
		t.Fatal("dependency handler bypassed its action lock")
	default:
	}

}

type dependencyRequestBodyBarrier struct {
	io.Reader
	once sync.Once
	read chan struct{}
}

func (b *dependencyRequestBodyBarrier) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.read) })
	return b.Reader.Read(p)
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
