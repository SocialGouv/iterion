package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

func boolp(b bool) *bool { return &b }

// The changelog compares entries by VALUE. Spec carries the three capability
// flags as POINTERS, so a struct `!=` compares ADDRESSES: two entries both
// stating `tool_call:false` would read as a flip, every regeneration would
// report the whole table as changed, and the "!! N% changed — verify this
// regeneration is intended" alarm would fire every time and mean nothing.
func TestSpecsEqual_ComparesFlagsByValueNotByAddress(t *testing.T) {
	spec := func(window int, reasoning, tool *bool) modelspecs.Spec {
		return modelspecs.Spec{ContextWindow: window, InputCostPerM: 1, Reasoning: reasoning, ToolCall: tool}
	}
	same := spec(4096, boolp(true), boolp(false))
	if !specsEqual(same, spec(4096, boolp(true), boolp(false))) {
		t.Error("two entries stating the same flags read as a change — the comparison is on addresses")
	}
	if specsEqual(same, spec(4096, boolp(true), boolp(true))) {
		t.Error("a tool_call flip did not read as a change")
	}
	// Unstated is not false: a source that stopped publishing a flag is a
	// change a reviewer must see, and so is one that started.
	if specsEqual(same, spec(4096, boolp(true), nil)) {
		t.Error("a flag that stopped being published did not read as a change")
	}
	if specsEqual(spec(4096, nil, nil), spec(4096, nil, boolp(false))) {
		t.Error("a flag that started being published did not read as a change")
	}
	if specsEqual(spec(4096, nil, nil), spec(4096, nil, nil)) == false {
		t.Error("two entries stating no flag read as a change")
	}
	// The numbers still decide too.
	if specsEqual(same, spec(8192, boolp(true), boolp(false))) {
		t.Error("a window change did not read as a change")
	}
}

// The `~` line of the changelog names the flags, in the order the snapshot
// and the operator table name them, so a flip is readable without diffing
// 1.6 MB of JSON.
func TestFlagWords_NamesTheThreeVerdictsInOrder(t *testing.T) {
	got := flagWords(modelspecs.Spec{Reasoning: boolp(true), ToolCall: boolp(false)})
	if got != "yes/no/?" {
		t.Errorf("flagWords = %q, want %q (reasoning/tool_call/temperature)", got, "yes/no/?")
	}
}

// printChangelog is the SITE: a comparison that only lived in specsEqual
// would leave the caller free to go back to a struct `!=`. Driven through
// the real printer, on two tables that differ in NOTHING BUT a flag.
func TestPrintChangelog_NamesAFlagFlipAndNothingElse(t *testing.T) {
	before := map[string]modelspecs.Spec{
		"prov/flip": {ContextWindow: 4096, InputCostPerM: 1, OutputCostPerM: 2, ToolCall: boolp(true)},
		"prov/same": {ContextWindow: 4096, InputCostPerM: 1, OutputCostPerM: 2, ToolCall: boolp(false)},
	}
	after := map[string]modelspecs.Spec{
		"prov/flip": {ContextWindow: 4096, InputCostPerM: 1, OutputCostPerM: 2, ToolCall: boolp(false)},
		"prov/same": {ContextWindow: 4096, InputCostPerM: 1, OutputCostPerM: 2, ToolCall: boolp(false)},
	}
	out := captureStdout(t, func() { printChangelog(before, after) })
	if !strings.Contains(out, "changelog: +0 -0 ~1 (total 2)") {
		t.Errorf("the counts read %q, want one changed entry of two", firstLine(out))
	}
	if !strings.Contains(out, "~ prov/flip:") || !strings.Contains(out, "flags ?/yes/?→?/no/?") {
		t.Errorf("the flip is not named in the changelog:\n%s", out)
	}
	if strings.Contains(out, "~ prov/same") {
		t.Errorf("an unchanged entry was reported as changed:\n%s", out)
	}
}

// captureStdout runs fn with os.Stdout on a pipe and returns what it wrote.
// printChangelog prints with fmt.Printf — the changelog IS stdout, read by
// the operator regenerating the snapshot — so the pipe is the only seam.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = saved
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
	return <-done
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// readCurrent is BOTH sides of the changelog's diff — the snapshot on disk
// before the write, and the file re-read after it. A field it drops is a
// field no flip can ever be reported for, in either direction. Driven on a
// real file, in a scratch directory the framework owns.
func TestReadCurrent_KeepsTheFlagsTheFileCarries(t *testing.T) {
	body := []byte(`{"prov":{"models":{"m":{"limit":{"context":4096},"cost":{"input":1,"output":2},"tool_call":false,"reasoning":true}}}}`)
	rendered, err := modelspecs.WriteSnapshot(body, "sha256:test", time.Now().UTC())
	if err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dst, rendered, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	spec, ok := readCurrent()["prov/m"]
	if !ok {
		t.Fatalf("readCurrent lost the entry the file carries")
	}
	if spec.ToolCall == nil || *spec.ToolCall {
		t.Errorf("readCurrent read tool_call %s, want a stated false", flagWord(spec.ToolCall))
	}
	if spec.Reasoning == nil || !*spec.Reasoning {
		t.Errorf("readCurrent read reasoning %s, want a stated true", flagWord(spec.Reasoning))
	}
	if spec.Temperature != nil {
		t.Errorf("readCurrent invented temperature %s for a row that states none", flagWord(spec.Temperature))
	}
	if spec.ContextWindow != 4096 {
		t.Errorf("readCurrent read window %d, want 4096", spec.ContextWindow)
	}
	// No snapshot yet is an empty table, not a crash: that is the first
	// generation, and the changelog reports every entry as added.
	t.Chdir(t.TempDir())
	if n := len(readCurrent()); n != 0 {
		t.Errorf("a missing snapshot read %d entries, want an empty table", n)
	}
}
