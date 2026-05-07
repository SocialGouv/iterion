package model

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestScanPreviewURLs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []map[string]interface{}
	}{
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
		{
			name: "no directive",
			in:   "starting server on :8000\nlistening...\n",
			want: nil,
		},
		{
			name: "plain url",
			in:   "[iterion] preview_url=http://localhost:8000\n",
			want: []map[string]interface{}{
				{"url": "http://localhost:8000", "source": "tool-stdout", "scope": "external"},
			},
		},
		{
			name: "with kind and scope",
			in:   "starting...\n[iterion] preview_url=http://localhost:3000 kind=dev-server scope=internal\ndone\n",
			want: []map[string]interface{}{
				{"url": "http://localhost:3000", "source": "tool-stdout", "scope": "internal", "kind": "dev-server"},
			},
		},
		{
			name: "two directives",
			in:   "[iterion] preview_url=http://a/\nfoo\n[iterion] preview_url=http://b/ kind=deploy\n",
			want: []map[string]interface{}{
				{"url": "http://a/", "source": "tool-stdout", "scope": "external"},
				{"url": "http://b/", "source": "tool-stdout", "scope": "external", "kind": "deploy"},
			},
		},
		{
			name: "ignores wrong prefix",
			in:   "iterion preview_url=http://x/\npreview_url=http://y/\n",
			want: nil,
		},
		{
			name: "ignores empty url",
			in:   "[iterion] preview_url=\n",
			want: nil,
		},
		{
			name: "ignores unknown kv pairs",
			in:   "[iterion] preview_url=http://z/ foo=bar kind=dev-server\n",
			want: []map[string]interface{}{
				{"url": "http://z/", "source": "tool-stdout", "scope": "external", "kind": "dev-server"},
			},
		},
		{
			name: "trims whitespace",
			in:   "  [iterion] preview_url=http://localhost:1/  \n",
			want: []map[string]interface{}{
				{"url": "http://localhost:1/", "source": "tool-stdout", "scope": "external"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanPreviewURLs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("scanPreviewURLs() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScanPreviewScreenshots(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []ScreenshotDirective
	}{
		{name: "empty", in: "", want: nil},
		{name: "no directive", in: "doing stuff\n", want: nil},
		{
			name: "plain path",
			in:   "[iterion] preview_screenshot=/tmp/shot.png\n",
			want: []ScreenshotDirective{{Path: "/tmp/shot.png"}},
		},
		{
			name: "with url + tool_call_id",
			in:   "[iterion] preview_screenshot=/tmp/a.png url=http://x/ tool_call_id=t1\n",
			want: []ScreenshotDirective{{Path: "/tmp/a.png", URL: "http://x/", ToolCallID: "t1"}},
		},
		{
			name: "two directives",
			in:   "[iterion] preview_screenshot=/tmp/a.png\nfoo\n[iterion] preview_screenshot=/tmp/b.png url=http://b/\n",
			want: []ScreenshotDirective{
				{Path: "/tmp/a.png"},
				{Path: "/tmp/b.png", URL: "http://b/"},
			},
		},
		{
			name: "ignores empty path",
			in:   "[iterion] preview_screenshot=\n",
			want: nil,
		},
		{
			name: "ignores wrong prefix",
			in:   "iterion preview_screenshot=/tmp/a.png\n",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanPreviewScreenshots(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("scanPreviewScreenshots() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSanitizeAttachmentSegment(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"node1", "node1"},
		{"my_node-id", "my_node-id"},
		{"weird/name", "weird-name"},
		{"a/b\\c:d", "a-b-c-d"},
		{"-leading-dash", "leading-dash"},
		{strings.Repeat("x", 80), strings.Repeat("x", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := sanitizeAttachmentSegment(tc.in)
			if got != tc.want {
				t.Fatalf("sanitizeAttachmentSegment(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDetectScreenshotMIME(t *testing.T) {
	cases := map[string]string{
		"shot.png":        "image/png",
		"a.PNG":           "image/png",
		"thumb.jpg":       "image/jpeg",
		"thumb.JPEG":      "image/jpeg",
		"frame.webp":      "image/webp",
		"icon.gif":        "image/gif",
		"unknown":         "image/png",
		"weird.tar.gz":    "image/png",
		"trailing-slash/": "image/png",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := detectScreenshotMIME(in); got != want {
				t.Fatalf("detectScreenshotMIME(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

// fakeStore implements EventEmitter and AttachmentWriter for the
// captureBrowserScreenshot test. AppendEvent records every event;
// WriteAttachment captures the bytes the runtime tried to persist.
type fakeStore struct {
	mu          sync.Mutex
	events      []store.Event
	attachments map[string][]byte
	attachMeta  map[string]store.AttachmentRecord
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		attachments: make(map[string][]byte),
		attachMeta:  make(map[string]store.AttachmentRecord),
	}
}

func (f *fakeStore) AppendEvent(_ context.Context, _ string, evt store.Event) (*store.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, evt)
	return &evt, nil
}

func (f *fakeStore) WriteAttachment(_ context.Context, _ string, rec store.AttachmentRecord, body io.Reader) error {
	buf, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachments[rec.Name] = buf
	f.attachMeta[rec.Name] = rec
	return nil
}

func TestCaptureBrowserScreenshot_ReadsFileAndEmitsEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	pngBytes := []byte("\x89PNG\r\n\x1a\nfake-payload")
	if err := os.WriteFile(path, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	fs := newFakeStore()
	logger := iterlog.New(iterlog.LevelError, io.Discard)

	captureBrowserScreenshot(
		context.Background(), fs, fs, "run-1", "my-node",
		ScreenshotDirective{Path: path, URL: "http://x/", ToolCallID: "tc-9"},
		logger,
	)

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(fs.attachments))
	}
	if len(fs.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fs.events))
	}
	evt := fs.events[0]
	if evt.Type != store.EventBrowserScreenshot {
		t.Fatalf("event type = %v, want %v", evt.Type, store.EventBrowserScreenshot)
	}
	if evt.NodeID != "my-node" || evt.RunID != "run-1" {
		t.Fatalf("event run_id/node_id = %s/%s", evt.RunID, evt.NodeID)
	}
	name, _ := evt.Data["attachment_name"].(string)
	if name == "" {
		t.Fatal("event missing attachment_name")
	}
	body, ok := fs.attachments[name]
	if !ok {
		t.Fatalf("attachment %q not stored", name)
	}
	if !bytes.Equal(body, pngBytes) {
		t.Fatal("attachment bytes do not match input file")
	}
	if got, _ := evt.Data["url"].(string); got != "http://x/" {
		t.Fatalf("event url = %v", got)
	}
	if got, _ := evt.Data["tool_call_id"].(string); got != "tc-9" {
		t.Fatalf("event tool_call_id = %v", got)
	}
	if got, _ := evt.Data["mime"].(string); got != "image/png" {
		t.Fatalf("event mime = %v", got)
	}
}

func TestCaptureBrowserScreenshot_MissingFileIsNonFatal(t *testing.T) {
	fs := newFakeStore()
	logger := iterlog.New(iterlog.LevelError, io.Discard)

	captureBrowserScreenshot(
		context.Background(), fs, fs, "run-1", "n",
		ScreenshotDirective{Path: "/nope/does-not-exist.png"},
		logger,
	)

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.attachments) != 0 {
		t.Fatalf("expected no attachment, got %d", len(fs.attachments))
	}
	if len(fs.events) != 0 {
		t.Fatalf("expected no event, got %d", len(fs.events))
	}
}
