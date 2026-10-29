package kubernetes

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// #2086. A pod has no host filesystem to bind the run's attachments from,
// so the driver receives them as a seed (RunInfo.SeedDirs): the manifest
// declares an emptyDir at the seed's container path — the image's own
// directories are root-owned — and Start streams the directory in with
// the workspace's tar pipeline, then takes the write bit off the files.

// kubectlArgvShim puts a fake `kubectl` first on PATH that records every
// invocation's argv, keeps what `apply` was piped, and drains what `exec`
// is piped (the host tar must not die on a closed pipe).
func kubectlArgvShim(t *testing.T) (argvLog string) {
	t.Helper()
	isolateExecEnvironment(t)
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> \"$ITERION_TEST_KUBECTL_ARGV\"\n" +
		"case \"$*\" in\n" +
		"  *\" apply \"*) cat >> \"$ITERION_TEST_KUBECTL_ARGV.applied\"; printf '\\n---\\n' >> \"$ITERION_TEST_KUBECTL_ARGV.applied\"; exit 0 ;;\n" +
		"  *\" exec \"*) cat > /dev/null; exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ITERION_TEST_KUBECTL_ARGV", argvLog)
	return argvLog
}

func TestStartSeedsADirectoryIntoThePod(t *testing.T) {
	argvLog := kubectlArgvShim(t)
	sched, err := schedulingFromEnv(envOf(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{namespace: "ns", kubectl: "kubectl", logger: iterlog.New(iterlog.LevelInfo, io.Discard), sched: sched}
	seed := t.TempDir()
	if err := os.MkdirAll(filepath.Join(seed, "spec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "spec", "spec.md"), []byte("# spec"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared := &Prepared{spec: sandbox.Spec{Image: "img", User: "1000:1000"}, workspace: "/workspace"}
	const target = "/run/iterion/attachments"
	_, startErr := d.Start(context.Background(), prepared, sandbox.RunInfo{
		RunID:    "01a0-run",
		SeedDirs: []sandbox.SeedDir{{HostDir: seed, ContainerPath: target, ReadOnly: true}},
	})
	if startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}

	pod := appliedPod(t, argvLog+".applied")
	spec, _ := pod["spec"].(map[string]any)
	volumes, _ := spec["volumes"].([]any)
	seedVolume := false
	for _, v := range volumes {
		vm, _ := v.(map[string]any)
		if vm["name"] == "seed-0" {
			if _, ok := vm["emptyDir"]; !ok {
				t.Fatalf("the seed volume is not an emptyDir: %v", vm)
			}
			seedVolume = true
		}
	}
	if !seedVolume {
		t.Fatalf("the applied pod declares no seed-0 volume: %v", volumes)
	}
	containers, _ := spec["containers"].([]any)
	c, _ := containers[0].(map[string]any)
	mounts, _ := c["volumeMounts"].([]any)
	mounted := false
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		if mm["name"] == "seed-0" && mm["mountPath"] == target {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("the workload does not mount seed-0 at %s: %v", target, mounts)
	}

	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("no kubectl argv recorded: %v", err)
	}
	argv := string(raw)
	if !strings.Contains(argv, "tar -C "+target+" --no-overwrite-dir -xf -") {
		t.Fatalf("Start did not stream the seed into the pod:\n%s", argv)
	}
	if !strings.Contains(argv, "find "+target+" -type f -exec chmod a-w {} +") {
		t.Fatalf("Start left the seeded files writable:\n%s", argv)
	}
	if strings.Contains(argv, "tar -C /workspace") {
		t.Fatalf("a run with no workspace path copied a workspace:\n%s", argv)
	}
}

// A seed must land on a volume: a relative target names no volume mount
// and is refused before anything is applied.
func TestAManifestRefusesARelativeSeedTarget(t *testing.T) {
	_, err := BuildPodManifest(PodManifestInput{Namespace: "ns", Name: "p", RunID: "r", Spec: sandbox.Spec{Image: "img"}, SeedMounts: []string{"relative/dir"}})
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("err = %v, want the absolute-path refusal", err)
	}
}
