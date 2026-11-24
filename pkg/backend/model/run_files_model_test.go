package model

import (
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type runFilesCloseGrant struct {
	store.RunFilesGrant
	t      *testing.T
	dir    string
	closed int
	err    error
}

func (g *runFilesCloseGrant) Close() error {
	g.closed++
	if _, e := os.Stat(g.dir); !os.IsNotExist(e) {
		g.t.Error("grant closed before handoff cleanup")
	}
	return g.err
}

type runFilesCloseErr struct{ err error }

func (c runFilesCloseErr) Close() error { return c.err }
func TestRunFilesModelGrantCloseOrderAndErrors(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "routing-handoff")
	if e := os.Mkdir(p, 0700); e != nil {
		t.Fatal(e)
	}
	a := errors.New("extra close")
	b := errors.New("grant close")
	g := &runFilesCloseGrant{t: t, dir: p, err: b}
	e := &ClawExecutor{handoff: &HandoffRecorder{dir: p, created: true}, extraClosers: []io.Closer{runFilesCloseErr{a}}}
	WithRunFilesGrant(g)(e)
	if e.RunFilesGrant() != g {
		t.Fatal("provider lost original grant")
	}
	err := e.Close()
	if !errors.Is(err, a) || !errors.Is(err, b) || g.closed != 1 {
		t.Fatalf("close lost error or ownership: %v calls%d", err, g.closed)
	}
}
