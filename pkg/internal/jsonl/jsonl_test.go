package jsonl

import (
	"os"
	"path/filepath"
	"testing"
)

type rec struct {
	N int    `json:"n"`
	S string `json:"s"`
}

func TestAppendAndRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "audit.jsonl")
	for i := 1; i <= 3; i++ {
		if err := AppendJSON(p, rec{N: i, S: "x"}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got, err := ReadLines[rec](p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 3 || got[0].N != 1 || got[2].N != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestAppendRepairsTornTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := AppendJSON(p, rec{N: 1}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Simulate a crash mid-write: partial JSON with no trailing newline.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"n":2,"s":"tor`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := AppendJSON(p, rec{N: 3}); err != nil {
		t.Fatalf("append after torn tail: %v", err)
	}
	got, err := ReadLines[rec](p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Torn line skipped; records 1 and 3 intact.
	if len(got) != 2 || got[0].N != 1 || got[1].N != 3 {
		t.Fatalf("got %+v, want records 1 and 3", got)
	}
}

func TestReadLinesMissingFile(t *testing.T) {
	got, err := ReadLines[rec](filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}
