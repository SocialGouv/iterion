package operatormcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const nativeWriteSource = `dsl: 2
contract Result:
  display_name: "Deliver result"
  responsibility: "Produce the public result"
  outputs:
    value: string
compute internal:
  expr:
    value: "\"private technical expression\""
workflow main:
  runtime_semantics: "ports-v1"
  contract: Result
  graph:
    nodes:
      deliver:
        implementation: internal
        contract: Result
    exports:
      value: deliver.value
    products: ["value"]
`

func contractWriteArgs(path, source, expected string) string {
	raw, _ := json.Marshal(map[string]string{"file_path": path, "source": source, "expected_sha256": expected})
	return string(raw)
}

func TestLocalContractWriteValidatesAndProtectsExistingSource(t *testing.T) {
	s := newTestServer(t)
	path := filepath.Join(s.WorkDir, "native.bot")
	create := contractWriteArgs("native.bot", nativeWriteSource, "absent")
	out, isErr := call(t, s, "local_contract_write", create)
	if isErr {
		t.Fatalf("create: %s", out)
	}
	var result struct {
		Written    bool   `json:"written"`
		SHA256     string `json:"sha256"`
		PublicView struct {
			GraphIdentity string   `json:"graph_identity"`
			Products      []string `json:"products"`
		} `json:"public_view"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Written || result.PublicView.GraphIdentity == "" || len(result.PublicView.Products) != 1 {
		t.Fatalf("missing public write result: %s", out)
	}
	sum := sha256.Sum256([]byte(nativeWriteSource))
	if result.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("wrong source digest: %s", out)
	}
	if strings.Contains(out, "private technical expression") {
		t.Fatalf("public response exposed technical expression: %s", out)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != nativeWriteSource {
		t.Fatalf("technical source was not preserved: %v %q", err, actual)
	}
	broken := strings.Replace(nativeWriteSource, "value: deliver.value", "value: missing.value", 1)
	if out, isErr := call(t, s, "local_contract_write", contractWriteArgs("native.bot", broken, result.SHA256)); !isErr || !strings.Contains(out, "invalid contract source") {
		t.Fatalf("invalid native update was accepted: %s", out)
	}
	if actual, err = os.ReadFile(path); err != nil || string(actual) != nativeWriteSource {
		t.Fatalf("invalid update changed the source: %v %q", err, actual)
	}
	public, isErr := call(t, s, "local_contract_read", `{"file_path":"native.bot"}`)
	if isErr || !strings.Contains(public, result.SHA256) || strings.Contains(public, "private technical expression") {
		t.Fatalf("public read did not return digest with technical source hidden: %s", public)
	}
	technical, isErr := call(t, s, "local_contract_read", `{"file_path":"native.bot","include_source":true}`)
	if isErr || !strings.Contains(technical, "private technical expression") {
		t.Fatalf("explicit technical read did not return source: %s", technical)
	}
	if out, isErr = call(t, s, "local_contract_write", create); !isErr || !strings.Contains(out, "changed") {
		t.Fatalf("create overwrote existing file: %s", out)
	}
	if out, isErr = call(t, s, "local_contract_write", contractWriteArgs("native.bot", nativeWriteSource+"# revision\n", strings.Repeat("0", 64))); !isErr || !strings.Contains(out, "stale") {
		t.Fatalf("stale write was accepted: %s", out)
	}
	updated := nativeWriteSource + "# revision\n"
	if out, isErr = call(t, s, "local_contract_write", contractWriteArgs("native.bot", updated, result.SHA256)); isErr {
		t.Fatalf("update: %s", out)
	}
	actual, err = os.ReadFile(path)
	if err != nil || string(actual) != updated {
		t.Fatalf("updated source differs: %v %q", err, actual)
	}
}

func TestLocalContractWriteRejectsInvalidAndEscapingSources(t *testing.T) {
	s := newTestServer(t)
	if out, isErr := call(t, s, "local_contract_write", contractWriteArgs("invalid.bot", minimalBot, "absent")); !isErr || !strings.Contains(out, "ports-v1") {
		t.Fatalf("legacy source was accepted: %s", out)
	}
	if _, err := os.Stat(filepath.Join(s.WorkDir, "invalid.bot")); !os.IsNotExist(err) {
		t.Fatalf("invalid source was written: %v", err)
	}
	if out, isErr := call(t, s, "local_contract_write", contractWriteArgs("../outside.bot", nativeWriteSource, "absent")); !isErr || !strings.Contains(out, "inside") {
		t.Fatalf("escaping path was accepted: %s", out)
	}
	if out, isErr := call(t, s, "local_contract_read", `{"file_path":"../outside.bot"}`); !isErr || !strings.Contains(out, "inside") {
		t.Fatalf("escaping read path was accepted: %s", out)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.WorkDir, "escape")); err != nil {
		t.Fatal(err)
	}
	if out, isErr := call(t, s, "local_contract_write", contractWriteArgs("escape/child.bot", nativeWriteSource, "absent")); !isErr || !strings.Contains(out, "inside") {
		t.Fatalf("symlink escape was accepted: %s", out)
	}
	readOnly := &Server{WorkDir: s.WorkDir, ReadOnly: true, Only: FamilyLocal}
	if _, err := readOnly.Call(context.Background(), "local_contract_write", json.RawMessage(contractWriteArgs("native.bot", nativeWriteSource, "absent"))); err == nil {
		t.Fatal("mutating tool was exposed in read-only mode")
	}
	if !strings.Contains(toolNames(readOnly.Tools()), "local_contract_read") {
		t.Fatal("public read tool was hidden in read-only mode")
	}
}

func toolNames(tools []Tool) string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	return strings.Join(names, ",")
}
