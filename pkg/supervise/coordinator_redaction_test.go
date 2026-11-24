package supervise

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A coordinator logs the steer it enqueued — model-derived text — on the
// RUN's logger, and that line is the one place in iterion that CUTS text
// before logging it. The cut runs AFTER the run's matcher: a value split
// by the cut is a value no matcher recognises, so cutting first publishes
// its head (iterlog.WithRedactor's own trap, iterion#2822).
func TestTheSteerLineRedactsBeforeItCuts(t *testing.T) {
	const value = "hunter2-9f8e7d6c5b4a-db-password"
	placeholder := secretguard.PlaceholderForName("db")
	// The production pass: the run's guard over one declared value, the
	// same function runview.BuildExecutor installs on the run's logger.
	guard := secretguard.New([]secretguard.Secret{
		{Name: "db", Value: value, Placeholder: placeholder},
	}, secretguard.DefaultConfig())

	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &buf)
	logger.SetRedactor(guard.Redact)

	inj := &recordInjector{}
	c := New(&fakeObserver{ch: make(chan *store.Event)}, inj, "r1", Spec{Name: "wd"}, &stubEval{}, logger)
	if c == nil {
		t.Fatal("New returned nil")
	}

	// The steer straddles the 120-byte cut: the value starts at byte 110,
	// so a cut taken first keeps exactly the head the cut leaves whole.
	steer := strings.Repeat("re-read the diff. ", 7)[:110] + value + " — then rerun"
	at := strings.Index(steer, value)
	if len(steer) <= 120 || at >= 120 || at+len(value) <= 120 {
		t.Fatalf("the premise of this test is wrong: the value must straddle the cut (len=%d, at=%d)", len(steer), at)
	}
	head := value[:120-at]
	if err := c.inject(steer, "test-trigger"); err != nil {
		t.Fatalf("inject: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "steered run r1") {
		t.Fatalf("the steer line never reached the log, so this test proves nothing: %q", got)
	}
	if strings.Contains(got, head) {
		t.Errorf("the cut published the value's head %q — the line cut before the pass: %q", head, got)
	}
	if !strings.Contains(got, placeholder[:10]) {
		t.Errorf("the logged steer shows no placeholder where the value was: %q", got)
	}
}
