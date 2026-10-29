package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2086. A driver with no host filesystem drops the host binds the runtime
// declared. Two of the three things that go are promises a withdrawal can
// honour (a variable, a bundle); the attachments are not: a file the
// operator attached that no node can open is a run that goes on without
// what it was given. So the drop is a RUN EVENT, never only a runner
// warning, and a run that holds attachments is refused typed before any
// node — on the one driver shape that can neither bind nor copy them.

type capturedEvent struct {
	typ  store.EventType
	data map[string]any
}

// startSandboxFor drives the production wiring against the canary driver
// (Prepare refuses with errCanaryStop) and returns what it emitted and how
// it ended: errCanaryStop means the wiring accepted the run and reached
// Prepare; anything else is the wiring's own verdict.
func startSandboxFor(t *testing.T, hostBinds bool, p SandboxParams) ([]capturedEvent, error) {
	t.Helper()
	t.Setenv("ITERION_MODE", "local")
	d := &specCapturingDriver{hostBinds: hostBinds}
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

func capturedEventsOf(events []capturedEvent, typ store.EventType) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if ev.typ == typ {
			out = append(out, ev.data)
		}
	}
	return out
}

func attachTo(t *testing.T, dir, name, file string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, file), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxRefusesARunWhoseAttachmentsNoDriverCanDeliver(t *testing.T) {
	t.Run("no host filesystem, attachments held: refused typed, said on the run's stream", func(t *testing.T) {
		p := canaryParams(t)
		attachTo(t, p.AttachmentsHostDir, "spec", "spec.md")
		attachTo(t, p.AttachmentsHostDir, "logo", "logo.png")
		events, err := startSandboxFor(t, false, p)
		if errors.Is(err, errCanaryStop) {
			t.Fatal("the wiring reached Prepare: a run holding attachments was accepted by a driver that dropped their bind")
		}
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Code != ErrCodeAttachmentsUndeliverable {
			t.Fatalf("err = %v, want a RuntimeError with code %s", err, ErrCodeAttachmentsUndeliverable)
		}
		for _, name := range []string{"logo", "spec", "docker"} {
			if !strings.Contains(rt.Message, name) {
				t.Fatalf("the refusal does not name %q: %s", name, rt.Message)
			}
		}
		if rt.Hint == "" {
			t.Fatal("the refusal carries no remedy")
		}
		skipped := capturedEventsOf(events, store.EventSandboxSkipped)
		if len(skipped) != 1 {
			t.Fatalf("sandbox_skipped emitted %d times, want 1: %v", len(skipped), skipped)
		}
		if skipped[0]["refused"] != true || skipped[0]["error_code"] != string(ErrCodeAttachmentsUndeliverable) {
			t.Fatalf("the refusal event does not carry the code: %v", skipped[0])
		}
		if want := []string{"logo", "spec"}; !reflect.DeepEqual(skipped[0]["attachments"], want) {
			t.Fatalf("the refusal event names %v, want %v", skipped[0]["attachments"], want)
		}
		dropped := capturedEventsOf(events, store.EventSandboxHostBindsDropped)
		if len(dropped) != 1 {
			t.Fatalf("sandbox_host_binds_dropped emitted %d times, want 1", len(dropped))
		}
		if want := []string{"/iterion/artifact-files", "/run/iterion/attachments"}; !reflect.DeepEqual(dropped[0]["mounts"], want) {
			t.Fatalf("the drop event lists %v, want %v", dropped[0]["mounts"], want)
		}
		if want := []string{runFilesEnvVar}; !reflect.DeepEqual(dropped[0]["withdrawn"], want) {
			t.Fatalf("the drop event withdraws %v, want %v", dropped[0]["withdrawn"], want)
		}
	})
	t.Run("a host filesystem: the binds stay, nothing is dropped or refused", func(t *testing.T) {
		p := canaryParams(t)
		attachTo(t, p.AttachmentsHostDir, "spec", "spec.md")
		events, err := startSandboxFor(t, true, p)
		if !errors.Is(err, errCanaryStop) {
			t.Fatalf("err = %v, want the canary's refusal at Prepare", err)
		}
		if n := len(capturedEventsOf(events, store.EventSandboxHostBindsDropped)); n != 0 {
			t.Fatalf("a driver with a host filesystem reported %d drops", n)
		}
	})
	t.Run("no host filesystem, no attachments: the drop is said, the run goes on", func(t *testing.T) {
		p := canaryParams(t) // the attachments dir exists and is empty
		events, err := startSandboxFor(t, false, p)
		if !errors.Is(err, errCanaryStop) {
			t.Fatalf("err = %v, want the canary's refusal at Prepare — a run with nothing to deliver was refused", err)
		}
		dropped := capturedEventsOf(events, store.EventSandboxHostBindsDropped)
		if len(dropped) != 1 {
			t.Fatalf("sandbox_host_binds_dropped emitted %d times, want 1", len(dropped))
		}
		if n := len(capturedEventsOf(events, store.EventSandboxSkipped)); n != 0 {
			t.Fatalf("sandbox_skipped emitted %d times on a run with no attachments", n)
		}
	})
}
