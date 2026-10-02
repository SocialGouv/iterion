package ciguard

import (
	"os"
	"regexp"
	"testing"
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
