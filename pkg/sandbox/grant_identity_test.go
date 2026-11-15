package sandbox

import (
	"context"
	"errors"
	"os"
	"testing"
)

type grantCheck struct {
	path  string
	calls int
	err   error
	ctx   context.Context
}

func (g *grantCheck) HostPath() string                         { return g.path }
func (g *grantCheck) ValidateHostPath(c context.Context) error { g.calls++; g.ctx = c; return g.err }
func TestRunFilesGrantIdentityBeforeMutation(t *testing.T) {
	p := t.TempDir()
	if err := os.Chmod(p, 0755); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("identity changed")
	g := &grantCheck{path: p, err: denied}
	c := BindSourceCheck{Root: p, Path: p, Identity: g}
	if err := AnswerBindSourceCheckContext(context.Background(), c); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0755 {
		t.Fatal("foreign directory chmod", st.Mode())
	}
	c.Path = p + "/other"
	if err := AnswerBindSourceCheckContext(context.Background(), c); err == nil {
		t.Fatal("mismatched path accepted")
	}
	if g.calls != 1 {
		t.Fatal(g.calls)
	}
	if err := AnswerBindSourceCheck(c); err == nil {
		t.Fatal("identity check without context accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Path = p
	if err := AnswerBindSourceCheckContext(ctx, c); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if g.calls != 1 {
		t.Fatal("called on cancellation")
	}
}
