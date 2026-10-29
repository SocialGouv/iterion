package ciguard

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The actions the ARC runner image bakes, and the proof they resolve without
// codeload.github.com.
//
// Why (#2435): an ephemeral runner starts with an empty `_actions`, so every
// job downloads every action again. On 2026-10-15 codeload answered 429 and
// `test-unit`, `vendor-check` and `fmt-check` failed before any step ran — the
// fourth merge-queue ejection of that class — and a third party measured the
// same throttling with no ARC involved, so the remedy cannot rely on the
// service behaving. Baking the archives does not; a mirror or a shared mutable
// cache would.
//
// The drift class these guards close is the one runner_image_test.go already
// guards for Go: an image that bakes a set the workflows no longer ask for
// looks healthy and silently goes back to downloading.
const (
	bakedManifestScript  = "../../scripts/ci/baked-actions-manifest.sh"
	bakedVerifierScript  = "../../scripts/ci/verify-baked-actions.sh"
	bakedManifestPath    = "../../ci/arc-runner/baked-actions.txt"
	runnerVerifierPath   = "../../ci/arc-runner/verify.sh"
	bakedCacheEnvName    = "ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE"
	runnerWorkVolumePath = "/home/runner/_work"
)

var (
	dockerArchiveCacheEnv = regexp.MustCompile(`(?m)^ENV ` + bakedCacheEnvName + `=(\S+)\s*$`)
	dockerToolCacheEnv    = regexp.MustCompile(`(?m)^ENV RUNNER_TOOL_CACHE=(\S+)\s*$`)
	dockerManifestCopy    = regexp.MustCompile(`(?m)^COPY baked-actions\.txt (\S+)\s*$`)
)

// scriptUnderTest returns the path of a script this package runs through a
// child shell, after READING it in the test process.
//
// The read is the point. `go test` caches a result against the files the test
// process itself opened, and a script a child shell reads is invisible to that
// bookkeeping: with a warm cache, editing the shipped script and re-running
// the guard replays the previous PASS. Measured here while proving mutants of
// scripts/ci/baked-actions-manifest.sh — two of them came back `ok (cached)`
// on a script whose rule had been deleted. Every helper in this package that
// hands a script path to a child goes through this.
func scriptUnderTest(t *testing.T, path string) string {
	t.Helper()
	if len(mustRead(t, path)) == 0 {
		t.Fatalf("%s is empty — a guard that runs an empty script proves nothing", path)
	}
	return path
}

// runScript runs one of the shipped scripts and returns stdout, stderr and the
// exit code — the scripts are the implementation, so the guards exercise them
// rather than a Go re-reading of the same rule.
func runScript(t *testing.T, script string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{scriptUnderTest(t, script)}, args...)...)
	cmd.Env = scriptEnv(t)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s %v: %v\nstderr: %s", script, args, err, errb.String())
		}
		code = exit.ExitCode()
	}
	return out.String(), errb.String(), code
}

// TestTheRunnerImageBakesTheActionsTheWorkflowsAskFor holds the committed
// manifest to what the workflows use, by running the generator the maintainer
// runs.
//
// Mutations seen red: a `uses:` added to .github/workflows/arc-canary.yml (the
// test names the action missing from the manifest); one line deleted from
// ci/arc-runner/baked-actions.txt (same, from the other side).
func TestTheRunnerImageBakesTheActionsTheWorkflowsAskFor(t *testing.T) {
	want, stderr, code := runScript(t, bakedManifestScript, workflowDir)
	if code != 0 {
		t.Fatalf("%s refused the workflows of this tree (exit %d): %s", bakedManifestScript, code, stderr)
	}
	got := string(mustRead(t, bakedManifestPath))
	if got == want {
		if lines := strings.Count(strings.TrimSpace(want), "\n") + 1; lines < 10 {
			t.Errorf("the manifest holds %d action(s) — this tree pins far more, so the generator is reading the wrong files", lines)
		}
		return
	}
	for _, line := range missingLines(want, got) {
		t.Errorf("%s asks for %q and %s does not bake it: regenerate with `bash scripts/ci/baked-actions-manifest.sh > ci/arc-runner/baked-actions.txt`", workflowDir, line, bakedManifestPath)
	}
	for _, line := range missingLines(got, want) {
		t.Errorf("%s bakes %q, which no `uses:` of %s asks for: regenerate the manifest rather than hand-editing it", bakedManifestPath, line, workflowDir)
	}
}

// missingLines returns the lines of a that b does not carry.
func missingLines(a, b string) []string {
	have := map[string]bool{}
	for _, line := range strings.Split(b, "\n") {
		have[line] = true
	}
	var missing []string
	for _, line := range strings.Split(a, "\n") {
		if line != "" && !have[line] {
			missing = append(missing, line)
		}
	}
	return missing
}

// TestTheManifestGeneratorRefusesAnActionItCannotKey drives the generator over
// crafted workflows, in both directions: what it must harvest, and what it
// must refuse.
//
// The refusal is the load-bearing half. The cache is keyed by the sha the
// Actions service resolves, so a tag-pinned action misses it and goes straight
// back to codeload — a hole that looks exactly like a working cache. The
// generator names the file and line instead of skipping the line, because a
// manifest silently missing an action is the failure this whole unit is about.
//
// Mutation seen red: the `[0-9a-f]{40}` pin test in
// scripts/ci/baked-actions-manifest.sh relaxed to `.+` — the tag case stops
// being refused and this test fails on its own assertion.
func TestTheManifestGeneratorRefusesAnActionItCannotKey(t *testing.T) {
	const sha = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	const otherSha = "924ae3a1cded613372ab5595356fb5720e22ba16"
	for _, tc := range []struct {
		name     string
		files    map[string]string
		want     string
		wantCode int
		wantErr  string
	}{
		{
			name: "a step, a reusable call and a quoted value, deduplicated and sorted",
			files: map[string]string{"a.yml": "" +
				"jobs:\n  one:\n    steps:\n      - uses: actions/checkout@" + sha + " # v7.0.1\n" +
				"      - uses: actions/checkout@" + sha + "\n" +
				"      - uses: \"actions/setup-go@" + otherSha + "\"\n" +
				"  two:\n    uses: ./.github/workflows/_build-image.yml\n",
			},
			want: "actions/checkout " + sha + " .\nactions/setup-go " + otherSha + " .\n",
		},
		{
			name: "an action in a subdirectory keeps the path its uses: names",
			files: map[string]string{"a.yml": "      - uses: github/codeql-action/upload-sarif@" + sha + "\n" +
				"      - uses: github/codeql-action/init@" + sha + "\n"},
			want: "github/codeql-action " + sha + " init\ngithub/codeql-action " + sha + " upload-sarif\n",
		},
		{
			name: "a reusable workflow of another repository is not an action",
			files: map[string]string{"a.yml": "" +
				"jobs:\n  one:\n    uses: another/repo/.github/workflows/build.yml@" + sha + "\n" +
				"  two:\n    steps:\n      - uses: actions/checkout@" + sha + "\n"},
			want: "actions/checkout " + sha + " .\n",
		},
		{
			name: "two spellings of one repository are refused",
			files: map[string]string{"a.yml": "" +
				"      - uses: azure/setup-helm@" + sha + "\n" +
				"      - uses: Azure/setup-helm@" + otherSha + "\n"},
			wantCode: 1,
			wantErr:  "is spelled azure/setup-helm elsewhere",
		},
		{
			name:     "a tag is refused, naming the file and the line",
			files:    map[string]string{"a.yml": "steps:\n      - uses: actions/checkout@v7\n"},
			wantCode: 1,
			wantErr:  "a.yml:2: `uses: actions/checkout@v7` cannot be baked",
		},
		{
			name:     "a short sha is refused too: the cache key is the full one",
			files:    map[string]string{"a.yml": "      - uses: actions/checkout@3d3c42e\n"},
			wantCode: 1,
			wantErr:  "cannot be baked",
		},
		{
			name:     "a directory whose workflows pin nothing bakes nothing, and says so",
			files:    map[string]string{"a.yml": "jobs:\n  one:\n    uses: ./.github/workflows/_build-image.yml\n"},
			wantCode: 1,
			wantErr:  "no sha-pinned action found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, tc.files)
			stdout, stderr, code := runScript(t, bakedManifestScript, dir)
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.wantCode, stdout, stderr)
			}
			if tc.wantCode == 0 && stdout != tc.want {
				t.Errorf("manifest:\n%s\nwant:\n%s", stdout, tc.want)
			}
			if tc.wantCode != 0 {
				if stdout != "" {
					t.Errorf("a refusal wrote %q to stdout — a caller redirecting this script into the manifest would get half of one", stdout)
				}
				if !strings.Contains(stderr, tc.wantErr) {
					t.Errorf("stderr %q does not carry %q", stderr, tc.wantErr)
				}
			}
		})
	}
}

// writeActionArchive writes a .tar.gz shaped like the repository archive
// GitHub serves: one nested directory holding the files named.
func writeActionArchive(t *testing.T, path string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTheOfflineActionVerifierRefusesAGapInTheCache drives the verifier the
// image's own check runs — with no network, which is how ci/arc-runner's
// verify.sh runs it — over a cache built here.
//
// It is the negative control the image README already uses for gcc, applied to
// the actions: remove one archive and the image fails its verification instead
// of shipping a cache with a hole in it, which would read as a working cache
// and quietly cost every job a codeload download.
//
// Mutations seen red, each on its own case: the `-s` archive test dropped (a
// missing archive passes); the single-nested-directory check dropped; the
// action.yml lookup dropped; the `checked -eq 0` refusal dropped (an empty
// manifest reports success).
func TestTheOfflineActionVerifierRefusesAGapInTheCache(t *testing.T) {
	const sha = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	root := "checkout-" + sha
	for _, tc := range []struct {
		name     string
		manifest string
		build    func(t *testing.T, cache string)
		wantCode int
		wantErr  string
	}{
		{
			name:     "a complete cache resolves every line",
			manifest: "actions/checkout " + sha + " .\ngithub/codeql-action " + sha + " upload-sarif\n",
			build: func(t *testing.T, cache string) {
				writeActionArchive(t, filepath.Join(cache, "actions_checkout", sha+".tar.gz"), map[string]string{root + "/action.yml": "name: Checkout\n"})
				writeActionArchive(t, filepath.Join(cache, "github_codeql-action", sha+".tar.gz"), map[string]string{"codeql-action-" + sha + "/upload-sarif/action.yaml": "name: Upload\n"})
			},
			wantErr: "every one of the 2 baked action(s) resolves",
		},
		{
			name:     "a missing archive is named, not skipped",
			manifest: "actions/checkout " + sha + " .\n",
			build:    func(t *testing.T, cache string) {},
			wantCode: 1,
			wantErr:  "actions/checkout@" + sha + ": no baked archive",
		},
		{
			name:     "an empty archive is a gap too",
			manifest: "actions/checkout " + sha + " .\n",
			build: func(t *testing.T, cache string) {
				writeFiles(t, cache, map[string]string{filepath.Join("actions_checkout", sha+".tar.gz"): ""})
			},
			wantCode: 1,
			wantErr:  "no baked archive",
		},
		{
			name:     "two top-level directories are not what the runner extracts",
			manifest: "actions/checkout " + sha + " .\n",
			build: func(t *testing.T, cache string) {
				writeActionArchive(t, filepath.Join(cache, "actions_checkout", sha+".tar.gz"), map[string]string{
					root + "/action.yml": "name: Checkout\n",
					"stray/action.yml":   "name: Stray\n",
				})
			},
			wantCode: 1,
			wantErr:  "top-level directories",
		},
		{
			name:     "an archive without the action's manifest at that subpath",
			manifest: "github/codeql-action " + sha + " upload-sarif\n",
			build: func(t *testing.T, cache string) {
				writeActionArchive(t, filepath.Join(cache, "github_codeql-action", sha+".tar.gz"), map[string]string{"codeql-action-" + sha + "/init/action.yml": "name: Init\n"})
			},
			wantCode: 1,
			wantErr:  "carries no upload-sarif/action.yml",
		},
		{
			name:     "an empty manifest is a refusal, not a pass",
			manifest: "",
			build:    func(t *testing.T, cache string) {},
			wantCode: 1,
			wantErr:  "lists no action to verify",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cache := filepath.Join(dir, "cache")
			if err := os.MkdirAll(cache, 0o755); err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(dir, "baked-actions.txt")
			if err := os.WriteFile(manifest, []byte(tc.manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.build(t, cache)
			_, stderr, code := runScript(t, bakedVerifierScript, cache, manifest)
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d\nstderr: %s", code, tc.wantCode, stderr)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("stderr %q does not carry %q", stderr, tc.wantErr)
			}
		})
	}
}

// TestTheBakedActionCacheSitsOutsideTheWorkVolume holds the image to the one
// place a baked cache survives.
//
// ARC mounts a volume over /home/runner/_work, which hides everything the
// image put there — the trap this very image already paid for the tool cache
// (RUNNER_TOOL_CACHE moved to /opt/hostedtoolcache for exactly that reason).
// The runner reads the cache from an environment variable, so the escape is to
// set it outside _work; a cache baked under the volume would be invisible at
// run time while every check here still passed.
//
// Mutations seen red: the ENV path changed to /home/runner/_work/_actions-cache
// (this test fails naming the volume); the COPY destination moved out of the
// tool cache (the manifest the RUN reads is no longer the one shipped); the
// verifier call deleted from ci/arc-runner/verify.sh.
func TestTheBakedActionCacheSitsOutsideTheWorkVolume(t *testing.T) {
	dockerfile := mustRead(t, runnerDockerfilePath)
	cacheEnv := dockerArchiveCacheEnv.FindSubmatch(dockerfile)
	if cacheEnv == nil {
		t.Fatalf("%s has no `ENV %s=<path>` line — the image no longer bakes action archives where the runner reads them", runnerDockerfilePath, bakedCacheEnvName)
	}
	cache := string(cacheEnv[1])
	if strings.HasPrefix(cache, runnerWorkVolumePath) {
		t.Errorf("%s bakes the action archive cache at %s, under the volume ARC mounts over %s: everything baked there is hidden at run time, and every job goes back to codeload while this image looks correct (the same trap RUNNER_TOOL_CACHE pays by living in /opt)", runnerDockerfilePath, cache, runnerWorkVolumePath)
	}

	toolCache := dockerToolCacheEnv.FindSubmatch(dockerfile)
	if toolCache == nil {
		t.Fatalf("%s has no `ENV RUNNER_TOOL_CACHE=<path>` line; keep this guard in step", runnerDockerfilePath)
	}
	copied := dockerManifestCopy.FindSubmatch(dockerfile)
	if copied == nil {
		t.Fatalf("%s does not COPY baked-actions.txt — the image would bake from nothing", runnerDockerfilePath)
	}
	manifestInImage := string(copied[1])
	if !strings.HasPrefix(manifestInImage, string(toolCache[1])+"/") {
		t.Errorf("%s copies the manifest to %s and the RUN reads it from $RUNNER_TOOL_CACHE (%s): the two paths must be the same file, or the image bakes from a manifest nobody shipped", runnerDockerfilePath, manifestInImage, toolCache[1])
	}
	if !bytes.Contains(dockerfile, []byte(`done < "$RUNNER_TOOL_CACHE/`+filepath.Base(manifestInImage)+`"`)) {
		t.Errorf("%s does not read %s in its bake loop", runnerDockerfilePath, manifestInImage)
	}

	// The image's own verifier must run the offline check: without it nothing
	// proves the archives are usable, only that they were fetched.
	verifier := mustRead(t, runnerVerifierPath)
	for _, want := range []string{
		"/verify-baked-actions.sh:ro",
		"baked-actions.txt:/baked-actions.txt:ro",
		`bash /verify-baked-actions.sh "$ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE" /baked-actions.txt`,
		`cmp /baked-actions.txt "$RUNNER_TOOL_CACHE/` + filepath.Base(manifestInImage) + `"`,
	} {
		if !bytes.Contains(verifier, []byte(want)) {
			t.Errorf("%s does not carry %q — the baked actions would ship unverified", runnerVerifierPath, want)
		}
	}
	if !bytes.Contains(verifier, []byte("--network none")) {
		t.Errorf("%s no longer runs the image with --network none: the baked cache must be proved WITHOUT codeload, which is the whole point", runnerVerifierPath)
	}
}
