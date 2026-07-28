package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDepUpdateGuardPrepareClassifies guards the deterministic bump detector
// that scopes Vetty's whole run. It is the highest-consequence node in the
// bot: `prepare` decides WHAT the supply-chain auditor reads, and it fails
// silently in both directions.
//
//   - A dependency PR whose changed files match no known manifest pattern
//     still reaches the auditor — with an EMPTY bump_summary. The auditor
//     then renders a verdict on a diff nobody read: a façade "safe".
//   - A failed merge-base (shallow clone, base ref not fetched) yields no
//     changed files at all, which reads as "nothing to do" and ends the run
//     mute — no comment, and with a merge gate armed, a check pending forever.
//
// Both are what this test pins, against a real git tree.
func TestDepUpdateGuardPrepareClassifies(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	command := toolCommand(t, "dep-update-guard/main.bot", "prepare")

	type prepareResult struct {
		IsEmpty      bool     `json:"is_empty"`
		DepFiles     []string `json:"dep_files"`
		ChangedFiles int      `json:"changed_files"`
		BumpSummary  string   `json:"bump_summary"`
		NoManifest   bool     `json:"no_manifest"`
		Degraded     bool     `json:"degraded"`
	}

	run := func(t *testing.T, ws, base string) prepareResult {
		t.Helper()
		cmd := strings.ReplaceAll(command, "{{vars.workspace_dir}}", ws)
		cmd = strings.ReplaceAll(cmd, "{{vars.base_ref}}", base)
		out, err := exec.Command("sh", "-c", cmd).Output()
		if err != nil {
			t.Fatalf("prepare failed to execute: %v (out %q)", err, out)
		}
		var res prepareResult
		if uerr := json.Unmarshal(out, &res); uerr != nil {
			t.Fatalf("prepare output is not prepare_output JSON: %v (out %q)", uerr, out)
		}
		return res
	}

	git := func(t *testing.T, ws string, args ...string) {
		t.Helper()
		full := append([]string{"-C", ws}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}

	// bumpRepo builds a repo with a `main` baseline and a PR commit on top
	// that rewrites each named file — the shape of a Renovate PR.
	bumpRepo := func(t *testing.T, files map[string]string) string {
		t.Helper()
		ws := t.TempDir()
		git(t, ws, "init", "-q", "-b", "main")
		git(t, ws, "config", "user.email", "t@example.com")
		git(t, ws, "config", "user.name", "t")
		if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("base\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "-A")
		git(t, ws, "commit", "-qm", "base")

		for name, body := range files {
			p := filepath.Join(ws, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		git(t, ws, "checkout", "-q", "-b", "renovate/bump")
		git(t, ws, "add", "-A")
		git(t, ws, "commit", "-qm", "chore(deps): bump", "--allow-empty")
		return ws
	}

	// Every one of these is a real Renovate manager on a real repo. The
	// Docker/devbox/Taskfile ones were invisible to the classifier, so those
	// PRs reached the auditor with nothing to audit.
	for _, tc := range []struct {
		name string
		file string
		body string
	}{
		{"go modules", "go.mod", "module x\n\ngo 1.26\n\nrequire k8s.io/api v0.34.1\n"},
		{"npm lockfile", "pnpm-lock.yaml", "lockfileVersion: 9\n"},
		{"dockerfile digest pin", "cmd/buildd/Dockerfile", "FROM golang:1.26@sha256:3aff665\n"},
		{"containerfile", "Containerfile", "FROM alpine@sha256:abc\n"},
		{"devbox toolchain", "devbox.json", "{\"packages\": [\"jq@1.8.2\"]}\n"},
		{"devbox lock", "devbox.lock", "{\"lockfile_version\": \"1\"}\n"},
		{"task runner pins", "Taskfile.yml", "# renovate: datasource=go depName=x\ntasks:\n  lint:\n    cmds: [go run x@v2.12.2]\n"},
		{"asdf tool versions", ".tool-versions", "golang 1.26.4\n"},
		{"nix flake lock", "flake.lock", "{\"nodes\": {}}\n"},
		{"composite action pin", "action.yml", "runs:\n  using: node24\n"},
		{"helm chart", "deploy/helm/app/Chart.yaml", "apiVersion: v2\nversion: 1.2.3\n"},
		{"helm values image tag", "deploy/helm/app/values.yaml", "image:\n  tag: v1.2.3\n"},
		{"workflow action digest", ".github/workflows/ci.yml", "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@abc123\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := bumpRepo(t, map[string]string{tc.file: tc.body})
			res := run(t, ws, "main")

			if res.IsEmpty {
				t.Fatalf("%s: reported is_empty on a real bump", tc.file)
			}
			if len(res.DepFiles) != 1 || res.DepFiles[0] != tc.file {
				t.Errorf("dep_files = %v, want [%s]", res.DepFiles, tc.file)
			}
			// The auditor's whole input. Empty here is the façade-verdict bug.
			if !strings.Contains(res.BumpSummary, tc.file) {
				t.Errorf("bump_summary must carry the %s diff, got %q", tc.file, res.BumpSummary)
			}
		})
	}

	// A PR touching files no pattern recognises must still hand the auditor
	// the real diff — widened to the FULL change set — and say so, rather
	// than pass an empty string off as "audited".
	t.Run("unrecognised manifest widens to the full diff", func(t *testing.T) {
		ws := bumpRepo(t, map[string]string{"vendor.conf": "pkg 1.2.3\n"})
		res := run(t, ws, "main")

		if res.IsEmpty {
			t.Fatal("reported is_empty on a real change")
		}
		if !res.NoManifest {
			t.Error("no_manifest must flag that nothing matched, so the audit can widen its scope")
		}
		if !strings.Contains(res.BumpSummary, "vendor.conf") {
			t.Errorf("bump_summary must fall back to the full diff, got %q", res.BumpSummary)
		}
	})

	// A merge-base that cannot be resolved must degrade LOUDLY. Reporting
	// "no diff" here is what makes the run end mute.
	t.Run("unresolvable base degrades loudly", func(t *testing.T) {
		ws := bumpRepo(t, map[string]string{"go.mod": "module x\n"})
		res := run(t, ws, "origin/does-not-exist")

		if res.IsEmpty {
			t.Error("an unresolvable base must not read as an empty diff — that ends the run silently")
		}
		if !res.Degraded {
			t.Error("an unresolvable base must be reported as degraded")
		}
	})

	// A PR with genuinely no diff is the one case that may end the run early.
	t.Run("truly empty diff", func(t *testing.T) {
		ws := bumpRepo(t, nil)
		if res := run(t, ws, "main"); !res.IsEmpty {
			t.Errorf("an empty PR must report is_empty, got %+v", res)
		}
	})
}
