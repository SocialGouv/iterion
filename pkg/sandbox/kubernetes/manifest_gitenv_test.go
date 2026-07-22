package kubernetes

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// manifestEnv extracts the workload container's env as a name→value map
// from a rendered pod manifest.
func manifestEnv(t *testing.T, manifest []byte) map[string]string {
	t.Helper()
	var pod struct {
		Spec struct {
			Containers []struct {
				Env []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"env"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(manifest, &pod); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Spec.Containers))
	}
	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

// TestBuildPodManifest_GitSafeDirectoryEnv pins the fix for the live
// prod failure (run 019f8a50): the workspace emptyDir mountpoint is
// root-owned (kubelet; fsGroup only sets the group) while every exec
// runs as the non-root workload user, so git ≥2.35.2 flags the repo at
// the workspace root as dubiously owned and `git -C <ws> config` dies
// with "fatal: not in a git directory" (exit 128) — killing sandbox
// start at fixupWorkspaceGit. The pod env must carry safe.directory for
// the workspace via the protected GIT_CONFIG_* command scope, which
// every kubectl exec (fixup, agent git, tool nodes) inherits.
func TestBuildPodManifest_GitSafeDirectoryEnv(t *testing.T) {
	t.Run("default workspace", func(t *testing.T) {
		out, err := BuildPodManifest(PodManifestInput{
			Namespace: "ns", Name: "iterion-run-x", Spec: sandbox.Spec{Image: "img"},
		})
		if err != nil {
			t.Fatalf("BuildPodManifest: %v", err)
		}
		env := manifestEnv(t, out)
		if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "safe.directory" || env["GIT_CONFIG_VALUE_0"] != "/workspace" {
			t.Fatalf("safe.directory env not injected for the default workspace: %v", env)
		}
	})

	t.Run("host-mirror workspace mount (the repo-targeted cloud shape)", func(t *testing.T) {
		out, err := BuildPodManifest(PodManifestInput{
			Namespace: "ns", Name: "iterion-run-x",
			Spec:           sandbox.Spec{Image: "img"},
			WorkspaceMount: "/tmp/iterion/repos/run-1",
		})
		if err != nil {
			t.Fatalf("BuildPodManifest: %v", err)
		}
		env := manifestEnv(t, out)
		if env["GIT_CONFIG_VALUE_0"] != "/tmp/iterion/repos/run-1" {
			t.Fatalf("safe.directory must target the mounted workspace path: %v", env)
		}
	})

	t.Run("pre-existing GIT_CONFIG entries are preserved, ours appended", func(t *testing.T) {
		out, err := BuildPodManifest(PodManifestInput{
			Namespace: "ns", Name: "iterion-run-x",
			Spec: sandbox.Spec{Image: "img", Env: map[string]string{
				"GIT_CONFIG_COUNT":   "1",
				"GIT_CONFIG_KEY_0":   "commit.gpgsign",
				"GIT_CONFIG_VALUE_0": "false",
			}},
		})
		if err != nil {
			t.Fatalf("BuildPodManifest: %v", err)
		}
		env := manifestEnv(t, out)
		if env["GIT_CONFIG_KEY_0"] != "commit.gpgsign" || env["GIT_CONFIG_VALUE_0"] != "false" {
			t.Fatalf("pre-existing git-config env clobbered: %v", env)
		}
		if env["GIT_CONFIG_COUNT"] != "2" || env["GIT_CONFIG_KEY_1"] != "safe.directory" || env["GIT_CONFIG_VALUE_1"] != "/workspace" {
			t.Fatalf("safe.directory not appended after existing entries: %v", env)
		}
	})

	t.Run("malformed GIT_CONFIG_COUNT is a hard error", func(t *testing.T) {
		_, err := BuildPodManifest(PodManifestInput{
			Namespace: "ns", Name: "iterion-run-x",
			Spec: sandbox.Spec{Image: "img", Env: map[string]string{"GIT_CONFIG_COUNT": "banana"}},
		})
		if err == nil {
			t.Fatal("expected a hard error on a malformed GIT_CONFIG_COUNT")
		}
	})
}
