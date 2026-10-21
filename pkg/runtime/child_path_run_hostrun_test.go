package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
)

// The child devbox decorator only puts a directory first on PATH: whether its
// commands run on the engine's own host is still the inner run's answer.
func TestChildPathRunKeepsTheInnerRunsHost(t *testing.T) {
	d, err := noop.New()
	if err != nil {
		t.Fatal(err)
	}
	p, err := d.Prepare(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := d.Start(context.Background(), p, sandbox.RunInfo{})
	if err != nil {
		t.Fatal(err)
	}
	var run sandbox.Run = &childPathRun{Run: inner, bin: "/opt/bin"}
	hr, ok := run.(sandbox.HostRun)
	if !ok || !hr.HostExecutesCommands() {
		t.Fatal("a child devbox over a host run hides that its commands run on the host")
	}
	var container sandbox.Run = &childPathRun{Run: containerRun{}, bin: "/opt/bin"}
	if hr, ok := container.(sandbox.HostRun); ok && hr.HostExecutesCommands() {
		t.Fatal("a child devbox over a container run claims its commands run on the host")
	}
}

// containerRun is a sandbox.Run that is not a HostRun.
type containerRun struct{ sandbox.Run }
