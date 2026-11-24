package store

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestIncarnationDirectoryMissingLeafKeepsRetryParent(t *testing.T) {
	h := RunDataHeader{RunID: "run", Format: ExactValueFormat, Protocol: IncarnationRunData, BirthID: "birth"}
	b, err := CaptureRunDataBinding(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := WithRunDataBinding(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := ResolveIncarnationDataScope(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	d, err := OpenIncarnationDirectoryAt(ctx, base, scope, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Parent.Remove(d.Name); err != nil {
		d.Close()
		t.Fatal(err)
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	retry, err := OpenIncarnationDirectoryAt(ctx, base, scope, false)
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Close()
	if retry.Root != nil {
		t.Fatal("removed leaf was recreated")
	}
	if err = syncRunPayloadRoot(retry.Parent); err != nil {
		t.Fatal(err)
	}
	// Missing leaf permits cleanup retry; a replacement symlink must still refuse.
	if err = retry.Parent.Symlink("missing-target", retry.Name); err != nil {
		t.Fatal(err)
	}
	bad, err := OpenIncarnationDirectoryAt(ctx, base, scope, false)
	if bad != nil {
		bad.Close()
	}
	if !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatal("symlink treated as absent", err)
	}
}
