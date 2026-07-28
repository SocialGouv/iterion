package pisdk

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScanLines(t *testing.T) {
	t.Run("splits on LF and trims CR", func(t *testing.T) {
		var got []string
		err := ScanLines(strings.NewReader("a\r\nb\nc"), func(l string) { got = append(got, l) })
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"a", "b", "c"} // the trailing fragment is delivered too
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("lines = %v, want %v", got, want)
		}
	})

	// U+2028 / U+2029 are legal inside JSON strings; a reader that also broke
	// on them would split one record into two invalid fragments.
	t.Run("does not split on unicode line separators", func(t *testing.T) {
		var got []string
		if err := ScanLines(strings.NewReader("x y z\n"), func(l string) { got = append(got, l) }); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("lines = %d, want 1 (a separator inside the record split it)", len(got))
		}
	})

	// bufio.Scanner's default 64 KiB cap would truncate here.
	t.Run("no line-length cap", func(t *testing.T) {
		huge := strings.Repeat("z", 4<<20) // 4 MiB
		var got string
		if err := ScanLines(strings.NewReader(huge+"\n"), func(l string) { got = l }); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(huge) {
			t.Errorf("got %d bytes, want %d intact", len(got), len(huge))
		}
	})
}

func TestMarshalLine(t *testing.T) {
	line, err := MarshalLine(Command{Type: CmdPrompt, Message: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(line), "\n") {
		t.Error("record must be newline-terminated")
	}
	// omitempty keeps the payload to the variant's own keys — pi validates by
	// shape, so a stray null field is a real risk.
	for _, absent := range []string{"provider", "modelId", "level", "mode", "enabled"} {
		if strings.Contains(string(line), `"`+absent+`"`) {
			t.Errorf("prompt command leaked an unrelated field %q: %s", absent, line)
		}
	}
}

func TestUIRequestExpectsReply(t *testing.T) {
	for _, m := range []string{UIMethodSelect, UIMethodConfirm, UIMethodInput, UIMethodEditor} {
		if !(UIRequest{Method: m}).ExpectsReply() {
			t.Errorf("%s must expect a reply — the extension is blocked on it", m)
		}
	}
	for _, m := range []string{UIMethodNotify, UIMethodSetStatus, UIMethodSetWidget, UIMethodSetTitle} {
		if (UIRequest{Method: m}).ExpectsReply() {
			t.Errorf("%s is fire-and-forget; replying to it is a protocol error", m)
		}
	}
}

// ---------------------------------------------------------------------------
// Real-binary tests. The port is only as good as the stream it decodes, so
// these drive an actual `pi --mode rpc` with the scripted mock provider —
// no credentials, no network, no cost.
// ---------------------------------------------------------------------------

func requirePi(t *testing.T) (bin, ext string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only harness")
	}
	bin = os.Getenv("ITERION_PI_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("pi"); err != nil {
			t.Skip("pi not installed — skipping the real-binary RPC test " +
				"(npm i -g @earendil-works/pi-coding-agent, or set ITERION_PI_BIN)")
		}
	}
	abs, err := filepath.Abs(filepath.Join("testdata", "mock-provider.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("mock provider missing: %v", err)
	}
	return bin, abs
}

// newLiveClient starts a client against the real binary, with the scripted
// provider and a pinned agent dir so the operator's own pi config is untouched.
func newLiveClient(t *testing.T, opts *ClientOptions) *Client {
	t.Helper()
	bin, ext := requirePi(t)

	agentDir := t.TempDir()
	settings := `{"retry":{"enabled":false}}`
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	opts.Binary = bin
	opts.Dir = t.TempDir()
	opts.Args = append([]string{
		"--no-approve", "--no-context-files",
		"-e", ext, "--model", "mock/scripted",
		"--session-dir", filepath.Join(agentDir, "sessions"),
	}, opts.Args...)
	opts.Env = append(os.Environ(),
		"PI_CODING_AGENT_DIR="+agentDir,
		"ITERION_PI_MOCK_TEXT=live rpc ok",
	)
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 30 * time.Second
	}

	c := NewClient(*opts)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestClientLiveHandshake is the load-bearing assertion: get_state before any
// token is spent reveals the resolved model and session id, which is strictly
// better than print mode (where a bad --model costs a whole process start).
func TestClientLiveHandshake(t *testing.T) {
	c := newLiveClient(t, &ClientOptions{})

	state, err := c.GetState(context.Background())
	if err != nil {
		t.Fatalf("GetState: %v (stderr: %s)", err, c.Stderr())
	}
	if state.SessionID == "" {
		t.Error("SessionID empty — the get_state payload shape drifted")
	}
	if state.Model == nil || state.Model.ID != "scripted" {
		t.Errorf("Model = %+v, want the scripted mock resolved pre-flight", state.Model)
	}
	if state.IsStreaming {
		t.Error("IsStreaming true before any prompt")
	}
}

// TestClientLivePromptSettles pins the semantic that is easiest to get wrong:
// the reply to `prompt` means ACCEPTED, and completion is agent_settled.
func TestClientLivePromptSettles(t *testing.T) {
	var (
		mu       sync.Mutex
		types    []string
		settled  = make(chan struct{})
		lastText string
	)
	c := newLiveClient(t, &ClientOptions{
		OnEvent: func(ev Event) {
			mu.Lock()
			types = append(types, ev.Type)
			if ev.Type == EventMessageEnd && ev.Message != nil && ev.Message.IsAssistant() {
				lastText = ev.Message.Text()
			}
			mu.Unlock()
			if ev.Type == EventAgentSettled {
				close(settled)
			}
		},
	})

	if err := c.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v (stderr: %s)", err, c.Stderr())
	}

	select {
	case <-settled:
	case <-time.After(60 * time.Second):
		mu.Lock()
		seen := strings.Join(types, ",")
		mu.Unlock()
		t.Fatalf("no agent_settled within 60s; saw [%s] (stderr: %s)", seen, c.Stderr())
	}

	mu.Lock()
	defer mu.Unlock()
	if lastText != "live rpc ok" {
		t.Errorf("assistant text = %q, want the scripted reply", lastText)
	}
	// The lifecycle the event→hook mapping depends on.
	for _, want := range []string{EventAgentStart, EventTurnStart, EventMessageEnd, EventTurnEnd, EventAgentEnd} {
		if !containsString(types, want) {
			t.Errorf("event %q never arrived; saw [%s]", want, strings.Join(types, ","))
		}
	}
	// Ordering: settled must be last.
	if types[len(types)-1] != EventAgentSettled {
		t.Errorf("last event = %q, want agent_settled to close the turn", types[len(types)-1])
	}
}

// TestClientLiveSessionStats covers the authoritative accounting path that
// replaces per-message accumulation.
func TestClientLiveSessionStats(t *testing.T) {
	settled := make(chan struct{})
	var once sync.Once
	c := newLiveClient(t, &ClientOptions{
		OnEvent: func(ev Event) {
			if ev.Type == EventAgentSettled {
				once.Do(func() { close(settled) })
			}
		},
	})

	if err := c.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	select {
	case <-settled:
	case <-time.After(60 * time.Second):
		t.Fatalf("turn never settled (stderr: %s)", c.Stderr())
	}

	stats, err := c.SessionStats(context.Background())
	if err != nil {
		t.Fatalf("SessionStats: %v (stderr: %s)", err, c.Stderr())
	}
	if stats.Tokens.Total <= 0 {
		t.Errorf("Tokens.Total = %d, want > 0 — the get_session_stats shape drifted", stats.Tokens.Total)
	}
}

// TestClientLiveRequestOnDeadProcess guards the failure mode that would
// otherwise cost a caller its whole timeout: once pi is gone, a request must
// fail immediately with the exit error rather than hang.
func TestClientLiveRequestOnDeadProcess(t *testing.T) {
	c := newLiveClient(t, &ClientOptions{})
	if _, err := c.GetState(context.Background()); err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Logf("Close reported: %v", err) // a clean SIGTERM exit is not a failure
	}

	start := time.Now()
	_, err := c.GetState(context.Background())
	if err == nil {
		t.Fatal("expected an error after Close")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s to fail — a dead process must not cost the full request timeout", elapsed)
	}
}

// TestClientLiveUnknownUIRequestIsCancelled pins the safe default: a UI
// request the host does not recognise is cancelled, which neither blocks the
// agent nor fabricates an answer.
func TestClientLiveUnknownUIRequestIsCancelled(t *testing.T) {
	requirePi(t)

	var got []UIRequest
	var mu sync.Mutex
	c := newLiveClient(t, &ClientOptions{
		OnUIRequest: func(req UIRequest) *UIResponse {
			mu.Lock()
			got = append(got, req)
			mu.Unlock()
			return nil // decline → the client must cancel
		},
	})

	// No extension in this fixture raises a dialog, so this asserts the wiring
	// compiles and stays inert rather than firing spuriously.
	if _, err := c.GetState(context.Background()); err != nil {
		t.Fatalf("GetState: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Errorf("unexpected UI requests: %+v", got)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
