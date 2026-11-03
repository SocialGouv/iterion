package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestDepUpdateGuardCommitProbeReadsGit(t *testing.T) {
	script := toolScript(t, "dep-update-guard/main.bot", "commit_probe")
	type result struct {
		SHA    string `json:"sha"`
		Moved  bool   `json:"moved"`
		Reason string `json:"reason"`
	}
	read := func(t *testing.T, ws, prepared, reported string, extra ...string) result {
		t.Helper()
		body := script
		for ref, value := range map[string]string{"{{vars.workspace_dir}}": ws, "{{input.head_sha}}": prepared, "{{input.sha}}": reported} {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			body = strings.ReplaceAll(body, ref, string(encoded))
		}
		if strings.Contains(body, "{{") {
			t.Fatal("unsubstituted commit probe input")
		}
		path := filepath.Join(t.TempDir(), "probe.py")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("python3", "-I", path)
		cmd.Env = append(gittest.Env(), extra...)
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("probe: %v: %s", err, raw)
		}
		var got result
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("probe JSON: %v: %s", err, raw)
		}
		if got.Reason == "" {
			t.Fatal("probe omitted its reason")
		}
		return got
	}
	ws := t.TempDir()
	ancestor := gittest.InitRepo(t, ws)
	gittest.Run(t, ws, "commit", "--allow-empty", "-m", "dependency bump")
	prepared := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
	for _, reported := range []string{"", "none", "HEAD", "--all", strings.Repeat("f", 40), prepared, prepared[:7], prepared[:12], strings.ToUpper(prepared), prepared + "\n", ancestor} {
		t.Run("unmoved/"+reported, func(t *testing.T) {
			if got := read(t, ws, prepared, reported); got.Moved || got.SHA != "" {
				t.Fatalf("an unmoved head was certified as a new commit: %+v", got)
			}
		})
	}
	gittest.Run(t, ws, "commit", "--allow-empty", "-m", "alignment")
	aligned := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
	for _, reported := range []string{aligned, aligned[:7], aligned[:12], strings.ToUpper(aligned), aligned + "\n"} {
		t.Run("new-head/"+reported, func(t *testing.T) {
			if got := read(t, ws, prepared, reported); !got.Moved || got.SHA != aligned {
				t.Fatalf("git's new head was not resolved to its full identity: %+v", got)
			}
		})
	}
	foreign := t.TempDir()
	foreignHead := gittest.InitRepo(t, foreign)
	t.Run("ambient repository redirection is ignored", func(t *testing.T) {
		got := read(t, ws, prepared, aligned, "GIT_DIR="+filepath.Join(foreign, ".git"), "GIT_WORK_TREE="+foreign, "GIT_OBJECT_DIRECTORY="+filepath.Join(foreign, ".git", "objects"))
		if !got.Moved || got.SHA != aligned {
			t.Fatalf("ambient git variables redirected the proof: %+v", got)
		}
	})
	t.Run("foreign report is not evidence", func(t *testing.T) {
		if got := read(t, ws, prepared, foreignHead); got.Moved || got.SHA != "" {
			t.Fatalf("a foreign commit was accepted: %+v", got)
		}
	})
	gittest.Run(t, ws, "commit", "--allow-empty", "-m", "later commit")
	t.Run("a new but non-tip commit is not the published revision", func(t *testing.T) {
		if got := read(t, ws, prepared, aligned); got.Moved || got.SHA != "" {
			t.Fatalf("an intermediate commit was accepted as the tip: %+v", got)
		}
	})
	gittest.Run(t, ws, "switch", "--orphan", "unrelated")
	gittest.Run(t, ws, "commit", "--allow-empty", "-m", "unrelated root")
	unrelated := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
	t.Run("an unrelated head is not this run's alignment", func(t *testing.T) {
		if got := read(t, ws, prepared, unrelated); got.Moved || got.SHA != "" {
			t.Fatalf("an unrelated head was certified: %+v", got)
		}
	})
	t.Run("an unreadable workspace proves nothing", func(t *testing.T) {
		if got := read(t, t.TempDir(), prepared, aligned); got.Moved || got.SHA != "" {
			t.Fatalf("an unreadable repository certified a commit: %+v", got)
		}
	})
}

func TestDepUpdateGuardCommitEvidenceReachesEveryConsumer(t *testing.T) {
	wf := ir.Compile(parseBotUnit("dep-update-guard/main.bot").File).Workflow
	if wf == nil {
		t.Fatal("workflow did not compile")
	}
	if _, ok := wf.Nodes["commit_probe"].(*ir.ToolNode); !ok {
		t.Fatal("commit evidence must be a deterministic tool")
	}
	var commitEdges, probeEdges int
	for _, edge := range wf.Edges {
		mappings := map[string]string{}
		for _, m := range edge.With {
			mappings[m.Key] = strings.Trim(m.Raw, `"`)
		}
		if edge.From == "commit" {
			commitEdges++
			if edge.To != "commit_probe" || mappings["sha"] != "{{outputs.commit.sha}}" || mappings["head_sha"] != "{{outputs.prepare.head_sha}}" {
				t.Fatalf("commit bypasses or misfeeds its proof: %+v", edge)
			}
		}
		if edge.From == "commit_probe" {
			probeEdges++
			if edge.To != "commit_check" {
				t.Fatalf("proof bypasses commit verdict: %+v", edge)
			}
		}
		if edge.From == "feedback_health" && edge.To == "arm_automerge" && mappings["committed_sha"] != "{{outputs.commit_probe.sha}}" {
			t.Fatal("arming reads an unverified commit report")
		}
	}
	if commitEdges != 1 || probeEdges != 1 {
		t.Fatalf("commit/probe edges = %d/%d", commitEdges, probeEdges)
	}
}
