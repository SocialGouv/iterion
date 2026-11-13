package ciguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type stagingStep struct {
	Name string            `yaml:"name"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}
type stagingWorkflow struct {
	Jobs map[string]struct {
		Steps []stagingStep `yaml:"steps"`
	} `yaml:"jobs"`
}
type stagingInvocation struct{ run, attempt, label string }

// Execute the publication shell from the real workflow against a captured
// registry interleaving. Main and tag builds deliberately use the same SHA.
// A retry keeps a successful arm64 leg from attempt 1 while amd64 is rebuilt.
func TestImageStagingCannotMixWorkflowRuns(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../.github/workflows/_build-image.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf stagingWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	const image = "ghcr.io/example/image"
	evaluate := func(s string, inv stagingInvocation) string {
		s = strings.NewReplacer("${{ env.IMAGE_REF }}", image, "${{ github.sha }}", "same-source-sha", "${{ github.run_id }}", inv.run, "${{ github.run_attempt }}", inv.attempt, "${{ inputs.multiarch }}", "true", "${{ steps.meta.outputs.tags }}", image+":"+inv.label, "${{ steps.fasttags.outputs.tags }}", image+":"+inv.label).Replace(s)
		if strings.Contains(s, "${{") {
			t.Fatalf("unhandled workflow expression %q", s)
		}
		return s
	}
	stage := func(t *testing.T, registry map[string]string, arch string, inv stagingInvocation) {
		t.Helper()
		for _, step := range wf.Jobs[arch].Steps {
			if tag := step.With["tags"]; tag != "" && step.With["push"] == "true" {
				registry[evaluate(tag, inv)] = inv.label + "/" + arch
				return
			}
		}
		t.Fatalf("missing %s build staging tag", arch)
	}
	for _, mode := range []string{"multiarch", "single-arch", "partial-retry"} {
		t.Run(mode, func(t *testing.T) {
			want := stagingInvocation{"release-run", "1", "release"}
			other := stagingInvocation{"main-run", "1", "main"}
			registry := map[string]string{}
			if mode == "single-arch" {
				want, other = other, want
			}
			stage(t, registry, "amd64", want)
			stage(t, registry, "arm64", want)
			// The other workflow finishes after our architecture builds but before our publication.
			stage(t, registry, "amd64", other)
			stage(t, registry, "arm64", other)
			if mode == "partial-retry" {
				want.attempt = "2"
				stage(t, registry, "amd64", want)
			}
			job, name := "merge", "Create multi-arch index from per-arch staging manifests"
			if mode == "single-arch" {
				job, name = "amd64", "Publish final tags (single-arch fast path)"
			}
			var publish *stagingStep
			for _, step := range wf.Jobs[job].Steps {
				if step.Name == name {
					copy := step
					publish = &copy
					break
				}
			}
			if publish == nil {
				t.Fatal("publication step missing")
			}
			dir := t.TempDir()
			capture := filepath.Join(dir, "arguments")
			output := filepath.Join(dir, "outputs")
			fake := `#!/bin/sh
set -eu
[ "$1" = buildx ] && [ "$2" = imagetools ]
case "$3" in
 create) shift 3; printf '%s\n' "$@" > "$CAPTURE" ;;
 inspect) printf 'sha256:%064d\n' 0 ;;
 *) exit 91 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-c", publish.Run)
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "IMAGE_REF="+image, "CAPTURE="+capture, "GITHUB_OUTPUT="+output)
			for k, v := range publish.Env {
				cmd.Env = append(cmd.Env, k+"="+evaluate(v, want))
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("publication shell: %v\n%s", err, out)
			}
			args, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Fields(string(args))
			var sources []string
			for i := 0; i < len(lines); i++ {
				if lines[i] == "-t" {
					i++
					continue
				}
				sources = append(sources, lines[i])
			}
			expected := 2
			if mode == "single-arch" {
				expected = 1
			}
			if len(sources) != expected {
				t.Fatalf("sources %v, want %d", sources, expected)
			}
			seen := map[string]bool{}
			for _, source := range sources {
				arch := "amd64"
				if strings.HasSuffix(source, "-arm64") {
					arch = "arm64"
				}
				if seen[arch] {
					t.Errorf("duplicate architecture %s", arch)
				}
				seen[arch] = true
				if registry[source] != want.label+"/"+arch {
					t.Errorf("published %s from %q, want own %q build", source, registry[source], want.label)
				}
			}
		})
	}
}
