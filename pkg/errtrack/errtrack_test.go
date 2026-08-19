package errtrack

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	sentry "github.com/getsentry/sentry-go"

	"github.com/SocialGouv/iterion/pkg/log"
)

// memTransport is the in-memory sentry.Transport every test installs:
// events are recorded in the process and nothing ever touches the
// network, so the suite needs neither a DSN of a real project nor
// egress.
type memTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (t *memTransport) Configure(sentry.ClientOptions) {}
func (t *memTransport) SendEvent(e *sentry.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, e)
}
func (t *memTransport) Flush(time.Duration) bool              { return true }
func (t *memTransport) FlushWithContext(context.Context) bool { return true }
func (t *memTransport) Close()                                {}

func (t *memTransport) all() []*sentry.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*sentry.Event, len(t.events))
	copy(out, t.events)
	return out
}

// testDSN is syntactically valid and points nowhere — the memTransport
// short-circuits the send, so the host is never resolved.
const testDSN = "https://publickey@localhost/1"

// enable initialises the package against an in-memory transport and
// restores the off state when the test ends.
func enable(t *testing.T, cfg Config) *memTransport {
	t.Helper()
	tr := &memTransport{}
	cfg.DSN = testDSN
	cfg.Transport = tr
	t.Cleanup(func() {
		// The global hub's scope outlives the client, so a breadcrumb
		// left by one test would ride the next one's first event.
		sentry.CurrentHub().Scope().Clear()
		sentry.CurrentHub().BindClient(nil)
		reset()
	})
	sentry.CurrentHub().Scope().Clear()
	reset()
	if !Init(cfg) {
		t.Fatal("Init returned false with a valid DSN and transport")
	}
	return tr
}

func TestDSNUnsetIsACompleteNoOp(t *testing.T) {
	t.Setenv(EnvDSN, "")
	reset()
	t.Cleanup(reset)

	var buf bytes.Buffer
	logger := log.New(log.LevelTrace, &buf)

	if Init(Config{Logger: logger}) {
		t.Fatal("Init reported enabled with no DSN")
	}
	if Enabled() {
		t.Fatal("Enabled() true with no DSN")
	}
	if sentry.CurrentHub().Client() != nil {
		t.Fatal("a sentry client was installed with no DSN")
	}
	// Every helper must be inert, not merely quiet.
	CaptureError(errors.New("boom"), map[string]any{"k": "v"})
	CaptureMessage(sentry.LevelError, "boom", nil)
	CapturePanic("boom")
	AddBreadcrumb(sentry.LevelWarning, "crumb", nil)
	if !Flush() {
		t.Fatal("Flush should succeed trivially when disabled")
	}
	if AttachLogHook(logger) {
		t.Fatal("the log hook must not be installed when tracking is off")
	}
	if got := buf.String(); got != "" {
		t.Fatalf("disabled errtrack wrote to the log: %q", got)
	}
}

func TestInitFailureIsLoudButNotFatal(t *testing.T) {
	reset()
	t.Cleanup(reset)

	var buf bytes.Buffer
	logger := log.New(log.LevelError, &buf)

	if Init(Config{DSN: "not-a-dsn", Logger: logger}) {
		t.Fatal("Init reported enabled with a malformed DSN")
	}
	if Enabled() {
		t.Fatal("Enabled() true after a failed init")
	}
	out := buf.String()
	if !strings.Contains(out, "errtrack:") {
		t.Fatalf("init failure was not logged: %q", out)
	}
}

func TestInitTagsReleaseAndEnvironment(t *testing.T) {
	tr := enable(t, Config{Environment: "staging", Release: "iterion@v1.2.3+abc"})

	CaptureMessage(sentry.LevelError, "tagged", nil)
	Flush()

	events := tr.all()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].Release != "iterion@v1.2.3+abc" {
		t.Errorf("release = %q", events[0].Release)
	}
	if events[0].Environment != "staging" {
		t.Errorf("environment = %q", events[0].Environment)
	}
}

func TestDefaultReleaseCarriesTheBuildStamp(t *testing.T) {
	rel := defaultRelease()
	if !strings.HasPrefix(rel, "iterion@") {
		t.Fatalf("release %q does not name the app and its version", rel)
	}
}

func TestErrorLogBecomesAnEventWithFields(t *testing.T) {
	tr := enable(t, Config{})

	logger := log.New(log.LevelDebug, &bytes.Buffer{})
	if !AttachLogHook(logger) {
		t.Fatal("AttachLogHook returned false while enabled")
	}

	logger.WithField("run_id", "abc123").Error("node %s failed", "implement")
	Flush()

	events := tr.all()
	if len(events) != 1 {
		t.Fatalf("want 1 event from an error log, got %d", len(events))
	}
	ev := events[0]
	if ev.Message != "node implement failed" {
		t.Errorf("message = %q", ev.Message)
	}
	if ev.Level != sentry.LevelError {
		t.Errorf("level = %q", ev.Level)
	}
	ctx, ok := ev.Contexts[contextKey]
	if !ok {
		t.Fatalf("event carries no %q context: %+v", contextKey, ev.Contexts)
	}
	if ctx["run_id"] != "abc123" {
		t.Errorf("run_id field = %v", ctx["run_id"])
	}
}

func TestWarnLogBecomesABreadcrumbNotAnEvent(t *testing.T) {
	tr := enable(t, Config{})

	logger := log.New(log.LevelDebug, &bytes.Buffer{})
	AttachLogHook(logger)

	logger.WithField("node", "review").Warn("retrying")
	Flush()
	if got := len(tr.all()); got != 0 {
		t.Fatalf("a warn line produced %d event(s); it must only leave a breadcrumb", got)
	}

	logger.Error("gave up")
	Flush()

	events := tr.all()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	crumbs := events[0].Breadcrumbs
	if len(crumbs) != 1 {
		t.Fatalf("want 1 breadcrumb on the event, got %d", len(crumbs))
	}
	if crumbs[0].Message != "retrying" {
		t.Errorf("breadcrumb message = %q", crumbs[0].Message)
	}
	if crumbs[0].Level != sentry.LevelWarning {
		t.Errorf("breadcrumb level = %q", crumbs[0].Level)
	}
	if crumbs[0].Data["node"] != "review" {
		t.Errorf("breadcrumb data = %v", crumbs[0].Data)
	}
}

func TestInfoAndDebugLogsProduceNothing(t *testing.T) {
	tr := enable(t, Config{})

	logger := log.New(log.LevelTrace, &bytes.Buffer{})
	AttachLogHook(logger)

	logger.Info("starting")
	logger.Debug("details")
	logger.Trace("noise")
	logger.Error("boom")
	Flush()

	events := tr.all()
	if len(events) != 1 {
		t.Fatalf("want exactly 1 event (the error), got %d", len(events))
	}
	if n := len(events[0].Breadcrumbs); n != 0 {
		t.Fatalf("info/debug/trace left %d breadcrumb(s)", n)
	}
}

func TestCaptureErrorAndPanic(t *testing.T) {
	tr := enable(t, Config{})

	CaptureError(errors.New("disk on fire"), map[string]any{"path": "/tmp/x"})
	CapturePanic("nil map write")
	Flush()

	events := tr.all()
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	if len(events[0].Exception) == 0 || events[0].Exception[0].Value != "disk on fire" {
		t.Errorf("captured error = %+v", events[0].Exception)
	}
	if events[0].Contexts[contextKey]["path"] != "/tmp/x" {
		t.Errorf("context = %+v", events[0].Contexts)
	}
	// A non-error panic value is reported as a message event by the SDK.
	if !strings.Contains(events[1].Message, "nil map write") {
		t.Errorf("captured panic = %+v", events[1])
	}
	// A nil panic value is not an incident.
	CapturePanic(nil)
	CaptureError(nil, nil)
	Flush()
	if got := len(tr.all()); got != 2 {
		t.Fatalf("nil captures produced extra events: %d", got)
	}
}

func TestSecretsAreScrubbedBeforeSend(t *testing.T) {
	tr := enable(t, Config{})

	logger := log.New(log.LevelDebug, &bytes.Buffer{})
	AttachLogHook(logger)

	logger.WithFields(map[string]any{
		"api_key":  "sk-live-0123456789abcdef",
		"endpoint": "https://user:hunter2@ingest.example.com/1",
		"operator": "someone@example.com",
	}).Error("upload failed with token ghp_0123456789abcdefghij")
	Flush()

	events := tr.all()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	ev := events[0]
	if strings.Contains(ev.Message, "ghp_0123456789abcdefghij") {
		t.Errorf("message leaked a token: %q", ev.Message)
	}
	ctx := ev.Contexts[contextKey]
	if ctx["api_key"] != redacted {
		t.Errorf("api_key was not dropped: %v", ctx["api_key"])
	}
	if s, _ := ctx["endpoint"].(string); strings.Contains(s, "hunter2") {
		t.Errorf("endpoint leaked userinfo: %q", s)
	}
	if s, _ := ctx["operator"].(string); strings.Contains(s, "someone@example.com") {
		t.Errorf("operator email was not redacted: %q", s)
	}
}

func TestRedactKeepsUsefulText(t *testing.T) {
	cases := []struct {
		in       string
		wantGone string
		wantKept string
	}{
		{"connect to https://abc123@sentry.example.com/42 failed", "abc123", "sentry.example.com"},
		{"header: Bearer eyJhbGciOiJIUzI1NiJ9", "eyJhbGciOiJIUzI1NiJ9", "header:"},
		{"secret __ITERION_SECRET_FORGE_TOKEN__ missing", "__ITERION_SECRET_FORGE_TOKEN__", "missing"},
		{"plain message with no secret", "", "plain message with no secret"},
	}
	for _, tc := range cases {
		got := Redact(tc.in)
		if tc.wantGone != "" && strings.Contains(got, tc.wantGone) {
			t.Errorf("Redact(%q) = %q — still contains %q", tc.in, got, tc.wantGone)
		}
		if !strings.Contains(got, tc.wantKept) {
			t.Errorf("Redact(%q) = %q — lost %q", tc.in, got, tc.wantKept)
		}
	}
}

func TestUserRecordNeverLeaves(t *testing.T) {
	tr := enable(t, Config{})

	sentry.WithScope(func(scope *sentry.Scope) {
		scope.SetUser(sentry.User{Email: "operator@example.com", ID: "u-1"})
		sentry.CaptureMessage("identified")
	})
	Flush()

	events := tr.all()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].User.Email != "" || events[0].User.ID != "" {
		t.Fatalf("user identity was sent: %+v", events[0].User)
	}
}
