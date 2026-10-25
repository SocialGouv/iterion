// Command testcaps publishes a deterministic source census of test skips and
// sleeps, plus optional observations of the runner's literal prerequisites.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

type site struct {
	File            string `json:"file"`
	Line            int    `json:"line"`
	Column          int    `json:"column"`
	Function        string `json:"function"`
	Scope           string `json:"scope"`
	BuildConstraint string `json:"build_constraint,omitempty"`
}
type prerequisite struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}
type skip struct {
	site
	Method         string         `json:"method"`
	Message        string         `json:"message"`
	Expression     string         `json:"expression"`
	ReasonStatus   string         `json:"reason_status"`
	Class          string         `json:"class"`
	FunctionProbes []prerequisite `json:"function_probes,omitempty"`
}
type sleep struct {
	site
	Clock       string `json:"clock"`
	Expression  string `json:"expression"`
	Status      string `json:"status"`
	RequestedNS *int64 `json:"requested_ns"`
	MinimumNS   *int64 `json:"minimum_ns"`
	// Sleep has no finite upper bound; static analysis cannot measure elapsed time.
	MaximumNS *int64 `json:"maximum_ns"`
}
type census struct {
	SchemaVersion int            `json:"schema_version"`
	Mode          string         `json:"mode"`
	Exclusions    []string       `json:"exclusions"`
	Files         int            `json:"files"`
	Counts        map[string]int `json:"counts"`
	Skips         []skip         `json:"skips"`
	Sleeps        []sleep        `json:"sleeps"`
}

func main() {
	root := flag.String("root", ".", "repository root")
	out := flag.String("out", ".iterion/test-capabilities", "output directory")
	observe := flag.Bool("observe", false, "also report runner platform and literal function probes (no secret values or service calls)")
	flag.Parse()
	if err := run(*root, *out, *observe); err != nil {
		fmt.Fprintln(os.Stderr, "testcaps:", err)
		os.Exit(1)
	}
}

func run(root, out string, observe bool) error {
	paths, err := sourceFiles(root)
	if err != nil {
		return err
	}
	c, err := scan(root, paths)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(out, "census.json"), c); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "summary.md"), summary(c), 0o644); err != nil {
		return err
	}
	if observe {
		if err := writeJSON(filepath.Join(out, "runner.json"), observations(c, os.Getenv, exec.LookPath)); err != nil {
			return err
		}
	} else if err := os.Remove(filepath.Join(out, "runner.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("testcaps: %d skips, %d sleeps; %s\n", len(c.Skips), len(c.Sleeps), out)
	return nil
}

// Git supplies the same source boundary as review, including new local files.
// Deleted tracked paths are absent from the working tree and no longer count.
func sourceFiles(root string) ([]string, error) {
	cmd := exec.Command("git", gitlib.NoRunHooks("ls-files", "--cached", "--others", "--exclude-standard", "-z")...)
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	cmd.Dir = root
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list source files: %w", err)
	}
	seen := map[string]bool{}
	var paths []string
	for _, p := range strings.Split(string(raw), "\x00") {
		if !strings.HasSuffix(p, ".go") || seen[p] {
			continue
		}
		excluded := false
		for _, part := range strings.Split(p, "/") {
			if part == "vendor" || part == "testdata" {
				excluded = true
			}
		}
		if excluded {
			continue
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("source is not a regular file: %s", p)
		}
		seen[p] = true
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func summary(c census) []byte {
	var b bytes.Buffer
	fmt.Fprint(&b, "# Test capability census\n\nStatic source inventory; all build tags and platforms. No elapsed-time measurements.\n")
	fmt.Fprintf(&b, "%d Go files; **%d skips**, **%d sleeps**. Full sites and expressions: `census.json`.\n\n", c.Files, len(c.Skips), len(c.Sleeps))
	fmt.Fprintln(&b, "| Metric | Count |\n|---|---:|")
	keys := make([]string, 0, len(c.Counts))
	for k := range c.Counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "| %s | %d |\n", k, c.Counts[k])
	}
	fmt.Fprintln(&b, "\n## Reasons requiring inspection\n\n| Site | Function | Status | Expression |\n|---|---|---|---|")
	for _, s := range c.Skips {
		if s.ReasonStatus != "named" {
			fmt.Fprintf(&b, "| %s:%d | %s | %s | %s |\n", s.File, s.Line, s.Function, s.ReasonStatus, markdown(s.Expression))
		}
	}
	fmt.Fprintln(&b, "\nSleep `requested_ns` is the constant argument, `minimum_ns` is max(0, requested) in the sleep clock domain, and `maximum_ns` is null (unbounded). Dynamic arguments have null bounds. A site is not an execution count.\n\n`runner.json`, when requested, reports only platform, executable lookup and nonempty environment variables referenced literally in functions containing skips. It does not prove credentials or services work.")
	return b.Bytes()
}
func markdown(s string) string {
	return strings.NewReplacer("|", "&#124;", "\n", " ", "<", "&lt;", ">", "&gt;").Replace(s)
}

type observation struct {
	prerequisite
	Available bool `json:"available"`
}
type runner struct {
	GOOS   string        `json:"goos"`
	GOARCH string        `json:"goarch"`
	Probes []observation `json:"probes"`
}

func observations(c census, getenv func(string) string, lookpath func(string) (string, error)) runner {
	r := runner{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Probes: []observation{}}
	seen := map[prerequisite]bool{}
	for _, s := range c.Skips {
		for _, p := range s.FunctionProbes {
			if seen[p] {
				continue
			}
			seen[p] = true
			available := getenv(p.Name) != ""
			if p.Kind == "executable" {
				_, err := lookpath(p.Name)
				available = err == nil
			}
			r.Probes = append(r.Probes, observation{p, available})
		}
	}
	sort.Slice(r.Probes, func(i, j int) bool {
		a, b := r.Probes[i], r.Probes[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	return r
}
