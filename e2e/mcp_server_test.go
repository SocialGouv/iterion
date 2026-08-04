package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// mcpProc drives one `iterion mcp` stdio server subprocess with
// line-delimited JSON-RPC calls.
type mcpProc struct {
	t    *testing.T
	cmd  *exec.Cmd
	in   *json.Encoder
	out  *bufio.Scanner
	stop func()
}

func startMCPProc(t *testing.T, binPath, workDir, storeDir string) *mcpProc {
	t.Helper()
	cmd := exec.Command(binPath, "mcp", "--store-dir", storeDir)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		// Deterministic: never try to sandbox the spawned runner in CI.
		"ITERION_SANDBOX_DEFAULT=none",
		// Make the detached-runner binary resolution unambiguous.
		"ITERION_BIN="+binPath,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start iterion mcp: %v", err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	p := &mcpProc{t: t, cmd: cmd, in: json.NewEncoder(stdin), out: sc}
	p.stop = func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}
	return p
}

// call sends one JSON-RPC request and decodes the next response line.
func (p *mcpProc) call(method string, params any) map[string]any {
	p.t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := p.in.Encode(req); err != nil {
		p.t.Fatalf("send %s: %v", method, err)
	}
	if !p.out.Scan() {
		p.t.Fatalf("no response to %s (scanner err: %v)", method, p.out.Err())
	}
	var resp map[string]any
	if err := json.Unmarshal(p.out.Bytes(), &resp); err != nil {
		p.t.Fatalf("decode response to %s: %v\n%s", method, err, p.out.Text())
	}
	return resp
}

// toolText extracts the text payload of a tools/call response, failing
// the test on protocol- or tool-level errors.
func toolText(t *testing.T, resp map[string]any) string {
	t.Helper()
	if errObj, ok := resp["error"]; ok {
		t.Fatalf("JSON-RPC error: %v", errObj)
	}
	result, _ := resp["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("tool error: %+v", result)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("empty content: %+v", result)
	}
	return content[0].(map[string]any)["text"].(string)
}

// TestMCPServer_DetachedRunSurvivesServerExit exercises the operator
// MCP end to end with the real binary: launch a tool-only workflow via
// local_run, kill the MCP server immediately (the detached runner must
// survive it — that is the whole point of the detached design), wait
// for the run to finish in the store, then read it back through a
// SECOND MCP server (local_run_get + local_run_report).
func TestMCPServer_DetachedRunSurvivesServerExit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary-spawning e2e in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("detached runs are unix-only")
	}

	// Build the real binary, named `iterion` so the runner-binary
	// sibling resolution finds it.
	binPath := filepath.Join(t.TempDir(), "iterion")
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/iterion")
	buildCmd.Dir = ".."
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build iterion: %v\n%s", err, out)
	}

	workDir := t.TempDir()
	storeDir := filepath.Join(workDir, ".iterion")
	botPath := filepath.Join(workDir, "probe.bot")
	bot := "tool probe:\n" +
		"  command: \"echo mcp-e2e-probe\"\n\n" +
		"workflow main:\n" +
		"  entry: probe\n" +
		"  probe -> done\n"
	if err := os.WriteFile(botPath, []byte(bot), 0o644); err != nil {
		t.Fatal(err)
	}

	// --- Server #1: initialize + launch, then exit immediately. ---
	p1 := startMCPProc(t, binPath, workDir, storeDir)
	initResp := p1.call("initialize", nil)
	srvInfo := initResp["result"].(map[string]any)["serverInfo"].(map[string]any)
	if srvInfo["name"] != "iterion" {
		t.Fatalf("serverInfo.name=%v", srvInfo["name"])
	}
	launchText := toolText(t, p1.call("tools/call", map[string]any{
		"name":      "local_run",
		"arguments": map[string]any{"file_path": botPath},
	}))
	var launch struct {
		RunID    string `json:"run_id"`
		Detached bool   `json:"detached"`
	}
	if err := json.Unmarshal([]byte(launchText), &launch); err != nil {
		t.Fatalf("decode launch result: %v\n%s", err, launchText)
	}
	if launch.RunID == "" || !launch.Detached {
		t.Fatalf("unexpected launch result: %s", launchText)
	}
	// Kill the MCP server right away — the run must keep going.
	p1.stop()

	// --- The detached runner finishes on its own. ---
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	deadline := time.Now().Add(90 * time.Second)
	var lastStatus store.RunStatus
	for {
		r, lerr := st.LoadRun(context.Background(), launch.RunID)
		if lerr == nil {
			lastStatus = r.Status
			if r.Status == store.RunStatusFinished {
				break
			}
			if r.Status.IsTerminal() {
				t.Fatalf("run ended %s (error: %s)", r.Status, r.Error)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish in time (last status: %s)", launch.RunID, lastStatus)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// The --background runner removes its .pid on exit; give the defer
	// a moment past the final status write.
	if pidS := store.AsPIDStore(st); pidS != nil {
		pidDeadline := time.Now().Add(10 * time.Second)
		for {
			// ReadPIDFile reports a missing file as (0, nil).
			if pid, err := pidS.ReadPIDFile(launch.RunID); err == nil && pid == 0 {
				break // gone — clean detached shutdown
			}
			if time.Now().After(pidDeadline) {
				t.Fatalf("runner .pid file still present after completion")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	// --- Server #2: read the finished run back over MCP. ---
	p2 := startMCPProc(t, binPath, workDir, storeDir)
	defer p2.stop()
	getText := toolText(t, p2.call("tools/call", map[string]any{
		"name":      "local_run_get",
		"arguments": map[string]any{"run_id": launch.RunID},
	}))
	var view struct {
		Status   string `json:"status"`
		Workflow string `json:"workflow_name"`
	}
	if err := json.Unmarshal([]byte(getText), &view); err != nil {
		t.Fatalf("decode run view: %v\n%s", err, getText)
	}
	if view.Status != "finished" || view.Workflow != "main" {
		t.Fatalf("unexpected run view: %s", getText)
	}
	reportText := toolText(t, p2.call("tools/call", map[string]any{
		"name":      "local_run_report",
		"arguments": map[string]any{"run_id": launch.RunID},
	}))
	if !strings.Contains(reportText, "probe") {
		t.Fatalf("report should mention the probe node:\n%s", reportText)
	}
	// The event stream is reachable over MCP too.
	eventsText := toolText(t, p2.call("tools/call", map[string]any{
		"name":      "local_run_events",
		"arguments": map[string]any{"run_id": launch.RunID},
	}))
	if !strings.Contains(eventsText, "run_finished") {
		t.Fatalf("events should contain run_finished:\n%s", fmt.Sprintf("%.2000s", eventsText))
	}
}
