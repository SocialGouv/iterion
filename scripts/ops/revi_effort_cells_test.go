// Package ops holds operator scripts; this test drives revi-effort-cells.py
// against a stand-in `iterion` CLI that serves an instance from a state file.
package ops

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeIterion stands in for `iterion remote api`: it serves the orgs, teams
// and forge integrations of $FAKE_STATE, applies a PATCH's routing block to
// it (or answers a parked approval when the state says so), and appends each
// call to $FAKE_LOG.
const fakeIterion = `#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
assert args[:2] == ["remote", "api"], args
method, path = args[2], args[3]
body = None
if "--data" in args:
    ref = args[args.index("--data") + 1]
    body = json.load(open(ref[1:]))
state_path = os.environ["FAKE_STATE"]
state = json.load(open(state_path))
with open(os.environ["FAKE_LOG"], "a") as log:
    log.write(json.dumps({"method": method, "path": path, "body": body}) + "\n")
parts = path.strip("/").split("/")
if method == "GET" and path == "/api/admin/orgs":
    print(json.dumps([{"id": "o1", "name": "SDPC"}]))
elif method == "GET" and parts[:3] == ["api", "admin", "orgs"]:
    print(json.dumps([{"id": t["id"], "name": t["name"]} for t in state["teams"]]))
elif method == "GET" and parts[:2] == ["api", "teams"]:
    team = next(t for t in state["teams"] if t["id"] == parts[2])
    print(json.dumps({"integrations": team["integrations"]}))
elif method == "PATCH" and parts[:2] == ["api", "teams"]:
    if state.get("park"):
        print(json.dumps({"pending_approval": True}))
        sys.exit(0)
    team = next(t for t in state["teams"] if t["id"] == parts[2])
    integ = next(i for i in team["integrations"] if i["id"] == parts[5])
    integ["routing"] = body["routing"]
    json.dump(state, open(state_path, "w"))
    print(json.dumps({"id": integ["id"]}))
else:
    print("unexpected call", file=sys.stderr)
    sys.exit(1)
`

const gatewayModel = "openai_compatible/scaleway/deepseek-v4.1-flash"

// gatewayBlock is the activation block as production carried it on
// 2026-11-15: no effort on any cell.
func gatewayBlock() map[string]any {
	var cells []any
	for _, sel := range []string{
		"review-pr/converge", "review-pr/converge_gpt", "review-pr/reviewer_claude", "review-pr/reviewer_claude_glance",
		"review-pr/reviewer_gpt", "review-pr/reviewer_gpt_glance", "review-pr/converge#glm", "review-pr/reviewer_claude#glm",
		"review-pr/reviewer_claude_glance#glm", "revi-converse/converse_agent", "revi-converse/converse_agent#glm",
	} {
		cells = append(cells, map[string]any{"selector": sel, "backend": "claw", "provider": "openai_compatible", "model": gatewayModel})
	}
	cells = append(cells,
		map[string]any{"selector": "review-pr/supervisor:pacer", "model": gatewayModel},
		map[string]any{"selector": "review-pr/supervisor:pacer_gpt", "model": gatewayModel})
	return map[string]any{"overrides": cells}
}

type instance struct {
	t     *testing.T
	dir   string
	state string
	log   string
}

func newInstance(t *testing.T) *instance {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "iterion"), []byte(fakeIterion), 0o755); err != nil {
		t.Fatal(err)
	}
	partial := gatewayBlock()
	partial["overrides"] = partial["overrides"].([]any)[2:] // no converge cells
	in := &instance{t: t, dir: dir, state: filepath.Join(dir, "state.json"), log: filepath.Join(dir, "calls.log")}
	in.write(map[string]any{"teams": []any{
		map[string]any{"id": "t-domifa", "name": "DomiFa", "integrations": []any{
			map[string]any{"id": "i-domifa", "repo_full_name": "SocialGouv/domifa", "routing": gatewayBlock()},
		}},
		map[string]any{"id": "t-vao", "name": "VAO", "integrations": []any{
			map[string]any{"id": "i-vao", "repo_full_name": "SocialGouv/vao", "routing": gatewayBlock()},
		}},
		map[string]any{"id": "t-iterion", "name": "Iterion", "integrations": []any{
			map[string]any{"id": "i-iterion", "repo_full_name": "SocialGouv/iterion"},
			map[string]any{"id": "i-partial", "repo_full_name": "SocialGouv/partial", "routing": partial},
		}},
	}})
	return in
}

func (in *instance) write(state map[string]any) {
	raw, err := json.Marshal(state)
	if err != nil {
		in.t.Fatal(err)
	}
	if err := os.WriteFile(in.state, raw, 0o644); err != nil {
		in.t.Fatal(err)
	}
}

func (in *instance) read() map[string]any {
	raw, err := os.ReadFile(in.state)
	if err != nil {
		in.t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		in.t.Fatal(err)
	}
	return state
}

// routing is the stored block of one integration.
func (in *instance) routing(id string) map[string]any {
	for _, team := range in.read()["teams"].([]any) {
		for _, integ := range team.(map[string]any)["integrations"].([]any) {
			if m := integ.(map[string]any); m["id"] == id {
				r, _ := m["routing"].(map[string]any)
				return r
			}
		}
	}
	in.t.Fatalf("no integration %s", id)
	return nil
}

func (in *instance) patches() int {
	raw, err := os.ReadFile(in.log)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		in.t.Fatal(err)
	}
	return strings.Count(string(raw), `"method": "PATCH"`)
}

// run executes the script; it answers its combined output and exit code.
func (in *instance) run(args ...string) (string, int) {
	in.t.Helper()
	cmd := exec.Command("python3", append([]string{"-I", "revi-effort-cells.py"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(in.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_STATE="+in.state, "FAKE_LOG="+in.log)
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		in.t.Fatalf("run %v: %v", args, err)
	}
	return string(out), code
}

// efforts maps each cell's selector to its effort ("" when none).
func efforts(block map[string]any) map[string]string {
	out := map[string]string{}
	for _, c := range block["overrides"].([]any) {
		m := c.(map[string]any)
		e, _ := m["effort"].(string)
		out[m["selector"].(string)] = e
	}
	return out
}

// TestReviEffortCells_BackupCanaryApplyRestore walks the operator procedure
// on a stand-in instance: plan names the targets; apply refuses an
// integration with no backup; the canary gets the fixture's efforts on its
// node cells and nothing else changes; a second apply writes nothing; a
// parked approval is not reported applied; restore puts the backed-up block
// back.
//
// Mutation that reddens it: drop the read_backup call from cmd_apply, or the
// `if not changes` early exit.
func TestReviEffortCells_BackupCanaryApplyRestore(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	in := newInstance(t)
	backups := filepath.Join(in.dir, "backup")

	out, code := in.run("plan")
	for _, want := range []string{"SocialGouv/domifa", "SocialGouv/vao", "CHANGE ", "SKIP partial", "left alone", "2 target(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output lacks %q:\n%s", want, out)
		}
	}
	if code != 0 || in.patches() != 0 {
		t.Fatalf("plan exited %d after %d PATCH(es):\n%s", code, in.patches(), out)
	}

	if out, code := in.run("apply", "--dir", backups, "--only", "SocialGouv/domifa", "--yes"); code != 2 || in.patches() != 0 {
		t.Fatalf("apply with no backup exited %d after %d PATCH(es), want a refusal before any write:\n%s", code, in.patches(), out)
	}
	if out, code := in.run("backup", "--dir", backups); code != 0 {
		t.Fatalf("backup exited %d:\n%s", code, out)
	}

	before := in.routing("i-domifa")
	if out, code := in.run("apply", "--dir", backups, "--only", "SocialGouv/domifa", "--yes"); code != 0 || !strings.Contains(out, "applied  SocialGouv/domifa") {
		t.Fatalf("canary apply exited %d:\n%s", code, out)
	}
	got := efforts(in.routing("i-domifa"))
	want := map[string]string{
		"review-pr/converge": "low", "review-pr/converge_gpt": "low", "review-pr/reviewer_claude": "high",
		"review-pr/reviewer_claude_glance": "low", "review-pr/reviewer_gpt": "high", "review-pr/reviewer_gpt_glance": "low",
		"revi-converse/converse_agent": "high",
	}
	for sel, e := range got {
		if e != want[sel] {
			t.Errorf("after apply, %s carries effort %q, want %q", sel, e, want[sel])
		}
	}
	if len(got) != len(efforts(before)) {
		t.Errorf("apply changed the cell count: %d -> %d", len(efforts(before)), len(got))
	}
	if e := efforts(in.routing("i-vao"))["review-pr/reviewer_claude"]; e != "" {
		t.Errorf("the canary run touched VAO: reviewer_claude effort %q", e)
	}

	n := in.patches()
	if out, code := in.run("apply", "--dir", backups, "--only", "i-domifa", "--yes"); code != 0 || in.patches() != n || !strings.Contains(out, "already applied") {
		t.Fatalf("a second apply exited %d after %d new PATCH(es), want none:\n%s", code, in.patches()-n, out)
	}

	state := in.read()
	state["park"] = true
	in.write(state)
	if out, code := in.run("apply", "--dir", backups, "--only", "SocialGouv/vao", "--yes"); code != 1 || !strings.Contains(out, "PARKED") {
		t.Fatalf("a parked apply exited %d, want 1 and PARKED:\n%s", code, out)
	}
	state = in.read()
	state["park"] = false
	in.write(state)

	if out, code := in.run("restore", "--dir", backups, "--only", "SocialGouv/domifa", "--yes"); code != 0 || !strings.Contains(out, "restored SocialGouv/domifa") {
		t.Fatalf("restore exited %d:\n%s", code, out)
	}
	for sel, e := range efforts(in.routing("i-domifa")) {
		if e != "" {
			t.Errorf("after restore, %s still carries effort %q", sel, e)
		}
	}
}
