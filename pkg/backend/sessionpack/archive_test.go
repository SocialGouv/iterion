package sessionpack

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPackUnpackRoundTrip(t *testing.T) {
	h := Header{Backend: "claude_code", SessionID: "sess-1"}
	blob, err := Pack(h, []File{{Name: "projects/x/sess-1.jsonl", Body: []byte("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := Unpack(blob, h, dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "projects", "x", "sess-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestPackRejectsDotDot(t *testing.T) {
	h := Header{Backend: "claude_code", SessionID: "s"}
	if _, err := Pack(h, []File{{Name: "../etc/passwd", Body: []byte("x")}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestCollectSkipsAuthFiles(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "auth.json"), []byte("SECRET"), 0o600)
	_ = os.WriteFile(filepath.Join(root, ".credentials.json"), []byte("SECRET"), 0o600)
	_ = os.WriteFile(filepath.Join(root, "sess-1.jsonl"), []byte("ok"), 0o600)
	files, err := CollectBySessionID(root, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "sess-1.jsonl" {
		t.Fatalf("files=%+v", files)
	}
	for _, f := range files {
		if string(f.Body) == "SECRET" {
			t.Fatal("packed an auth file")
		}
	}
}

func TestCollectSkipsHardlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "sess-1.jsonl")
	if err := os.WriteFile(target, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "other.jsonl")
	if err := os.Link(target, link); err != nil {
		t.Skip("hardlinks not supported")
	}
	files, err := CollectBySessionID(root, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("hardlinked session file was packed: %+v", files)
	}
}

func TestUnpackHeaderMismatch(t *testing.T) {
	h := Header{Backend: "claude_code", SessionID: "a"}
	blob, err := Pack(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Unpack(blob, Header{Backend: "codex", SessionID: "a"}, t.TempDir()); err == nil {
		t.Fatal("expected header mismatch")
	}
}
