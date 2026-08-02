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
