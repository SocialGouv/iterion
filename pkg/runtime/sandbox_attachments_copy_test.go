package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2086. A driver with no host filesystem but a copy (kubernetes) receives
// the run's attachments as a seed at the container path a bind would have
// served; the run's stream says so, and the refusal is left to the driver
// that has neither.

type seedPrepared struct{}

func (seedPrepared) DriverName() string { return "docker" }

var errSeedStop = errors.New("seed: not starting")

// seedCapturingDriver reaches Start, records what it was handed, and stops.
type seedCapturingDriver struct {
	copy    bool
	started bool
	info    sandbox.RunInfo
}

func (d *seedCapturingDriver) Name() string { return "docker" }

func (d *seedCapturingDriver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		SupportsImage:          true,
		SupportsMounts:         true,
		SupportsHostBindMounts: false,
		SupportsHostDirCopy:    d.copy,
		SupportsPostCreate:     true,
		SupportsRemoteUser:     true,
	}
}

func (d *seedCapturingDriver) Prepare(context.Context, sandbox.Spec) (sandbox.PreparedSpec, error) {
	return seedPrepared{}, nil
}

func (d *seedCapturingDriver) Start(_ context.Context, _ sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	d.started = true
	d.info = info
	return nil, errSeedStop
}

func startSandboxOn(t *testing.T, d sandbox.Driver, p SandboxParams) ([]capturedEvent, error) {
	t.Helper()
	t.Setenv("ITERION_MODE", "local")
	p.Drivers = map[string]sandbox.DriverConstructor{
		"docker": func() (sandbox.Driver, error) { return d, nil },
	}
	p.Logger = iterlog.Nop()
	var events []capturedEvent
	p.EmitEvent = func(typ store.EventType, data map[string]any) error {
		events = append(events, capturedEvent{typ: typ, data: data})
		return nil
	}
	_, err := resolveAndStartSandbox(context.Background(), p)
	return events, err
}

func TestAttachmentsAreCopiedWhereNoBindExists(t *testing.T) {
	t.Run("a driver that copies receives the attachments as a seed", func(t *testing.T) {
		p := canaryParams(t)
		attachTo(t, p.AttachmentsHostDir, "spec", "spec.md")
		d := &seedCapturingDriver{copy: true}
		events, err := startSandboxOn(t, d, p)
		if !errors.Is(err, errSeedStop) {
			t.Fatalf("err = %v, want the driver's stop at Start — the run was refused or never reached Start", err)
		}
		if !d.started {
			t.Fatal("Start never ran")
		}
		want := []sandbox.SeedDir{{HostDir: p.AttachmentsHostDir, ContainerPath: attachmentsContainerPath, ReadOnly: true}}
		if !reflect.DeepEqual(d.info.SeedDirs, want) {
			t.Fatalf("Start was handed seeds %+v, want %+v", d.info.SeedDirs, want)
		}
		copied := capturedEventsOf(events, store.EventSandboxAttachmentsCopied)
		if len(copied) != 1 {
			t.Fatalf("sandbox_attachments_copied emitted %d times, want 1", len(copied))
		}
		if copied[0]["target"] != attachmentsContainerPath || !reflect.DeepEqual(copied[0]["attachments"], []string{"spec"}) {
			t.Fatalf("the copied event does not name the seed: %v", copied[0])
		}
		if n := len(capturedEventsOf(events, store.EventSandboxSkipped)); n != 0 {
			t.Fatalf("a run the driver can serve was refused: %d sandbox_skipped", n)
		}
	})
	t.Run("a driver that neither binds nor copies still refuses, before Start", func(t *testing.T) {
		p := canaryParams(t)
		attachTo(t, p.AttachmentsHostDir, "spec", "spec.md")
		d := &seedCapturingDriver{copy: false}
		_, err := startSandboxOn(t, d, p)
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Code != ErrCodeAttachmentsUndeliverable {
			t.Fatalf("err = %v, want the typed refusal", err)
		}
		if d.started {
			t.Fatal("Start ran on a refused run")
		}
	})
	t.Run("no attachments: nothing to seed, Start runs with none", func(t *testing.T) {
		p := canaryParams(t)
		d := &seedCapturingDriver{copy: true}
		_, err := startSandboxOn(t, d, p)
		if !errors.Is(err, errSeedStop) || len(d.info.SeedDirs) != 0 {
			t.Fatalf("err = %v, seeds = %+v; want Start reached with no seed", err, d.info.SeedDirs)
		}
	})
}
