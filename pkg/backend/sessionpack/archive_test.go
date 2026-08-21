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
	files, err := CollectBySessionID(root, "sess-1", "claude_code")
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
	files, err := CollectBySessionID(root, "sess-1", "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("hardlinked session file was packed: %+v", files)
	}
}

func TestUnpackMergesWithoutClobberingSiblings(t *testing.T) {
	h := Header{Backend: "claude_code", SessionID: "sess-1"}
	blob, err := Pack(h, []File{{Name: "projects/x/sess-1.jsonl", Body: []byte("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	other := filepath.Join(dir, "projects", "other", "sess-999.jsonl")
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("keep-me"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(dir, "projects", "x", "sess-2.jsonl")
	if err := os.MkdirAll(filepath.Dir(sibling), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("also-keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(blob, h, dir); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "projects", "x", "sess-1.jsonl")); err != nil || string(got) != "hello" {
		t.Fatalf("unpacked sess-1: %q %v", got, err)
	}
	if got, err := os.ReadFile(other); err != nil || string(got) != "keep-me" {
		t.Fatalf("sibling project clobbered: %q %v", got, err)
	}
	if got, err := os.ReadFile(sibling); err != nil || string(got) != "also-keep" {
		t.Fatalf("sibling session clobbered: %q %v", got, err)
	}
}

func TestHasFileDoesNotRequireRead(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "sess-1.jsonl")
	if err := os.WriteFile(p, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !HasFile(root, "sess-1", "claude_code") {
		t.Fatal("expected hit")
	}
	if HasFile(root, "sess-missing", "claude_code") {
		t.Fatal("missing session reported present")
	}
}

func TestCollectCodexRolloutName(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "sessions", "2026", "08", "21", "rollout-2026-08-21T10-00-00-threadABC.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := CollectBySessionID(root, "threadABC", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "sessions/2026/08/21/rollout-2026-08-21T10-00-00-threadABC.jsonl" {
		t.Fatalf("files=%+v", files)
	}
	if !HasFile(root, "threadABC", "codex") {
		t.Fatal("HasFile missed codex rollout")
	}
	if HasFile(root, "threadABC", "claude_code") {
		t.Fatal("claude matcher must not accept rollout-* names")
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
