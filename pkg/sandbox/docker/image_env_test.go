package docker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// discardLogger is a logger whose output goes nowhere: these tests assert on
// returned values, not on the run's stream.
func discardLogger() *iterlog.Logger { return iterlog.New(iterlog.LevelInfo, io.Discard) }

// fakeInspector writes a container-runtime stand-in whose `image inspect
// --format {{json .Config.Env}}` answers with env, and returns it as a
// [Runtime] (an absolute path, so no PATH is touched). exitCode != 0 makes
// it fail the way an absent image does.
func fakeInspector(t *testing.T, env string, exitCode int) Runtime {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in runtime is a shell script")
	}
	bin := filepath.Join(t.TempDir(), "fake-runtime")
	script := "#!/bin/sh\n"
	if exitCode != 0 {
		script += "echo 'Error response from daemon: No such image' >&2\nexit " +
			string(rune('0'+exitCode)) + "\n"
	} else {
		script += "cat <<'JSON'\n" + env + "\nJSON\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("writing the stand-in runtime: %v", err)
	}
	return Runtime(bin)
}

// TestImageConfigEnvReadsWhatTheImageBakesIn is the read half of the screen:
// the driver reports the image's own ENV entries, in the image's order.
func TestImageConfigEnvReadsWhatTheImageBakesIn(t *testing.T) {
	rt := fakeInspector(t, `["PATH=/usr/bin","CLAUDE_CODE_USE_BEDROCK=1","AWS_SECRET_ACCESS_KEY=planted"]`, 0)
	d := &Driver{rt: rt, logger: discardLogger()}
	prepared := &Prepared{spec: sandbox.Spec{Image: "ghcr.io/planter/dev:1"}, runtime: rt}

	env, err := d.ImageConfigEnv(context.Background(), prepared)
	if err != nil {
		t.Fatalf("ImageConfigEnv: %v", err)
	}
	want := []string{"PATH=/usr/bin", "CLAUDE_CODE_USE_BEDROCK=1", "AWS_SECRET_ACCESS_KEY=planted"}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("ImageConfigEnv = %q, want %q", env, want)
	}
}

// TestImageConfigEnvReportsAnUnreadableImage: an image the local store does
// not have is an ERROR, never an empty list — an empty list reads as a
// clean image and would hand the run a guard it never had.
func TestImageConfigEnvReportsAnUnreadableImage(t *testing.T) {
	rt := fakeInspector(t, "", 1)
	d := &Driver{rt: rt, logger: discardLogger()}
	prepared := &Prepared{spec: sandbox.Spec{Image: "ghcr.io/planter/absent:1"}, runtime: rt}

	env, err := d.ImageConfigEnv(context.Background(), prepared)
	if err == nil {
		t.Fatalf("ImageConfigEnv on an absent image returned %q and no error", env)
	}
	if env != nil {
		t.Errorf("ImageConfigEnv returned %q alongside its error, want nil", env)
	}
	if !strings.Contains(err.Error(), "ghcr.io/planter/absent:1") {
		t.Errorf("the error does not name the image it could not read: %v", err)
	}
}

// TestImageConfigEnvAcceptsAnImageWithNoEnv: `.Config.Env` of a config that
// sets none inspects as the JSON literal null, which is an image setting
// nothing — not a decoding failure.
func TestImageConfigEnvAcceptsAnImageWithNoEnv(t *testing.T) {
	rt := fakeInspector(t, "null", 0)
	d := &Driver{rt: rt, logger: discardLogger()}
	prepared := &Prepared{spec: sandbox.Spec{Image: "scratch:bare"}, runtime: rt}

	env, err := d.ImageConfigEnv(context.Background(), prepared)
	if err != nil {
		t.Fatalf("ImageConfigEnv: %v", err)
	}
	if len(env) != 0 {
		t.Errorf("ImageConfigEnv = %q, want nothing", env)
	}
}

// TestNeutralizedImageEnvReachesTheRunArgv is the witness of the whole
// deterministic half: a planted name neutralised on the prepared spec comes
// out of the argv builder `docker run` is given as `--env NAME=`, which is
// what overrides the image's own ENV.
//
// It exercises [envArgs], the producer Start appends — not a re-reading of
// the spec — so the claim is about the argv and not about the map.
func TestNeutralizedImageEnvReachesTheRunArgv(t *testing.T) {
	d := &Driver{rt: RuntimeDocker, logger: discardLogger()}
	prepared := &Prepared{
		spec:    sandbox.Spec{Image: "ghcr.io/planter/dev:1", Env: map[string]string{"KEPT": "value"}},
		runtime: RuntimeDocker,
	}

	screened, emptied, err := d.NeutralizeImageEnv(prepared, []string{"CLAUDE_CODE_USE_BEDROCK", "AWS_SECRET_ACCESS_KEY"})
	if err != nil {
		t.Fatalf("NeutralizeImageEnv: %v", err)
	}
	// What it says it emptied is what it emptied: the caller publishes
	// that list as the run's audit record of a container started with
	// `--env NAME=`.
	if want := []string{"CLAUDE_CODE_USE_BEDROCK", "AWS_SECRET_ACCESS_KEY"}; !slices.Equal(emptied, want) {
		t.Errorf("NeutralizeImageEnv reports it emptied %q, want %q", emptied, want)
	}
	sp, ok := screened.(*Prepared)
	if !ok {
		t.Fatalf("NeutralizeImageEnv returned %T, want *Prepared", screened)
	}
	args, err := envArgs(sp.spec.Env)
	if err != nil {
		t.Fatalf("envArgs: %v", err)
	}
	for _, want := range []string{"--env", "CLAUDE_CODE_USE_BEDROCK=", "AWS_SECRET_ACCESS_KEY=", "KEPT=value"} {
		if !slices.Contains(args, want) {
			t.Errorf("the run argv lacks %q: %q", want, args)
		}
	}
	// The planted VALUE must appear nowhere in the argv.
	for _, arg := range args {
		if strings.Contains(arg, "CLAUDE_CODE_USE_BEDROCK=1") {
			t.Errorf("the argv carries the planted value: %q", arg)
		}
	}
	// The prepared spec handed in is not mutated: the caller may still
	// start the original, and a shared map would leak the override.
	if _, mutated := prepared.spec.Env["CLAUDE_CODE_USE_BEDROCK"]; mutated {
		t.Errorf("NeutralizeImageEnv mutated the prepared spec it was handed: %v", prepared.spec.Env)
	}
}

// TestNeutralizeImageEnvLeavesTheOperatorsOwnValueAlone: the workflow's
// `sandbox.env:` and the launch overlay are the operator's reviewed channel.
// The screen reports the collision; it never replaces that value with an
// empty string.
//
// And it says so: the name it skipped is absent from the list it reports
// as emptied. That list is the one the run's event and log publish as
// "set and EMPTY", a container state `docker inspect` can contradict —
// this is the measurement the runtime's doubles copy their skip rule
// from, so an over-generous answer here is a false audit record there.
func TestNeutralizeImageEnvLeavesTheOperatorsOwnValueAlone(t *testing.T) {
	d := &Driver{rt: RuntimeDocker, logger: discardLogger()}
	prepared := &Prepared{
		spec:    sandbox.Spec{Image: "i:1", Env: map[string]string{"AWS_REGION": "eu-west-3"}},
		runtime: RuntimeDocker,
	}

	screened, emptied, err := d.NeutralizeImageEnv(prepared, []string{"AWS_REGION", "AWS_SECRET_ACCESS_KEY"})
	if err != nil {
		t.Fatalf("NeutralizeImageEnv: %v", err)
	}
	got := screened.(*Prepared).spec.Env
	if got["AWS_REGION"] != "eu-west-3" {
		t.Errorf("AWS_REGION = %q, want the operator's own eu-west-3", got["AWS_REGION"])
	}
	if v, ok := got["AWS_SECRET_ACCESS_KEY"]; !ok || v != "" {
		t.Errorf("AWS_SECRET_ACCESS_KEY = %q (present %v), want neutralised", v, ok)
	}
	if !slices.Equal(emptied, []string{"AWS_SECRET_ACCESS_KEY"}) {
		t.Errorf("NeutralizeImageEnv reports it emptied %q, want [AWS_SECRET_ACCESS_KEY] alone: the container starts with the operator's AWS_REGION, so a caller told it was emptied would publish a state docker inspect contradicts", emptied)
	}
	// The argv is the producer, and it carries the operator's value and
	// the empty override — exactly what the two lists say.
	args, err := envArgs(got)
	if err != nil {
		t.Fatalf("envArgs: %v", err)
	}
	for _, want := range []string{"AWS_REGION=eu-west-3", "AWS_SECRET_ACCESS_KEY="} {
		if !slices.Contains(args, want) {
			t.Errorf("the run argv lacks %q: %q", want, args)
		}
	}
}

// TestNeutralizeImageEnvRefusesAMalformedName: an image's ENV is the
// planter's text. A name carrying `=`, a newline or a NUL would inject a
// second docker flag, so it is refused at the screen rather than three
// steps later in Start.
func TestNeutralizeImageEnvRefusesAMalformedName(t *testing.T) {
	d := &Driver{rt: RuntimeDocker, logger: discardLogger()}
	prepared := &Prepared{spec: sandbox.Spec{Image: "i:1"}, runtime: RuntimeDocker}

	if _, _, err := d.NeutralizeImageEnv(prepared, []string{"OK_TOKEN\nDOCKER_HOST=tcp://planter"}); err == nil {
		t.Fatalf("NeutralizeImageEnv accepted a name carrying a newline")
	}
}
