package tools

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// minimal 1x1 transparent PNG used as fixture content.
var pngFixture = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
	0x42, 0x60, 0x82,
}

func TestReadImage_Base64FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.png")
	if err := os.WriteFile(path, pngFixture, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	res, err := ExecuteReadImage(context.Background(), map[string]any{"path": path})
	if err != nil {
		t.Fatalf("ExecuteReadImage: %v", err)
	}
	if len(res.Blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(res.Blocks))
	}
	block := res.Blocks[0]
	if block.Type != "image" || block.Source == nil {
		t.Fatalf("expected image block with Source, got %+v", block)
	}
	if block.Source.Type != "base64" {
		t.Errorf("expected Source.Type=base64, got %q", block.Source.Type)
	}
	if block.Source.MediaType != "image/png" {
		t.Errorf("expected MediaType=image/png, got %q", block.Source.MediaType)
	}
	if block.Source.Data == "" {
		t.Errorf("expected non-empty base64 data")
	}
	if res.Description == "" {
		t.Errorf("expected description to default to filename")
	}
}

func TestReadImage_RejectsLargeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.png")
	// 6 MB of zeros — past the 5 MB cap.
	if err := os.WriteFile(path, make([]byte, 6*1024*1024), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := ExecuteReadImage(context.Background(), map[string]any{"path": path})
	if err == nil {
		t.Fatal("expected error on oversized file")
	}
	if !strings.Contains(err.Error(), "5 MB") {
		t.Errorf("expected size-limit error, got %v", err)
	}
}

func TestReadImage_RejectsReadPastCap(t *testing.T) {
	// The stat-size pre-check passes (declared at or under the cap) but the
	// bytes actually read exceed it — a file grown between stat and read
	// must not bypass the 5 MB limit.
	_, err := readImageData(bytes.NewReader(make([]byte, maxImageBytes+1)), "grown.png")
	if err == nil {
		t.Fatal("expected error on read past the cap")
	}
	if !strings.Contains(err.Error(), "5 MB") {
		t.Errorf("expected size-limit error, got %v", err)
	}
}

func TestReadImage_RejectsSymlinkPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink confinement is enforced by the unix openat walk")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real.png")
	if err := os.WriteFile(real, pngFixture, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	link := filepath.Join(dir, "link.png")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// A symlinked final component is how a swap between the upstream gate's
	// path resolution and this open would surface; it must be refused, not
	// followed.
	_, err := ExecuteReadImage(context.Background(), map[string]any{"path": link})
	if err == nil {
		t.Fatal("expected symlinked path to be refused")
	}
}

func TestReadImage_RejectsSymlinkedDirComponent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink confinement is enforced by the unix openat walk")
	}
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "img.png"), pngFixture, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	linkDir := filepath.Join(dir, "linkdir")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// A directory component swapped for a symlink mid-walk is the TOCTOU the
	// openat discipline closes: the walk must refuse, not escape through it.
	_, err := ExecuteReadImage(context.Background(), map[string]any{
		"path": filepath.Join(linkDir, "img.png"),
	})
	if err == nil {
		t.Fatal("expected path through symlinked directory to be refused")
	}
}

func TestReadImage_RejectsHTTPURL(t *testing.T) {
	_, err := ExecuteReadImage(context.Background(), map[string]any{"url": "http://example.com/x.png"})
	if err == nil {
		t.Fatal("expected error on plain http url")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("expected https error, got %v", err)
	}
}

func TestReadImage_RejectsFileURL(t *testing.T) {
	_, err := ExecuteReadImage(context.Background(), map[string]any{"url": "file:///etc/passwd"})
	if err == nil {
		t.Fatal("expected error on file scheme")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("expected https error, got %v", err)
	}
}

func TestReadImage_HTTPSFetch(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngFixture)
	}))
	t.Cleanup(srv.Close)

	prev := http.DefaultClient
	http.DefaultClient = srv.Client()
	t.Cleanup(func() { http.DefaultClient = prev })

	res, err := ExecuteReadImage(context.Background(), map[string]any{"url": srv.URL + "/img.png"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(res.Blocks) != 1 || res.Blocks[0].Source == nil {
		t.Fatalf("expected one image block, got %+v", res)
	}
	if res.Blocks[0].Source.MediaType != "image/png" {
		t.Errorf("expected image/png, got %q", res.Blocks[0].Source.MediaType)
	}
}

func TestReadImage_RejectsHTTPSToHTTPRedirect(t *testing.T) {
	// Plain HTTP target — the "evil" host the redirect would land on.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("plain http server should never be hit; redirect must be blocked")
		_, _ = w.Write(pngFixture)
	}))
	defer plain.Close()

	// HTTPS origin that 302s to the plain http target. The starting URL
	// passes the scheme check, so without CheckRedirect we'd silently
	// follow into plaintext.
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/img.png", http.StatusFound)
	}))
	defer tls.Close()

	prev := http.DefaultClient
	http.DefaultClient = tls.Client()
	t.Cleanup(func() { http.DefaultClient = prev })

	_, err := ExecuteReadImage(context.Background(), map[string]any{"url": tls.URL + "/start"})
	if err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("expected redirect-to-non-https error, got %v", err)
	}
}

func TestReadImage_PathAndURL_Mutex(t *testing.T) {
	_, err := ExecuteReadImage(context.Background(), map[string]any{
		"path": "/tmp/x.png",
		"url":  "https://example.com/y.png",
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

// fakeRunner records every command issued and replays canned outputs/errors
// per binary. It also lets tests toggle a fake DISPLAY and decide which
// binaries appear on PATH.
type fakeRunner struct {
	display   bool
	available map[string]bool
	stdoutFor map[string][]byte
	errFor    map[string]error
	calls     []fakeCall
}

type fakeCall struct {
	name string
	args []string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		display:   true,
		available: map[string]bool{"xdotool": true, "import": true},
		stdoutFor: map[string][]byte{},
		errFor:    map[string]error{},
	}
}

func (f *fakeRunner) lookPath(name string) string {
	if f.available[name] {
		return "/fake/" + name
	}
	return ""
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, fakeCall{name: name, args: append([]string(nil), args...)})
	if err, ok := f.errFor[name]; ok && err != nil {
		return nil, err
	}
	return f.stdoutFor[name], nil
}

func (f *fakeRunner) hasDisplay() bool { return f.display }

// withFakeRunner installs r as the package-level runner for the duration of
// the test, restoring the previous value on cleanup.
func withFakeRunner(t *testing.T, r computerUseRunner) {
	t.Helper()
	prev := runnerOverride
	runnerOverride = r
	t.Cleanup(func() { runnerOverride = prev })
}

func TestComputerUseScreenshotEmitsImageSource(t *testing.T) {
	r := newFakeRunner()
	r.stdoutFor["import"] = pngFixture
	withFakeRunner(t, r)

	res, err := ExecuteComputerUse(context.Background(), map[string]any{"action": "screenshot"})
	if err != nil {
		t.Fatalf("ExecuteComputerUse: %v", err)
	}
	if len(res.Blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(res.Blocks))
	}
	src := res.Blocks[0].Source
	if src == nil || src.Type != "base64" || src.MediaType != "image/png" {
		t.Fatalf("expected base64 image/png source, got %+v", src)
	}
	if src.Data == "" {
		t.Errorf("expected non-empty base64 data")
	}
	if len(r.calls) != 1 || r.calls[0].name != "import" {
		t.Errorf("expected single import call, got %+v", r.calls)
	}
	wantArgs := []string{"-window", "root", "png:-"}
	if !strSliceEq(r.calls[0].args, wantArgs) {
		t.Errorf("import args = %v, want %v", r.calls[0].args, wantArgs)
	}
}

func TestComputerUseClickInvokesXdotool(t *testing.T) {
	r := newFakeRunner()
	withFakeRunner(t, r)

	cases := []struct {
		action string
		button string
		repeat string
	}{
		{"left_click", "1", "1"},
		{"middle_click", "2", "1"},
		{"right_click", "3", "1"},
		{"double_click", "1", "2"},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			r.calls = nil
			_, err := ExecuteComputerUse(context.Background(), map[string]any{
				"action":     tc.action,
				"coordinate": []any{float64(120), float64(240)},
			})
			if err != nil {
				t.Fatalf("ExecuteComputerUse(%s): %v", tc.action, err)
			}
			if len(r.calls) != 1 || r.calls[0].name != "xdotool" {
				t.Fatalf("expected single xdotool call, got %+v", r.calls)
			}
			args := r.calls[0].args
			want := []string{"mousemove", "--sync", "120", "240", "click", "--repeat", tc.repeat, tc.button}
			if !strSliceEq(args, want) {
				t.Errorf("xdotool args = %v, want %v", args, want)
			}
		})
	}
}

func TestComputerUseTypeEscapesShellArgs(t *testing.T) {
	r := newFakeRunner()
	withFakeRunner(t, r)

	// Input that would be dangerous if interpolated through a shell.
	dangerous := `; rm -rf / "$(whoami)" 'foo'`
	_, err := ExecuteComputerUse(context.Background(), map[string]any{
		"action": "type",
		"text":   dangerous,
	})
	if err != nil {
		t.Fatalf("ExecuteComputerUse(type): %v", err)
	}
	if len(r.calls) != 1 || r.calls[0].name != "xdotool" {
		t.Fatalf("expected single xdotool call, got %+v", r.calls)
	}
	args := r.calls[0].args
	// The text must arrive as a single argv entry after the "--" terminator.
	// argv[len-2] should be "--" and argv[len-1] should be the literal text.
	if len(args) < 2 || args[len(args)-2] != "--" || args[len(args)-1] != dangerous {
		t.Errorf("expected literal text after \"--\" terminator, got args=%v", args)
	}
	// "type" verb must precede.
	if args[0] != "type" {
		t.Errorf("expected first arg 'type', got %q", args[0])
	}
}

func TestComputerUseKeyRejectsShellMetacharacters(t *testing.T) {
	r := newFakeRunner()
	withFakeRunner(t, r)

	_, err := ExecuteComputerUse(context.Background(), map[string]any{
		"action": "key",
		"text":   "Return; xterm",
	})
	if err == nil {
		t.Fatal("expected error for key with metacharacters")
	}
	if !strings.Contains(err.Error(), "invalid characters") {
		t.Errorf("expected invalid-characters error, got %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("xdotool must not be called for invalid key, got %+v", r.calls)
	}
}

func TestComputerUseKeyAcceptsValidKeysym(t *testing.T) {
	r := newFakeRunner()
	withFakeRunner(t, r)

	_, err := ExecuteComputerUse(context.Background(), map[string]any{
		"action": "key",
		"text":   "ctrl+c",
	})
	if err != nil {
		t.Fatalf("ExecuteComputerUse(key): %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected one call, got %+v", r.calls)
	}
	want := []string{"key", "--", "ctrl+c"}
	if !strSliceEq(r.calls[0].args, want) {
		t.Errorf("xdotool args = %v, want %v", r.calls[0].args, want)
	}
}

func TestComputerUseUnsupportedSystemErrors(t *testing.T) {
	t.Run("no display", func(t *testing.T) {
		r := newFakeRunner()
		r.display = false
		withFakeRunner(t, r)
		_, err := ExecuteComputerUse(context.Background(), map[string]any{"action": "screenshot"})
		if err == nil {
			t.Fatal("expected error when no display available")
		}
		if !errors.Is(err, ErrComputerUseUnavailable) {
			t.Fatalf("expected ErrComputerUseUnavailable, got %v", err)
		}
	})

	t.Run("missing import binary", func(t *testing.T) {
		r := newFakeRunner()
		r.available["import"] = false
		withFakeRunner(t, r)
		_, err := ExecuteComputerUse(context.Background(), map[string]any{"action": "screenshot"})
		if err == nil {
			t.Fatal("expected error when import is missing")
		}
		if !errors.Is(err, ErrComputerUseUnavailable) {
			t.Fatalf("expected ErrComputerUseUnavailable, got %v", err)
		}
		if !strings.Contains(err.Error(), "import") {
			t.Errorf("expected 'import' in error, got %v", err)
		}
	})

	t.Run("missing xdotool binary", func(t *testing.T) {
		r := newFakeRunner()
		r.available["xdotool"] = false
		withFakeRunner(t, r)
		_, err := ExecuteComputerUse(context.Background(), map[string]any{
			"action":     "left_click",
			"coordinate": []any{10, 20},
		})
		if err == nil {
			t.Fatal("expected error when xdotool is missing")
		}
		if !errors.Is(err, ErrComputerUseUnavailable) {
			t.Fatalf("expected ErrComputerUseUnavailable, got %v", err)
		}
	})
}

func TestComputerUseRequiresAction(t *testing.T) {
	r := newFakeRunner()
	withFakeRunner(t, r)
	_, err := ExecuteComputerUse(context.Background(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "'action' is required") {
		t.Fatalf("expected action-required error, got %v", err)
	}
}

func TestComputerUseUnknownAction(t *testing.T) {
	r := newFakeRunner()
	withFakeRunner(t, r)
	_, err := ExecuteComputerUse(context.Background(), map[string]any{"action": "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("expected unknown-action error, got %v", err)
	}
}

func TestComputerUseCoordinateValidation(t *testing.T) {
	r := newFakeRunner()
	withFakeRunner(t, r)

	cases := []map[string]any{
		{"action": "left_click"},
		{"action": "left_click", "coordinate": []any{1}},
		{"action": "left_click", "coordinate": "not-an-array"},
		{"action": "left_click", "coordinate": []any{"x", "y"}},
	}
	for i, in := range cases {
		_, err := ExecuteComputerUse(context.Background(), in)
		if err == nil {
			t.Errorf("case %d: expected error for %+v", i, in)
		}
	}
}

func TestComputerUseCursorPositionParsesShellOutput(t *testing.T) {
	r := newFakeRunner()
	r.stdoutFor["xdotool"] = []byte("X=512\nY=384\nSCREEN=0\nWINDOW=0x1234\n")
	withFakeRunner(t, r)

	res, err := ExecuteComputerUse(context.Background(), map[string]any{"action": "cursor_position"})
	if err != nil {
		t.Fatalf("ExecuteComputerUse(cursor_position): %v", err)
	}
	if !strings.Contains(res.Description, "(512, 384)") {
		t.Errorf("expected description with (512, 384), got %q", res.Description)
	}
}

func TestComputerUseDragChainsXdotoolCommands(t *testing.T) {
	r := newFakeRunner()
	withFakeRunner(t, r)

	_, err := ExecuteComputerUse(context.Background(), map[string]any{
		"action":           "left_click_drag",
		"start_coordinate": []any{10, 20},
		"coordinate":       []any{100, 200},
	})
	if err != nil {
		t.Fatalf("ExecuteComputerUse(drag): %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected single chained xdotool call, got %d", len(r.calls))
	}
	want := []string{
		"mousemove", "--sync", "10", "20",
		"mousedown", "1",
		"mousemove", "--sync", "100", "200",
		"mouseup", "1",
	}
	if !strSliceEq(r.calls[0].args, want) {
		t.Errorf("drag args = %v, want %v", r.calls[0].args, want)
	}
}

func TestExecuteScreenshotShim(t *testing.T) {
	r := newFakeRunner()
	r.stdoutFor["import"] = pngFixture
	withFakeRunner(t, r)

	res, err := ExecuteScreenshot(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("ExecuteScreenshot: %v", err)
	}
	if len(res.Blocks) != 1 || res.Blocks[0].Source == nil {
		t.Fatalf("expected one image block, got %+v", res)
	}
}

func strSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
