package ciguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// The behaviour of scripts/brew-tap-resolve.sh: which tag brew-update.yml
// taps. It runs right after the nightly release run, whose own tag is still
// building at that moment — the cases below are the ways that timing could
// stall the tap, or walk it backwards.

const tapResolveScript = "../../scripts/brew-tap-resolve.sh"

// standInGH answers `gh release view <tag> --json assets --jq ...` with the
// asset names listed in $RELEASES/<tag>, and fails for a tag with no file —
// a release it cannot read.
const standInGH = `#!/usr/bin/env bash
[ "$1 $2" = "release view" ] || { echo "unexpected gh call: $*" >&2; exit 2; }
f="$RELEASES/$3"
[ -f "$f" ] || { echo "release not found" >&2; exit 1; }
cat "$f"
`

var completeCLI = []string{"iterion-darwin-arm64", "iterion-darwin-amd64", "iterion-linux-arm64", "iterion-linux-amd64"}

type tapRelease struct {
	tag    string
	assets []string // nil: no readable release
}

type resolved struct {
	code   int
	out    string
	output map[string]string // what the script wrote to GITHUB_OUTPUT
}

// resolveTap runs the resolver in a repository whose formula and cask carry
// the given versions and whose tags are the given releases.
func resolveTap(t *testing.T, formula, cask, inputTag string, releases ...tapRelease) resolved {
	t.Helper()
	dir := t.TempDir()
	gittest.InitRepo(t, dir)
	commit(t, dir, "tap", map[string]string{
		"Formula/iterion.rb":      "class Iterion < Formula\n  version \"" + formula + "\"\nend\n",
		"Cask/iterion-desktop.rb": "cask \"iterion-desktop\" do\n  version \"" + cask + "\"\nend\n",
	})

	fixtures := t.TempDir()
	for _, r := range releases {
		gittest.Run(t, dir, "tag", r.tag)
		if r.assets != nil {
			if err := os.WriteFile(filepath.Join(fixtures, r.tag), []byte(strings.Join(r.assets, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(standInGH), 0o755); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(t.TempDir(), "github_output")

	script, err := filepath.Abs(tapResolveScript)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", scriptUnderTest(t, script))
	cmd.Dir = dir
	cmd.Env = scriptEnv(t)
	for i, kv := range cmd.Env {
		if strings.HasPrefix(kv, "PATH=") {
			cmd.Env[i] = "PATH=" + bin + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
		}
	}
	cmd.Env = append(cmd.Env, "RELEASES="+fixtures, "GITHUB_OUTPUT="+outFile, "INPUT_TAG="+inputTag)
	out, err := cmd.CombinedOutput()
	r := resolved{out: string(out), output: map[string]string{}}
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v", script, err)
		}
		r.code = ee.ExitCode()
	}
	if raw, err := os.ReadFile(outFile); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				r.output[k] = v
			}
		}
	}
	return r
}

func expectTapped(t *testing.T, r resolved, tag string) {
	t.Helper()
	if r.code != 0 || r.output["tag"] != tag || r.output["skip"] != "" {
		t.Fatalf("want tag=%s and no skip, got exit %d, output %v:\n%s", tag, r.code, r.output, r.out)
	}
}

func expectNothingToTap(t *testing.T, r resolved) {
	t.Helper()
	if r.code != 0 || r.output["skip"] != "1" {
		t.Fatalf("want skip=1, got exit %d, output %v:\n%s", r.code, r.output, r.out)
	}
}

func TestTapResolvePassesOverATagStillBuilding(t *testing.T) {
	expectTapped(t, resolveTap(t, "1.0.0", "1.0.0", "",
		tapRelease{"v1.0.0", completeCLI},
		tapRelease{"v1.1.0", completeCLI},
		tapRelease{"v1.2.0", []string{"iterion-linux-amd64"}},
	), "v1.1.0")
}

func TestTapResolveReadsVersionsNotStrings(t *testing.T) {
	expectTapped(t, resolveTap(t, "1.0.0", "1.0.0", "",
		tapRelease{"v1.9.0", completeCLI},
		tapRelease{"v1.10.0", completeCLI},
	), "v1.10.0")
}

func TestTapResolveNeverWalksTheTapBackwards(t *testing.T) {
	// Nothing at or above the formula is complete: an older complete tag is
	// not a fallback.
	r := resolveTap(t, "1.1.0", "1.1.0", "",
		tapRelease{"v1.0.0", completeCLI},
		tapRelease{"v1.1.0", []string{"iterion-linux-amd64"}},
		tapRelease{"v1.2.0", []string{}},
	)
	expectNothingToTap(t, r)
	if tag := r.output["tag"]; tag != "" {
		t.Fatalf("resolved %s below the formula's 1.1.0", tag)
	}
}

func TestTapResolveFailsOnAReleaseItCannotRead(t *testing.T) {
	r := resolveTap(t, "1.0.0", "1.0.0", "",
		tapRelease{"v1.0.0", completeCLI},
		tapRelease{"v1.1.0", completeCLI},
		tapRelease{"v1.2.0", nil},
	)
	if r.code != 1 || !strings.Contains(r.out, "cannot read the release of v1.2.0") {
		t.Fatalf("want exit 1 naming v1.2.0, got exit %d, output %v:\n%s", r.code, r.output, r.out)
	}
}

func TestTapResolveSkipsATapThatCarriesTheTagTwice(t *testing.T) {
	// With the ZIP, so that only the both-carry rule can answer skip.
	r := resolveTap(t, "1.1.0", "1.1.0", "", tapRelease{"v1.1.0", withDesktopZIP()})
	expectNothingToTap(t, r)
	if r.output["tag"] != "v1.1.0" {
		t.Fatalf("want tag=v1.1.0 alongside the skip, got %v", r.output)
	}
}

func withDesktopZIP() []string {
	return append(append([]string{}, completeCLI...), "iterion-desktop-darwin-universal.zip")
}

func TestTapResolveCatchesUpACaskLeftBehind(t *testing.T) {
	expectTapped(t, resolveTap(t, "1.1.0", "1.0.0", "", tapRelease{"v1.1.0", withDesktopZIP()}), "v1.1.0")
}

func TestTapResolveNeverWalksTheCaskBack(t *testing.T) {
	// A desktop-only release dispatched into the cask leaves it ahead of the
	// formula; the formula's own tag must not pull it back.
	expectNothingToTap(t, resolveTap(t, "1.1.0", "1.2.0", "", tapRelease{"v1.1.0", withDesktopZIP()}))
}

func TestTapResolveLeavesTheCaskBehindUntilItsZIPExists(t *testing.T) {
	// Without the desktop ZIP the run would rewrite nothing, and hand
	// brew-tap-pr.sh the tag's branch — merged long ago — with nothing new.
	expectNothingToTap(t, resolveTap(t, "1.1.0", "1.0.0", "", tapRelease{"v1.1.0", completeCLI}))
	expectNothingToTap(t, resolveTap(t, "1.1.0", "1.0.0", "v1.1.0", tapRelease{"v1.1.0", completeCLI}))
}

func TestTapResolveNeverTapsAPreRelease(t *testing.T) {
	// sort -V ranks 1.2.0-rc1 above 1.2.0, and brew-tap-pr.sh refuses a
	// pre-release: resolving one would stall the final's update.
	expectTapped(t, resolveTap(t, "1.0.0", "1.0.0", "",
		tapRelease{"v1.1.0", completeCLI},
		tapRelease{"v1.2.0-rc1", completeCLI},
	), "v1.1.0")
	expectNothingToTap(t, resolveTap(t, "1.0.0", "1.0.0", "v1.2.0-rc1"))
}

func TestTapResolveTakesADispatchedTagUnlessItIsOlder(t *testing.T) {
	expectTapped(t, resolveTap(t, "1.0.0", "1.0.0", "v1.3.0"), "v1.3.0")
	expectNothingToTap(t, resolveTap(t, "1.1.0", "1.1.0", "v1.0.0"))
}
