package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/testinventory"
)

func inventory(names ...string) string {
	var out strings.Builder
	enc := json.NewEncoder(&out)
	for _, event := range []map[string]string{
		{"Action": "start", "Package": "example.test/suite"},
		{"Action": "output", "Package": "example.test/suite", "Output": strings.Join(names, "\n") + "\n"},
		{"Action": "pass", "Package": "example.test/suite"},
	} {
		if err := enc.Encode(event); err != nil {
			panic(err)
		}
	}
	return out.String()
}

func TestTestPatternsCoverEveryGoInventoryNameOnce(t *testing.T) {
	names := []string{"TestParent", "TestParentLonger", "Example", "Example_output", "FuzzBytes", "TestÉtat"}
	for i := 0; i < 100; i++ {
		names = append(names, fmt.Sprintf("TestFamily_%d", i))
	}
	listed, err := testinventory.Parse(strings.NewReader(inventory(names...)))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 2, 4, 7} {
		counts := map[string]int{}
		for shard := 0; shard < n; shard++ {
			var out, reordered bytes.Buffer
			if err := emitTestPatterns(n, shard, false, listed, &out); err != nil {
				t.Fatal(err)
			}
			reverse := slices.Clone(listed)
			slices.Reverse(reverse)
			if err := emitTestPatterns(n, shard, false, reverse, &reordered); err != nil || out.String() != reordered.String() {
				t.Fatalf("discovery order changes shard %d of %d: %v", shard, n, err)
			}
			pattern := regexp.MustCompile(strings.TrimSpace(out.String()))
			for _, name := range names {
				if pattern.MatchString(name) {
					counts[name]++
				}
			}
			if pattern.MatchString("TestParentLongerStill") || pattern.MatchString("NotTestParent") {
				t.Fatalf("pattern is not anchored to full names: %s", pattern)
			}
		}
		for _, name := range names {
			if counts[name] != 1 {
				t.Errorf("%s appears in %d of %d shards, want exactly one", name, counts[name], n)
			}
		}
	}
}

func TestTestDiscoveryRefusesPartialOrAmbiguousInventories(t *testing.T) {
	valid := inventory("TestOne", "Example", "FuzzInput")
	for name, input := range map[string]string{
		"empty": "", "no names": inventory(), "duplicate name": inventory("TestOne", "TestOne"),
		"truncated JSON": valid + "{", "no terminal event": strings.Replace(valid, `"Action":"pass"`, `"Action":"output"`, 1),
		"failure":         strings.Replace(valid, `"Action":"pass"`, `"Action":"fail"`, 1),
		"second package":  valid + strings.ReplaceAll(valid, "example.test/suite", "example.test/other"),
		"missing package": strings.ReplaceAll(valid, `,"Package":"example.test/suite"`, ""),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := testinventory.Parse(strings.NewReader(input)); err == nil {
				t.Fatal("invalid discovery produced an apparently usable inventory")
			}
		})
	}
	for _, args := range [][2]int{{0, 0}, {1, 1}, {4, 0}} {
		var out bytes.Buffer
		if err := emitTestPatterns(args[0], args[1], false, []string{"TestOnly"}, &out); err == nil || out.Len() != 0 {
			t.Errorf("invalid/empty partition %v emitted %q with err %v", args, out.String(), err)
		}
	}
}

func TestTestDiscoveryFailurePublishesNoPattern(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "inventory.json")
	if err := os.WriteFile(fixture, []byte(inventory("TestOne")), 0600); err != nil {
		t.Fatal(err)
	}
	// The child emits a complete-looking inventory before failing. Its
	// status must outrank the text, or the next CI command judges a subset.
	script := "#!/bin/sh\nwhile IFS= read -r line; do printf '%s\\n' \"$line\"; done < \"$INVENTORY\"\nexit 9\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("INVENTORY", fixture)
	var out bytes.Buffer
	if err := runTests(1, 0, false, "./suite", time.Minute, &out); err == nil || out.Len() != 0 {
		t.Fatalf("failed discovery emitted %q with err %v", out.String(), err)
	}
}

func TestTestPatternsExecuteTestsExamplesFuzzAndChildrenOnce(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"go.mod": "module example.test/partition\n\ngo 1.26\n",
		"partition_test.go": `package partition
import ("fmt"; "testing")
func TestOne(t *testing.T) {}
func TestTwo(t *testing.T) { t.Run("child", func(t *testing.T) {}) }
func Example() { fmt.Println("fixture")
// Output: fixture
}

func FuzzInput(f *testing.F) { f.Add("seed"); f.Fuzz(func(t *testing.T, s string) {}) }
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	counts := map[string]int{}
	for shard := 0; shard < 2; shard++ {
		var pattern bytes.Buffer
		if err := runTests(2, shard, false, ".", time.Minute, &pattern); err != nil {
			t.Fatal(err)
		}
		data, err := exec.Command("go", "test", "-mod=vendor", "-count=1", "-json", "-run", strings.TrimSpace(pattern.String()), ".").CombinedOutput()
		if err != nil {
			t.Fatalf("selected execution: %v\n%s", err, data)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		for {
			var event struct{ Action, Test string }
			if err := decoder.Decode(&event); err != nil {
				if err == io.EOF {
					break
				}
				t.Fatal(err)
			}
			if event.Action == "pass" && event.Test != "" {
				counts[event.Test]++
			}
		}
	}
	for _, name := range []string{"TestOne", "TestTwo", "TestTwo/child", "Example", "FuzzInput", "FuzzInput/seed#0"} {
		if counts[name] != 1 {
			t.Errorf("%s passed %d times across partitions, want 1: %v", name, counts[name], counts)
		}
	}
}
