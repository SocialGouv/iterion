package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRetainedRunFilesGrantKeepsOriginalDirectory(t *testing.T) {
	base := t.TempDir()
	p := filepath.Join(base, "files")
	if e := os.Mkdir(p, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(p, "out"), []byte("original"), 0600); e != nil {
		t.Fatal(e)
	}
	root, e := os.OpenRoot(p)
	if e != nil {
		t.Fatal(e)
	}
	h := RunDataHeader{RunID: "r", Format: 2, Protocol: IncarnationRunData, BirthID: "birth"}
	b, e := CaptureRunDataBinding(h)
	if e != nil {
		t.Fatal(e)
	}
	g, e := NewRetainedRunFilesGrant(p, root, b, func(ctx context.Context) error { return CheckRunDataBinding(ctx, h) })
	if e != nil {
		root.Close()
		t.Fatal(e)
	}
	defer g.Close()
	if e = g.ValidateHostPath(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(p, p+"-held"); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(p, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(p, "out"), []byte("foreign"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = g.ValidateHostPath(context.Background()); !errors.Is(e, ErrRunDataIdentityChanged) {
		t.Fatal("replacement accepted", e)
	}
	dup, e := g.OpenRoot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer dup.Close()
	if e = g.Close(); e != nil {
		t.Fatal(e)
	}
	body, e := dup.ReadFile("out")
	if e != nil || string(body) != "original" {
		t.Fatal(string(body), e)
	}
	if _, e = g.OpenRoot(context.Background()); e == nil {
		t.Fatal("closed grant reopened pathname")
	}
}
func TestRetainedRunFilesGrantRejectsConflictingBindings(t *testing.T) {
	root, e := os.OpenRoot(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	h := RunDataHeader{RunID: "r", Format: 2, Protocol: IncarnationRunData, BirthID: "original"}
	b, e := CaptureRunDataBinding(h)
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	current := h
	g, e := NewRetainedRunFilesGrant(root.Name(), root, b, func(ctx context.Context) error { calls++; return CheckRunDataBinding(ctx, current) })
	if e != nil {
		root.Close()
		t.Fatal(e)
	}
	defer g.Close()
	if _, e = g.Bind(context.Background(), "other"); !errors.Is(e, ErrRunDataIdentityChanged) {
		t.Fatal(e)
	}
	other := h
	other.BirthID = "successor"
	ob, e := CaptureRunDataBinding(other)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := WithRunDataBinding(context.Background(), ob)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = g.OpenRoot(ctx); !errors.Is(e, ErrRunDataIdentityChanged) {
		t.Fatal(e)
	}
	ctx, e = WithRunValueFormat(context.Background(), RunValueFormat{RunID: "r", Format: 1})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = g.OpenRoot(ctx); !errors.Is(e, ErrRunDataIdentityChanged) {
		t.Fatal(e)
	}
	if calls != 0 {
		t.Fatal("conflict reached metadata", calls)
	}
	current = other
	if _, e = g.OpenRoot(context.Background()); !errors.Is(e, ErrRunDataIdentityChanged) {
		t.Fatal("successor adopted", e)
	}
}
