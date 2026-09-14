package portsactivation

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Real binaries with different build fingerprints share the same persisted
// native recovery format. The second build must not inherit launch authority,
// yet it must be able to recover the first build's admitted run after Disable.
func TestNativeAdmissionSurvivesCompatibleBuildUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two fixture executables")
	}
	root := t.TempDir()
	repo := filepath.Join("..", "..")
	var binaries [2]string
	for i, commit := range []string{"build-one", "build-two"} {
		binary := filepath.Join(t.TempDir(), "buildprobe")
		flags := "-X github.com/SocialGouv/iterion/pkg/internal/appinfo.Version=probe-" + commit +
			" -X github.com/SocialGouv/iterion/pkg/internal/appinfo.Commit=" + commit
		cmd := exec.Command("go", "build", "-mod=vendor", "-buildvcs=false", "-ldflags", flags, "-o", binary, "./pkg/portsactivation/testdata/buildprobe")
		cmd.Dir = repo
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build fixture %d: %v\n%s", i, err, output)
		}
		binaries[i] = binary
	}
	for i, mode := range []string{"admit", "recover"} {
		cmd := exec.Command(binaries[i], mode, root)
		cmd.Env = []string{}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("build fixture %d %s: %v\n%s", i, mode, err, output)
		}
		if mode == "recover" && !strings.Contains(string(output), "compatible build recovery admitted after rollback") {
			t.Fatalf("second build did not report recovery: %q", output)
		}
	}
}
