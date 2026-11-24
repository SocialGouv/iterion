package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// dslPost serves one POST against the server's mux and returns the recorder.
func dslPost(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

// newAdmissionTestServer builds a Server with the gate resolved from the
// test's environment (t.Setenv BEFORE the call), exactly like newTestServer.
func newAdmissionTestServer(t *testing.T) *Server {
	t.Helper()
	workDir := t.TempDir()
	return New(Config{
		WorkDir:                 workDir,
		StoreDir:                workDir + "/.iterion",
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelError, os.Stderr))
}

// TestReadAdmission_RefusesPastTheCeiling is the #2693 witness through the
// real route: while the aggregate bound is held, POST /api/parse is refused
// 503 + Retry-After after the refusal grace — never served on top of the
// readings already running. Without the gate call in the handler this
// request answers 200.
func TestReadAdmission_RefusesPastTheCeiling(t *testing.T) {
	t.Setenv(EnvReadAdmissionMaxBytes, "8") // any reading clamps to the whole ceiling
	t.Setenv(EnvReadAdmissionWait, "50ms")
	srv := newAdmissionTestServer(t)
	if srv.readAdmission == nil {
		t.Fatal("the gate is nil — the env ceiling did not resolve")
	}
	// Occupy the whole aggregate, as concurrent readings at the edge would.
	if err := srv.readAdmission.sem.Acquire(context.Background(), 8); err != nil {
		t.Fatalf("occupy the gate: %v", err)
	}
	defer srv.readAdmission.sem.Release(8)

	rec := dslPost(t, srv, "/api/parse", `{"source":"agent a:\n  model: \"x/y\"\n\nworkflow main:\n  a -> done\n"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("parse past the aggregate bound = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("the 503 carries no Retry-After")
	}
	if !strings.Contains(rec.Body.String(), "dsl_read_admission") {
		t.Fatalf("the refusal body = %q, want the stable dsl_read_admission code", rec.Body.String())
	}
}

// TestReadAdmission_ServesUnderTheCeiling is the twin: the same request on
// an unheld gate is served — the bound refuses CONCURRENCY, not readings.
func TestReadAdmission_ServesUnderTheCeiling(t *testing.T) {
	t.Setenv(EnvReadAdmissionMaxBytes, "8")
	t.Setenv(EnvReadAdmissionWait, "50ms")
	srv := newAdmissionTestServer(t)

	rec := dslPost(t, srv, "/api/parse", `{"source":"agent a:\n  model: \"x/y\"\n\nworkflow main:\n  a -> done\n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("parse under the bound = %d (%s), want 200", rec.Code, rec.Body.String())
	}
}

// TestReadAdmission_DisabledByZero pins the escape hatch: 0 removes the
// bound — the same request stream answers 200 with the gate fully held,
// because there is no gate.
func TestReadAdmission_DisabledByZero(t *testing.T) {
	t.Setenv(EnvReadAdmissionMaxBytes, "0")
	srv := newAdmissionTestServer(t)
	if srv.readAdmission != nil {
		t.Fatal("a 0 ceiling must resolve to no gate — the bound's override")
	}
}

// TestReadAdmission_InvalidKeepsTheDefault: an operator's typo must not
// silently widen or remove the bound — the default stands.
func TestReadAdmission_InvalidKeepsTheDefault(t *testing.T) {
	t.Setenv(EnvReadAdmissionMaxBytes, "a lot")
	srv := newAdmissionTestServer(t)
	if srv.readAdmission == nil || srv.readAdmission.capacity != defaultReadAdmissionMaxBytes {
		t.Fatalf("capacity = %+v, want the default %d", srv.readAdmission, int64(defaultReadAdmissionMaxBytes))
	}
}

// TestReadAdmissionGate_Semantics pins the gate itself: the refusal grace
// bounds the wait, a release re-admits, a 0 grace refuses immediately, and
// an input heavier than the whole ceiling is admitted ALONE — never refused
// outright (the read budget already bounds one reading) — and never beside
// another reading.
func TestReadAdmissionGate_Semantics(t *testing.T) {
	g := &readAdmissionGate{sem: semaphore.NewWeighted(10), capacity: 10, wait: 50 * time.Millisecond}

	// Past the ceiling and held: refused after the grace, not before.
	release, refused := g.admit(context.Background(), 4, nil)
	if refused {
		t.Fatal("an empty gate refused a reading under the ceiling")
	}
	start := time.Now()
	if _, refused := g.admit(context.Background(), 16, nil); !refused {
		t.Fatal("a held gate admitted a second reading over the ceiling")
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("the refusal came after %v, want about the 50ms grace", elapsed)
	}

	// The release re-admits.
	release()
	release, refused = g.admit(context.Background(), 16, nil)
	if refused {
		t.Fatal("a drained gate refused a reading under the ceiling")
	}
	release()

	// Overweight: admitted alone into an empty gate, clamped to the ceiling.
	var infos []string
	heavyRelease, refused := g.admit(context.Background(), 1<<20, func(f string, a ...any) { infos = append(infos, f) })
	if refused {
		t.Fatal("an empty gate refused an overweight reading outright — the bound is against concurrency, not size")
	}
	if len(infos) != 1 {
		t.Fatalf("an overweight admission said nothing — want the clamp said at info: %v", infos)
	}
	// ...and refused while ANOTHER reading holds the gate, even past the grace.
	if _, refused := g.admit(context.Background(), 4, nil); !refused {
		t.Fatal("the gate over-admitted: a reading joined the clamped overweight one")
	}
	heavyRelease()

	// No grace: the answer is immediate.
	g0 := &readAdmissionGate{sem: semaphore.NewWeighted(10), capacity: 10, wait: 0}
	release0, refused := g0.admit(context.Background(), 4, nil)
	if refused {
		t.Fatal("0-grace: an empty gate refused")
	}
	start = time.Now()
	if _, refused := g0.admit(context.Background(), 8, nil); !refused {
		t.Fatal("0-grace: a held gate admitted past the ceiling")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("0-grace refusal took %v, want immediate", elapsed)
	}
	release0()
}

// TestAdmissionWeight maps the weight rule: proportional to the input, one
// unit for an empty one, and the dense edge — the budget's worst reading
// (~0.5–1 MiB of source, ~533 MiB measured RSS) — lands at about the
// default ceiling, so two of them never overlap.
func TestAdmissionWeight(t *testing.T) {
	if got := admissionWeight(0); got != 1 {
		t.Fatalf("weight(0) = %d, want 1", got)
	}
	if got := admissionWeight(1 << 20); got != 1<<20*readWeightPerInputByte {
		t.Fatalf("weight(1 MiB) = %d, want %d", got, 1<<20*readWeightPerInputByte)
	}
	if w := admissionWeight(1 << 20); w < defaultReadAdmissionMaxBytes/2 || w > 2*defaultReadAdmissionMaxBytes {
		t.Fatalf("the dense budget edge weighs %d against a default ceiling of %d — the two must be of one order or the bound is decorative", w, defaultReadAdmissionMaxBytes)
	}
}
