package testinventory

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func chunkedInventory(t *testing.T, chunks ...string) []byte {
	t.Helper()
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	write := func(action, output string) {
		t.Helper()
		if err := enc.Encode(map[string]string{"Action": action, "Package": "example.test/chunks", "Output": output}); err != nil {
			t.Fatal(err)
		}
	}
	write("start", "")
	for _, chunk := range chunks {
		write("output", chunk)
	}
	write("pass", "")
	return out.Bytes()
}

func TestParseJoinsLinesAcrossOutputEvents(t *testing.T) {
	want := []string{"Example", "FuzzInput", "TestOne", "TestTwo"}
	for _, chunks := range [][]string{
		{"TestOne\nTestTwo\nExample\nFuzzInput\n"},
		{"Te", "stOne\nTest", "Two\nEx", "ample\nFuzzInput", "\n"},
		strings.Split("TestOne\nTestTwo\nExample\nFuzzInput\n", ""),
	} {
		got, err := Parse(bytes.NewReader(chunkedInventory(t, chunks...)))
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("chunks %q: got %v, err %v; want %v", chunks, got, err, want)
		}
	}
	if _, err := Parse(bytes.NewReader(chunkedInventory(t, "TestOne\nTestUnfinished"))); err == nil {
		t.Error("an unterminated inventory line was accepted as complete")
	}
}

func TestParseSeparatesBuildDiagnosticsFromTheInventory(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		wantErr     bool
	}{
		{"compiler warning", `{"Action":"build-output","ImportPath":"example.test/suite [example.test/suite.test]","Output":"compiler warning\n"}`, false},
		{"dependency output is not a test", `{"Action":"build-output","ImportPath":"example.test/dependency","Output":"TestCompilerNoise\n"}`, false},
		{"failed build before a pass", `{"Action":"build-fail","ImportPath":"example.test/dependency"}`, true},
		{"build has no identity", `{"Action":"build-output","Output":"warning\n"}`, true},
		{"build is not a test event", `{"Action":"build-output","ImportPath":"example.test/dependency","Package":"example.test/chunks"}`, true},
		{"unknown build action", `{"Action":"build-unknown","ImportPath":"example.test/dependency"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.event + "\n" + string(chunkedInventory(t, "TestOne\n"))
			got, err := Parse(strings.NewReader(data))
			if tc.wantErr {
				if err == nil || len(got) != 0 {
					t.Fatalf("invalid build stream produced an inventory: %v, err %v", got, err)
				}
				return
			}
			if err != nil || !slices.Equal(got, []string{"TestOne"}) {
				t.Fatalf("build diagnostics changed the inventory: %v, err %v", got, err)
			}
		})
	}
}
