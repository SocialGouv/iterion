package ciguard

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// runnerDockerfilePath builds the image the organisation's runners use
// (iterion-ci-runner, published by ci-runner-image.yml).
const runnerDockerfilePath = "../../ci/arc-runner/Dockerfile"

var (
	goModGoLine        = regexp.MustCompile(`(?m)^go ([0-9][0-9.]*)\s*$`)
	goModToolchainLine = regexp.MustCompile(`(?m)^toolchain go([0-9][0-9.]*)\s*$`)
	dockerGoVersion    = regexp.MustCompile(`(?m)^ARG GO_VERSION=(\S+)\s*$`)
)

// TestRunnerImageBakesGoModsGo holds the Go the runner image bakes to the one
// go.mod asks for. setup-go reads go.mod — its `toolchain` line when there is
// one, its `go` line otherwise — and looks for that exact version in the
// image's tool cache before downloading it. A Go bump that leaves the image
// behind still works — every job downloads the toolchain again, ~30 s each —
// so nothing would ever turn red. The bump and the image move in the same
// change, and GO_SHA256 moves with them (the image build verifies it).
func TestRunnerImageBakesGoModsGo(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	dockerfile, err := os.ReadFile(runnerDockerfilePath)
	if err != nil {
		t.Fatalf("read %s: %v", runnerDockerfilePath, err)
	}
	want := goModToolchainLine.FindSubmatch(gomod)
	if want == nil {
		want = goModGoLine.FindSubmatch(gomod)
	}
	if want == nil {
		t.Fatal("go.mod has no `go` line this guard can read")
	}
	got := dockerGoVersion.FindSubmatch(dockerfile)
	if got == nil {
		t.Fatalf("%s has no `ARG GO_VERSION=` line — the runner image no longer bakes Go, or the line moved; keep this guard in step", runnerDockerfilePath)
	}
	if string(got[1]) != string(want[1]) {
		t.Errorf("go.mod asks for Go %s and the runner image bakes %s: bump GO_VERSION and GO_SHA256 in %s (https://go.dev/dl/?mode=json lists the sha256 of go%s.linux-amd64.tar.gz)", want[1], got[1], runnerDockerfilePath, want[1])
	}
}

// runnerImageWorkflowPath builds AND publishes the image above. It carries the
// one `packages: write` of this repository, so the two halves of it are held
// apart here: a build half that runs on every pull request with
// `contents: read` alone, and a publish half that runs on main, holds the
// scope, and pushes the bytes the build half validated rather than an image it
// rebuilt (#2787, ADR-105).
const runnerImageWorkflowPath = "../../.github/workflows/ci-runner-image.yml"

const (
	runnerImageBuildJob   = "build"
	runnerImagePublishJob = "publish"
	// The condition is written once in the file and used by the publish job
	// and by the two steps that hand the artefact over; the guard below holds
	// the three to the same string, because a publish job that is main-only
	// while the handover is not would fail only on a real push to main.
	runnerImageMainOnly = "github.event_name != 'pull_request' && github.ref == 'refs/heads/main'"
)

// runnerImageWorkflow reads the shipped text — the jobs, their scopes, their
// steps — rather than a copy of it kept here, because a copy is what drifts.
type runnerImageWorkflow struct {
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		Needs       any               `yaml:"needs"`
		If          string            `yaml:"if"`
		RunsOn      string            `yaml:"runs-on"`
		Permissions map[string]string `yaml:"permissions"`
		Env         map[string]string `yaml:"env"`
		Outputs     map[string]string `yaml:"outputs"`
		Steps       []struct {
			Name string         `yaml:"name"`
			Uses string         `yaml:"uses"`
			If   string         `yaml:"if"`
			Run  string         `yaml:"run"`
			With map[string]any `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readRunnerImageWorkflow(t *testing.T) runnerImageWorkflow {
	t.Helper()
	var wf runnerImageWorkflow
	if err := yaml.Unmarshal(mustRead(t, runnerImageWorkflowPath), &wf); err != nil {
		t.Fatalf("parse %s: %v", runnerImageWorkflowPath, err)
	}
	for _, job := range []string{runnerImageBuildJob, runnerImagePublishJob} {
		if _, ok := wf.Jobs[job]; !ok {
			t.Fatalf("%s defines no job %q — the file no longer has the shape these guards read", runnerImageWorkflowPath, job)
		}
	}
	return wf
}

// TestOnlyTheRunnerImagePublishJobHoldsPackagesWrite is the guard the ticket
// exists for: the registry-writing scope reaches the job that pushes on main,
// and nothing else — not the workflow, not the build job a pull request runs.
func TestOnlyTheRunnerImagePublishJobHoldsPackagesWrite(t *testing.T) {
	wf := readRunnerImageWorkflow(t)

	if _, held := wf.Permissions["packages"]; held {
		t.Errorf("%s grants packages:%s at WORKFLOW level — every job of every run gets it, pull requests included, which is the exposure #2787 closes; grant it on the %q job alone",
			runnerImageWorkflowPath, wf.Permissions["packages"], runnerImagePublishJob)
	}
	build := wf.Jobs[runnerImageBuildJob]
	if len(build.Permissions) != 1 || build.Permissions["contents"] != "read" {
		t.Errorf("job %q carries permissions %v, want exactly {contents: read} — it builds and validates on every pull request and must not be able to write the registry", runnerImageBuildJob, build.Permissions)
	}
	publish := wf.Jobs[runnerImagePublishJob]
	if len(publish.Permissions) != 2 || publish.Permissions["contents"] != "read" || publish.Permissions["packages"] != "write" {
		t.Errorf("job %q carries permissions %v, want exactly {contents: read, packages: write} — the one scope that pushes, on the one job that does", runnerImagePublishJob, publish.Permissions)
	}
}

// TestTheRunnerImagePublishJobPushesWhatItReceived holds the publish job to
// loading the artefact: `docker build` there would publish an image nothing
// validated, since the context's apt and curl answer to upstream and the
// rebuild is not the bytes verify.sh judged.
func TestTheRunnerImagePublishJobPushesWhatItReceived(t *testing.T) {
	wf := readRunnerImageWorkflow(t)
	publish := wf.Jobs[runnerImagePublishJob]

	if needs, _ := publish.Needs.(string); needs != runnerImageBuildJob {
		t.Errorf("job %q needs %v, want the %q job — without it the publish half can start before anything validated the image", runnerImagePublishJob, publish.Needs, runnerImageBuildJob)
	}
	var loads, builds, pushes bool
	for _, st := range publish.Steps {
		switch {
		case strings.Contains(st.Run, "docker load"):
			loads = true
		case strings.Contains(st.Run, "docker build"):
			builds = true
		}
		if strings.Contains(st.Run, "docker push") {
			pushes = true
		}
	}
	if !loads {
		t.Errorf("job %q runs no `docker load` — it publishes the image it received, not one it rebuilt", runnerImagePublishJob)
	}
	if builds {
		t.Errorf("job %q runs `docker build` — that is an image no validation judged; it must load the artefact the build job saved", runnerImagePublishJob)
	}
	if !pushes {
		t.Errorf("job %q runs no `docker push` — nothing publishes anything", runnerImagePublishJob)
	}
	// One truth for the tag: the publish job takes it from the build job's
	// output. Recomputed from the run number, it diverges on a re-attempt.
	if got := publish.Env["CI_RUNNER_IMAGE"]; !strings.Contains(got, "needs."+runnerImageBuildJob+".outputs.image") {
		t.Errorf("job %q reads CI_RUNNER_IMAGE from %q, want the %q job's `image` output — a tag recomputed downstream is a second source of one truth that diverges on a re-run attempt", runnerImagePublishJob, got, runnerImageBuildJob)
	}
	if out := wf.Jobs[runnerImageBuildJob].Outputs["image"]; !strings.Contains(out, "steps.") {
		t.Errorf("job %q exposes image = %q, want the step output that computed the tag", runnerImageBuildJob, out)
	}
}

// TestTheRunnerImagePublishPathIsMainOnly keeps the three places that gate on
// main saying the same thing: the publish job, and the two steps of the build
// job that hand the artefact over. A handover gated differently from the job
// that consumes it fails only on a real push to main.
func TestTheRunnerImagePublishPathIsMainOnly(t *testing.T) {
	wf := readRunnerImageWorkflow(t)

	if got := strings.TrimSpace(wf.Jobs[runnerImagePublishJob].If); got != runnerImageMainOnly {
		t.Errorf("job %q runs `if: %s`, want exactly %q — a publish job that can run off main, or on a pull request, is the exposure the split closes", runnerImagePublishJob, got, runnerImageMainOnly)
	}
	var gated int
	for _, st := range wf.Jobs[runnerImageBuildJob].Steps {
		if !strings.Contains(st.Run, "docker save") && !strings.Contains(st.Uses, "upload-artifact") {
			continue
		}
		gated++
		if got := strings.TrimSpace(st.If); got != runnerImageMainOnly {
			t.Errorf("step %q of job %q runs `if: %s`, want the same condition as the publish job (%q)", st.Name, runnerImageBuildJob, got, runnerImageMainOnly)
		}
	}
	if gated != 2 {
		t.Errorf("job %q has %d handover step(s) (a `docker save` and an upload-artifact), want 2 — the guard above then checks a condition nobody carries", runnerImageBuildJob, gated)
	}
}

// TestTheRunnerImageArtefactIsOneNameOnBothSides reads the handover the way the
// runner does: by name. Two names that disagree make the publish job fail only
// on a real push to main, after the build it was supposed to publish.
func TestTheRunnerImageArtefactIsOneNameOnBothSides(t *testing.T) {
	wf := readRunnerImageWorkflow(t)

	name := func(job, action string) string {
		t.Helper()
		for _, st := range wf.Jobs[job].Steps {
			if !strings.Contains(st.Uses, action) {
				continue
			}
			got, _ := st.With["name"].(string)
			if strings.TrimSpace(got) == "" {
				t.Fatalf("the %s step of job %q names no artefact — the handover would use the run's default name", action, job)
			}
			return got
		}
		t.Fatalf("job %q has no %s step — the validated image does not cross", job, action)
		return ""
	}
	if up, down := name(runnerImageBuildJob, "upload-artifact"), name(runnerImagePublishJob, "download-artifact"); up != down {
		t.Errorf("the build job uploads %q and the publish job downloads %q — one string, or the handover drifts silently", up, down)
	}
}

// TestTheRunnerImageArtefactActionsArePinnedLikeTheRest keeps the two new
// third-party actions of the pipeline that holds `packages: write` on a
// commit sha, and on one this tree already trusts elsewhere: a tag is a
// mutable reference to code that would run beside that token.
func TestTheRunnerImageArtefactActionsArePinnedLikeTheRest(t *testing.T) {
	// Two readings of the same shape: one anchored, to judge a `uses:` value
	// whole (a tag suffix must not pass), one unanchored, to harvest the pins
	// the rest of the tree carries.
	pinned := regexp.MustCompile(`^([\w.-]+/[\w.-]+)@([0-9a-f]{40})$`)
	anyPin := regexp.MustCompile(`([\w.-]+/[\w.-]+)@([0-9a-f]{40})`)
	elsewhere := map[string]bool{}
	dir := "../../.github/workflows"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the workflow directory: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == filepath.Base(runnerImageWorkflowPath) {
			continue
		}
		for _, m := range anyPin.FindAllString(string(mustRead(t, filepath.Join(dir, e.Name()))), -1) {
			elsewhere[m] = true
		}
	}
	if len(elsewhere) < 5 {
		t.Fatalf("harvested %d pinned action(s) from %s — the comparison below would accept anything", len(elsewhere), dir)
	}
	wf := readRunnerImageWorkflow(t)
	var seen int
	for job, spec := range wf.Jobs {
		for _, st := range spec.Steps {
			if !strings.Contains(st.Uses, "upload-artifact") && !strings.Contains(st.Uses, "download-artifact") {
				continue
			}
			seen++
			if !pinned.MatchString(st.Uses) {
				t.Errorf("job %q step %q uses %q — pin the action by its 40-character commit sha, never by tag: this is the pipeline that holds packages: write", job, st.Name, st.Uses)
				continue
			}
			if !elsewhere[st.Uses] {
				t.Errorf("job %q step %q uses %q, a pin no other workflow of this tree carries — reuse the sha already in tree (tests.yml for upload-artifact, release.yml for download-artifact) rather than introducing a second one", job, st.Name, st.Uses)
			}
		}
	}
	if seen != 2 {
		t.Errorf("%s carries %d artefact action step(s), want 2 (one upload, one download) — the handover is not what this guard reads", runnerImageWorkflowPath, seen)
	}
}
