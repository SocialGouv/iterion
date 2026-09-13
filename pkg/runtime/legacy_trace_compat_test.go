package runtime_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestLegacyRuntimeTraceParity runs one ordinary .bot workflow through actual
// binaries built from pinned main and this checkout. It guards the legacy
// execution route while public contracts are introduced alongside it.
func TestLegacyRuntimeTraceParity(t *testing.T) {
	legacy := os.Getenv("ITERION_TEST_LEGACY_BINARY")
	current := os.Getenv("ITERION_TEST_CURRENT_BINARY")
	if legacy == "" || current == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("ITERION_TEST_LEGACY_BINARY and ITERION_TEST_CURRENT_BINARY are required")
		}
		t.Skip("set ITERION_TEST_LEGACY_BINARY and ITERION_TEST_CURRENT_BINARY to compare real executables")
	}
	for _, binary := range []string{legacy, current} {
		if _, err := os.Stat(binary); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile(filepath.Join("testdata", "legacy_trace.bot"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		items      string
		count      string
		branches   int
		iterations int
	}{
		{name: "two", items: `[{"id":"a"},{"id":"b"}]`, count: "2", branches: 2, iterations: 3},
		{name: "zero", items: `[]`, count: "0", branches: 0, iterations: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bot := bytes.Replace(source, []byte(`[{"id":"a"},{"id":"b"}]`), []byte(tc.items), 1)
			bot = bytes.Replace(bot, []byte(`count: "2"`), []byte(`count: "`+tc.count+`"`), 1)
			botPath := filepath.Join(t.TempDir(), "legacy_trace.bot")
			if err := os.WriteFile(botPath, bot, 0o600); err != nil {
				t.Fatal(err)
			}
			before := runLegacyTraceBinary(t, legacy, botPath)
			after := runLegacyTraceBinary(t, current, botPath)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("legacy execution changed\nmain:    %+v\ncurrent: %+v", before, after)
			}
			if before.Status != "finished" || before.CheckpointNode != "collect" || before.CollectCount != tc.count || before.Branches != tc.branches || before.Iterations != tc.iterations {
				t.Fatalf("unexpected reference behavior: %+v", before)
			}
			expectedStarted := map[string]int{"prepare": 1, "dispatch": 1, "collect": 1, "done": 1}
			if tc.branches > 0 {
				expectedStarted["handle"] = tc.branches
			}
			if !reflect.DeepEqual(before.Started, expectedStarted) {
				t.Fatalf("unexpected node admissions: %+v", before.Started)
			}
			if tc.branches == 2 && !reflect.DeepEqual(before.Handled, map[string]string{"branch_dispatch_0": "a", "branch_dispatch_1": "b"}) {
				t.Fatalf("unexpected mapped values: %+v", before.Handled)
			}
		})
	}
}

type legacyTraceSnapshot struct {
	Status         string
	CheckpointNode string
	CollectCount   string
	Iterations     int
	Branches       int
	Budget         map[string]int
	Started        map[string]int
	Finished       map[string]int
	Handled        map[string]string
	JoinReady      int
}

func runLegacyTraceBinary(t *testing.T, binary, botPath string) legacyTraceSnapshot {
	t.Helper()
	storeDir := filepath.Join(t.TempDir(), "store")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "run", botPath, "--run-id", "legacy_trace_parity", "--store-dir", storeDir, "--sandbox", "none", "--repo-devbox", "off", "--supervisors", "off", "--timeout", "30s", "--json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", binary, err, output)
	}
	runDir := filepath.Join(storeDir, "runs", "legacy_trace_parity")
	var run struct {
		Status     string         `json:"status"`
		Budget     map[string]int `json:"budget"`
		Checkpoint struct {
			NodeID         string                    `json:"node_id"`
			Outputs        map[string]map[string]any `json:"outputs"`
			IterationsUsed int                       `json:"budget_iterations_used"`
		} `json:"checkpoint"`
	}
	runJSON, err := os.ReadFile(filepath.Join(runDir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(runJSON, &run); err != nil {
		t.Fatal(err)
	}
	s := legacyTraceSnapshot{
		Status: run.Status, CheckpointNode: run.Checkpoint.NodeID,
		CollectCount: fmt.Sprint(run.Checkpoint.Outputs["collect"]["count"]),
		Iterations:   run.Checkpoint.IterationsUsed, Budget: run.Budget,
		Started: map[string]int{}, Finished: map[string]int{}, Handled: map[string]string{},
	}
	events, err := os.Open(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	scanner := bufio.NewScanner(events)
	for scanner.Scan() {
		var event struct {
			Type     string         `json:"type"`
			NodeID   string         `json:"node_id"`
			BranchID string         `json:"branch_id"`
			Data     map[string]any `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		switch event.Type {
		case "node_started":
			s.Started[event.NodeID]++
		case "node_finished":
			s.Finished[event.NodeID]++
			if event.NodeID == "handle" {
				output, ok := event.Data["output"].(map[string]any)
				if !ok {
					t.Fatalf("handle missing output: %+v", event)
				}
				s.Handled[event.BranchID] = fmt.Sprint(output["value"])
			}
		case "branch_started":
			s.Branches++
		case "join_ready":
			s.JoinReady++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if s.Branches > 0 && s.JoinReady != 1 {
		t.Fatalf("expected one wait_all join: %+v", s)
	}
	if strings.TrimSpace(s.Status) == "" {
		t.Fatalf("missing persisted status from %s", binary)
	}
	return s
}
