package model

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestScanToolAttachmentsParsesNameAndMIME(t *testing.T) {
	out := `building the mix…
[iterion] attachment=/tmp/mix.mp3 name=audio_fr mime=audio/mpeg
some other log line
[iterion] attachment=/tmp/final.mp4
[iterion] attachment=
done`
	got := scanToolAttachments(out)
	if len(got) != 2 {
		t.Fatalf("scanToolAttachments = %d directives, want 2: %#v", len(got), got)
	}
	if got[0] != (AttachmentDirective{Path: "/tmp/mix.mp3", Name: "audio_fr", MIME: "audio/mpeg"}) {
		t.Errorf("first directive = %#v", got[0])
	}
	// A bare path is legal: name and MIME are derived later.
	if got[1] != (AttachmentDirective{Path: "/tmp/final.mp4"}) {
		t.Errorf("second directive = %#v", got[1])
	}
	if scanToolAttachments("nothing to see") != nil {
		t.Error("plain output must not yield directives")
	}
}

func TestAttachmentMIMEPrefersTheDeclaredValueThenTheExtension(t *testing.T) {
	cases := []struct {
		name string
		dir  AttachmentDirective
		want string
	}{
		{"declared wins", AttachmentDirective{Path: "/tmp/a.bin", MIME: "audio/mpeg"}, "audio/mpeg"},
		{"declared with params", AttachmentDirective{Path: "/tmp/a.bin", MIME: "text/plain; charset=utf-8"}, "text/plain"},
		{"sniffed from extension", AttachmentDirective{Path: "/tmp/clip.mp4"}, "video/mp4"},
		{"garbage declaration falls back", AttachmentDirective{Path: "/tmp/clip.mp4", MIME: "not a mime"}, "video/mp4"},
		{"unknown stays honest", AttachmentDirective{Path: "/tmp/thing.zzz"}, "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := attachmentMIME(tc.dir); got != tc.want {
				t.Errorf("attachmentMIME = %q, want %q", got, tc.want)
			}
		})
	}
}

// recordingSink captures what the hook asked the store to persist.
type recordingSink struct {
	rec  store.AttachmentRecord
	body []byte
	err  error
}

func (s *recordingSink) WriteAttachment(
	_ context.Context, _ string, rec store.AttachmentRecord, body io.Reader,
) error {
	if s.err != nil {
		return s.err
	}
	s.rec = rec
	s.body, _ = io.ReadAll(body)
	return nil
}

type nopEmitter struct{}

func (nopEmitter) AppendEvent(context.Context, string, store.Event) (*store.Event, error) {
	return &store.Event{}, nil
}

func TestPublishToolAttachmentStoresTheBytesUnderAUsableName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mix preview.mp3")
	if err := os.WriteFile(path, []byte("ID3 fake audio"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sink := &recordingSink{}
	logger := iterlog.New(iterlog.LevelError, os.Stderr)

	publishToolAttachment(
		context.Background(), sink, nopEmitter{}, "run-1", "publish_audio",
		AttachmentDirective{Path: path, Name: "audio_fr", MIME: "audio/mpeg"}, logger,
	)

	if sink.rec.Name != "audio_fr" {
		t.Errorf("attachment name = %q, want audio_fr", sink.rec.Name)
	}
	if sink.rec.MIME != "audio/mpeg" {
		t.Errorf("MIME = %q", sink.rec.MIME)
	}
	if sink.rec.OriginalFilename != "mix preview.mp3" {
		t.Errorf("original filename = %q", sink.rec.OriginalFilename)
	}
	if string(sink.body) != "ID3 fake audio" {
		t.Errorf("stored bytes = %q", sink.body)
	}
}

func TestPublishToolAttachmentDerivesTheNameAndSurvivesFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "final cut.mp4")
	if err := os.WriteFile(path, []byte("mp4"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	logger := iterlog.New(iterlog.LevelError, os.Stderr)

	// No name given: the file's stem becomes the handle, sanitised — the
	// gate's descriptor carries this exact string.
	sink := &recordingSink{}
	publishToolAttachment(
		context.Background(), sink, nopEmitter{}, "run-1", "publish",
		AttachmentDirective{Path: path}, logger,
	)
	if sink.rec.Name != "final-cut" {
		t.Errorf("derived name = %q, want final-cut", sink.rec.Name)
	}
	if sink.rec.MIME != "video/mp4" {
		t.Errorf("derived MIME = %q, want video/mp4", sink.rec.MIME)
	}

	// A missing file must never abort the tool node.
	publishToolAttachment(
		context.Background(), &recordingSink{}, nopEmitter{}, "run-1", "publish",
		AttachmentDirective{Path: filepath.Join(dir, "absent.mp4")}, logger,
	)
}

func TestSplitAttachmentTailKeepsSpacesInThePath(t *testing.T) {
	// Exported media routinely carries spaces. Before this, the shared
	// whitespace scanner truncated the path and the file was skipped with
	// a warning — the exact empty gate this verb removes.
	got := scanToolAttachments(
		"[iterion] attachment=/exports/final cut.mp4 name=final_video mime=video/mp4",
	)
	if len(got) != 1 {
		t.Fatalf("scanToolAttachments = %#v, want 1 directive", got)
	}
	want := AttachmentDirective{
		Path: "/exports/final cut.mp4", Name: "final_video", MIME: "video/mp4",
	}
	if got[0] != want {
		t.Errorf("directive = %#v, want %#v", got[0], want)
	}

	// …and with no options at all, the whole tail is the path.
	bare := scanToolAttachments("[iterion] attachment=/exports/final cut.mp4")
	if len(bare) != 1 || bare[0].Path != "/exports/final cut.mp4" {
		t.Errorf("bare spaced path = %#v", bare)
	}
}

func TestActiveMIMEIsNeutralisedBeforeStorage(t *testing.T) {
	// The serve route replies `Content-Disposition: inline` with no
	// nosniff and no CSP, so an executable type would run in the
	// studio's own origin. Tool stdout is not a trusted channel.
	for _, declared := range []string{
		"text/html", "image/svg+xml", "application/xhtml+xml",
		"text/javascript", "application/javascript", "TEXT/HTML",
	} {
		got := attachmentMIME(AttachmentDirective{Path: "/tmp/x.bin", MIME: declared})
		if got != "application/octet-stream" {
			t.Errorf("attachmentMIME(%q) = %q, want application/octet-stream", declared, got)
		}
	}
	// A .html path must not smuggle it back in through the extension.
	if got := attachmentMIME(AttachmentDirective{Path: "/tmp/report.html"}); got != "application/octet-stream" {
		t.Errorf("sniffed html = %q, want application/octet-stream", got)
	}
	// Media the gate is meant to preview is untouched.
	for path, want := range map[string]string{
		"/tmp/a.mp4": "video/mp4", "/tmp/a.mp3": "audio/mpeg", "/tmp/a.png": "image/png",
	} {
		if got := attachmentMIME(AttachmentDirective{Path: path}); got != want {
			t.Errorf("attachmentMIME(%q) = %q, want %q", path, got, want)
		}
	}
}

// listingSink adds the optional read half so the collision guard engages.
type listingSink struct {
	recordingSink
	existing []store.AttachmentRecord
}

func (s *listingSink) ListAttachments(
	context.Context, string,
) ([]store.AttachmentRecord, error) {
	return s.existing, nil
}

func TestPublishToolAttachmentRefusesToClobberAnExistingName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "final.mp4")
	if err := os.WriteFile(path, []byte("new bytes"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	logger := iterlog.New(iterlog.LevelError, os.Stderr)

	// An operator's own upload already holds the name: it must survive.
	taken := &listingSink{existing: []store.AttachmentRecord{{Name: "final"}}}
	publishToolAttachment(
		context.Background(), taken, nopEmitter{}, "run-1", "publish",
		AttachmentDirective{Path: path}, logger,
	)
	if taken.rec.Name != "" || taken.body != nil {
		t.Errorf("existing attachment was clobbered: %#v", taken.rec)
	}

	// A free name goes through.
	free := &listingSink{existing: []store.AttachmentRecord{{Name: "something_else"}}}
	publishToolAttachment(
		context.Background(), free, nopEmitter{}, "run-1", "publish",
		AttachmentDirective{Path: path}, logger,
	)
	if free.rec.Name != "final" {
		t.Errorf("free name did not publish: %#v", free.rec)
	}
}

func TestPublishToolAttachmentSkipsWhatIsTooLargeOrNotAFile(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.New(iterlog.LevelError, os.Stderr)

	// A directory is not a deliverable.
	sink := &recordingSink{}
	publishToolAttachment(
		context.Background(), sink, nopEmitter{}, "run-1", "publish",
		AttachmentDirective{Path: dir}, logger,
	)
	if sink.rec.Name != "" {
		t.Errorf("a directory was published: %#v", sink.rec)
	}

	// Over the cap: skipped rather than copied into the store. The file
	// is sparse so the test costs no real bytes.
	big := filepath.Join(dir, "huge.mp4")
	f, err := os.Create(big)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(maxToolAttachmentBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	f.Close()
	over := &recordingSink{}
	publishToolAttachment(
		context.Background(), over, nopEmitter{}, "run-1", "publish",
		AttachmentDirective{Path: big}, logger,
	)
	if over.rec.Name != "" {
		t.Errorf("an oversized file was published: %#v", over.rec)
	}
}
