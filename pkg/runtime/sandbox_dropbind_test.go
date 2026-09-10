package runtime

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// TestDropUnsupportedHostBindsWithdrawsTheRunFilesEnv pins the promise
// ITERION_ARTIFACT_FILES_DIR makes: it names a directory that exists. On a
// driver without a host filesystem the run-files bind is dropped, so the
// variable naming its target goes too — a tool that trusted it wrote into
// "Directory nonexistent" and a lot read an oracle verdict out of that. The
// rule is keyed on the mounts: a target a dropped bind served is gone unless
// a mount the driver honours still serves it.
func TestDropUnsupportedHostBindsWithdrawsTheRunFilesEnv(t *testing.T) {
	const path = "/iterion/artifact-files"
	newSpec := func() *sandbox.Spec {
		return &sandbox.Spec{
			Mounts: []string{
				"type=bind,source=/var/lib/iterion/run-files/r1,target=" + path,
				"type=pvc,source=claim,target=/data",
			},
			Env: map[string]string{runFilesEnvVar: path, "KEEP": "1"},
		}
	}
	t.Run("the variable goes with the bind that served its target", func(t *testing.T) {
		spec := newSpec()
		dropUnsupportedHostBinds(spec, "", nil)
		if v, ok := spec.Env[runFilesEnvVar]; ok {
			t.Fatalf("%s still set to %q after its bind was dropped", runFilesEnvVar, v)
		}
		if spec.Env["KEEP"] != "1" {
			t.Fatalf("an unrelated variable was touched: %v", spec.Env)
		}
		if want := []string{"type=pvc,source=claim,target=/data"}; !reflect.DeepEqual(spec.Mounts, want) {
			t.Fatalf("mounts kept %v, want %v", spec.Mounts, want)
		}
	})
	t.Run("a pvc the bot declared at the same target keeps the directory, and the variable", func(t *testing.T) {
		spec := newSpec()
		spec.Mounts = append(spec.Mounts, "type=pvc,source=my-claim,target="+path)
		dropUnsupportedHostBinds(spec, "", nil)
		if spec.Env[runFilesEnvVar] != path {
			t.Fatalf("the variable was withdrawn although a pvc still serves %s: %v", path, spec.Env)
		}
		if want := []string{"type=pvc,source=claim,target=/data", "type=pvc,source=my-claim,target=" + path}; !reflect.DeepEqual(spec.Mounts, want) {
			t.Fatalf("mounts kept %v, want %v", spec.Mounts, want)
		}
	})
	t.Run("a value naming a path no bind ever served is not the runtime's promise", func(t *testing.T) {
		spec := newSpec()
		spec.Env[runFilesEnvVar] = "/mnt/files"
		dropUnsupportedHostBinds(spec, "", nil)
		if spec.Env[runFilesEnvVar] != "/mnt/files" {
			t.Fatalf("the bot's own value was withdrawn: %v", spec.Env)
		}
		if len(spec.Mounts) != 1 {
			t.Fatalf("host binds survived: %v", spec.Mounts)
		}
	})
	t.Run("a nil env is fine", func(t *testing.T) {
		spec := &sandbox.Spec{Mounts: []string{"type=bind,source=/x,target=" + path}}
		dropUnsupportedHostBinds(spec, "", nil)
		if len(spec.Mounts) != 0 {
			t.Fatalf("host bind survived: %v", spec.Mounts)
		}
	})
	t.Run("a value under a dropped bind's target goes with it", func(t *testing.T) {
		spec := newSpec()
		spec.Env[runFilesEnvVar] = path + "/out"
		dropUnsupportedHostBinds(spec, "", nil)
		if v, ok := spec.Env[runFilesEnvVar]; ok {
			t.Fatalf("%s still set to %q although the bind serving its parent was dropped", runFilesEnvVar, v)
		}
	})
	t.Run("a value under a mount the driver honours is kept", func(t *testing.T) {
		spec := newSpec()
		spec.Env[runFilesEnvVar] = "/data/out"
		dropUnsupportedHostBinds(spec, "", nil)
		if spec.Env[runFilesEnvVar] != "/data/out" {
			t.Fatalf("a value under the pvc's target was withdrawn: %v", spec.Env)
		}
	})
	t.Run("the bot's own value is handed back when the runtime's promise is withdrawn", func(t *testing.T) {
		spec := newSpec()
		spec.Mounts = append(spec.Mounts, "type=pvc,source=files,target=/mnt/files")
		dropUnsupportedHostBinds(spec, "/mnt/files", nil)
		if spec.Env[runFilesEnvVar] != "/mnt/files" {
			t.Fatalf("the bot's value was not handed back: %v", spec.Env)
		}
	})
	t.Run("a bot value that names a dropped target is not handed back", func(t *testing.T) {
		spec := newSpec()
		dropUnsupportedHostBinds(spec, path+"/mine", nil)
		if v, ok := spec.Env[runFilesEnvVar]; ok {
			t.Fatalf("a value under the dropped bind was handed back: %q", v)
		}
	})
	t.Run("a spaced field or a trailing slash names the same directory", func(t *testing.T) {
		for _, mount := range []string{
			"type=bind, source=/var/lib/iterion/run-files/r1, target=" + path,
			"type=bind,source=/var/lib/iterion/run-files/r1,target=" + path + "/",
		} {
			spec := &sandbox.Spec{Mounts: []string{mount}, Env: map[string]string{runFilesEnvVar: path}}
			dropUnsupportedHostBinds(spec, "", nil)
			if v, ok := spec.Env[runFilesEnvVar]; ok {
				t.Fatalf("%q: %s still set to %q after its bind was dropped", mount, runFilesEnvVar, v)
			}
		}
	})
}

// TestFinalizeMountsForDriver pins the call site the pod backend takes: with
// no host filesystem every host bind drops, the run-files variable goes with
// its bind and the attachments path is withdrawn; with one, nothing moves.
func TestFinalizeMountsForDriver(t *testing.T) {
	const path = "/iterion/artifact-files"
	newSpec := func() *sandbox.Spec {
		return &sandbox.Spec{
			Mounts: []string{
				"type=bind,source=/var/lib/iterion/run-files/r1,target=" + path,
				"type=bind,source=/var/lib/iterion/attachments/r1,target=/run/iterion/attachments",
				"type=secret,source=sec,target=/run/secrets/forge_token",
			},
			Env: map[string]string{runFilesEnvVar: path},
		}
	}
	t.Run("no host filesystem: binds, variable and attachments path all go", func(t *testing.T) {
		spec := newSpec()
		got := finalizeMountsForDriver(spec, sandbox.Capabilities{}, "/run/iterion/attachments", "", nil)
		if got != "" {
			t.Fatalf("attachments path %q handed out although its bind was dropped", got)
		}
		if _, ok := spec.Env[runFilesEnvVar]; ok {
			t.Fatalf("%s survived the drop: %v", runFilesEnvVar, spec.Env)
		}
		if want := []string{"type=secret,source=sec,target=/run/secrets/forge_token"}; !reflect.DeepEqual(spec.Mounts, want) {
			t.Fatalf("mounts kept %v, want %v", spec.Mounts, want)
		}
	})
	t.Run("a host filesystem: nothing moves", func(t *testing.T) {
		spec := newSpec()
		got := finalizeMountsForDriver(spec, sandbox.Capabilities{SupportsHostBindMounts: true}, "/run/iterion/attachments", "", nil)
		if got != "/run/iterion/attachments" {
			t.Fatalf("attachments path withdrawn on a driver that honours binds: %q", got)
		}
		if spec.Env[runFilesEnvVar] != path || len(spec.Mounts) != 3 {
			t.Fatalf("spec changed on a driver that honours binds: env %v mounts %v", spec.Env, spec.Mounts)
		}
	})
}

func TestMountIsHostBind(t *testing.T) {
	cases := []struct {
		mount string
		want  bool
	}{
		// explicit type=bind (the ~/.claude OAuth mount bots author for docker)
		{"type=bind,source=/home/u/.claude,target=/home/devbox/.claude,consistency=cached", true},
		{"source=/x,target=/y,type=bind,readonly", true},
		// no explicit type + a source ⇒ docker's bind default
		{"source=/x,target=/y", true},
		// cloud-supported mount types are never host binds
		{"type=pvc,source=myclaim,target=/data", false},
		{"type=configmap,source=cm,target=/etc/cfg", false},
		{"type=secret,source=sec,target=/run/secrets/x", false},
	}
	for _, c := range cases {
		if got := mountIsHostBind(c.mount); got != c.want {
			t.Errorf("mountIsHostBind(%q) = %v, want %v", c.mount, got, c.want)
		}
	}
}

func TestDropHostBindMounts(t *testing.T) {
	in := []string{
		"type=bind,source=/home/u/.claude,target=/home/devbox/.claude,consistency=cached",
		"type=pvc,source=claim,target=/data",
		"source=/host/bin,target=/usr/local/bin/iterion,type=bind,readonly",
		"type=secret,source=sec,target=/run/secrets/forge_token",
	}
	got := dropHostBindMounts(in, nil)
	want := []string{
		"type=pvc,source=claim,target=/data",
		"type=secret,source=sec,target=/run/secrets/forge_token",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dropHostBindMounts kept %v, want %v", got, want)
	}
	// nil / empty passes through untouched.
	if out := dropHostBindMounts(nil, nil); out != nil {
		t.Errorf("dropHostBindMounts(nil) = %v, want nil", out)
	}
}
