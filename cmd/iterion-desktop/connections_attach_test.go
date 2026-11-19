//go:build desktop

package main

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// With daemons opted out, a project other than the current one cannot open
// in a pane: the refusal says why instead of spawning the daemon the
// operator turned off.
func TestOpenLocalConn_AttachOffRefusesAnotherProject(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	t.Setenv("ITERION_DESKTOP_ATTACH_DAEMON", "0")
	app := &App{embeddedProjectID: "current", embeddedURL: "http://127.0.0.1:6666/"}
	err := app.openLocalConn(&Project{ID: "other", Name: "other", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "ITERION_DESKTOP_ATTACH_DAEMON") {
		t.Fatalf("openLocalConn with daemons off = %v, want a refusal naming ITERION_DESKTOP_ATTACH_DAEMON", err)
	}
	if app.lookupConn("other") != nil {
		t.Fatal("a refused project was registered as a connection")
	}
}

// The project the embedded server serves opens on it — even with nothing in
// the registry (after the first-run wizard or a switch restarted the server)
// or with a stale entry on the previous port — and on the embedded server's
// own URL, not serverURL (which a cloud connection may have taken over).
func TestOpenLocalConn_AttachOffOpensTheEmbeddedProject(t *testing.T) {
	t.Setenv("ITERION_DESKTOP_ATTACH_DAEMON", "0")
	const embedded = "http://127.0.0.1:6666/"
	for name, app := range map[string]*App{
		"empty registry": {},
		"stale entry":    withConn(&App{}, &activeConn{id: "current", kind: ProjectKindLocal, serverURL: "http://127.0.0.1:5555/"}),
	} {
		app.embeddedProjectID, app.embeddedURL = "current", embedded
		app.serverURL = "https://cloud.example/"
		if err := app.openLocalConn(&Project{ID: "current", Name: "current"}); err != nil {
			t.Fatalf("%s: openLocalConn(embedded project) with daemons off = %v, want nil", name, err)
		}
		if c := app.lookupConn("current"); c == nil || c.serverURL != embedded {
			t.Fatalf("%s: registered %+v, want the embedded server %s", name, c, embedded)
		}
	}
}

type fakeEmbeddedServer struct {
	addr string
	err  error
}

func (f *fakeEmbeddedServer) Start(context.Context, string, string) (string, error) {
	return f.addr, f.err
}

// A restart that stops the embedded server and fails to start it again
// leaves nothing to open a pane on: the previous project is refused, not
// registered on the dead port.
func TestOpenLocalConn_AttachOffRefusesAfterAFailedRestart(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	t.Setenv("ITERION_DESKTOP_ATTACH_DAEMON", "0")
	orig := windowReloader
	windowReloader = nil
	t.Cleanup(func() { windowReloader = orig })
	cfg := NewConfig()
	a := cfg.AddProject(t.TempDir())
	srv := &fakeEmbeddedServer{addr: "127.0.0.1:7777"}
	app := &App{config: cfg, server: srv}
	if _, err := app.restartServerForCurrentProject(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.AddProject(t.TempDir())
	srv.err = errors.New("bind failed")
	if _, err := app.restartServerForCurrentProject(context.Background()); err == nil {
		t.Fatal("restart with a failing Start = nil error")
	}
	if err := app.openLocalConn(&a); err == nil {
		t.Fatalf("openLocalConn(%s) after a failed restart = nil, want a refusal (registered %+v)", a.ID, app.lookupConn(a.ID))
	}
}
func (f *fakeEmbeddedServer) Stop() {}

// A project switch restarts the embedded server on a new port without
// touching the registry: with daemons off, the switched-to project still
// opens, on the new port.
func TestOpenLocalConn_AttachOffOpensTheProjectASwitchRestartedOn(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	t.Setenv("ITERION_DESKTOP_ATTACH_DAEMON", "0")
	orig := windowReloader
	windowReloader = nil // no webview here
	t.Cleanup(func() { windowReloader = orig })
	cfg := NewConfig()
	p := cfg.AddProject(t.TempDir())
	app := &App{config: cfg, server: &fakeEmbeddedServer{addr: "127.0.0.1:7777"}}
	if _, err := app.restartServerForCurrentProject(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.openLocalConn(&p); err != nil {
		t.Fatalf("openLocalConn(switched-to project) with daemons off = %v, want nil", err)
	}
	if c := app.lookupConn(p.ID); c == nil || c.serverURL != "http://127.0.0.1:7777/" {
		t.Fatalf("registered %+v, want the restarted embedded server", c)
	}
}

// With daemons off, a run served from another project's store deep-links to
// the embedded server, never to that project's daemon.
func TestGetDaemonURLForStore_AttachOffStaysOnTheEmbeddedServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	project := filepath.Join(home, "other")
	writeDaemonInfo(project, "http://"+ln.Addr().String()+"/")
	t.Setenv("ITERION_DESKTOP_ATTACH_DAEMON", "0")

	app := &App{serverURL: "http://127.0.0.1:2/"}
	store := filepath.Join(home, "projects", encodeProjectDirKey(project))
	got, err := app.GetDaemonURLForStore(store)
	if err != nil || got != app.serverURL {
		t.Fatalf("GetDaemonURLForStore with daemons off = %q, %v; want the embedded %q", got, err, app.serverURL)
	}
}
