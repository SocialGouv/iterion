package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"gopkg.in/yaml.v3"
)

func source(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
func scanSource(t *testing.T, body string) census {
	t.Helper()
	root := t.TempDir()
	source(t, root, "subject_test.go", body)
	c, err := scan(root, []string{"subject_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCensusFindsTestingReceiversAndIgnoresText(t *testing.T) {
	c := scanSource(t, `//go:build windows && live
package subject
import (tt "testing"; clock "time")
type fixture struct { t *tt.T }
type unrelated struct{}
func (unrelated) Skip(string) {}
func (f *fixture) helper() { f.t.Skip("[fixture] requires a submodule") }
func TestProof(tb *tt.T) {
 // tb.Skip("comment"); clock.Sleep(99)
 _ = "tb.Skip(); clock.Sleep(99)"
 alias := tb
 alias.Skipf("[platform] needs %s", "windows")
 tb.Run("nested", func(child *tt.T) { child.Skip("[service] requires Mongo") })
 unrelated{}.Skip("not testing")
 clock.Sleep(2 * clock.Millisecond)
}
`)
	if len(c.Skips) != 3 || len(c.Sleeps) != 1 {
		t.Fatalf("skips=%+v sleeps=%+v", c.Skips, c.Sleeps)
	}
	if c.Skips[0].Line != 7 || c.Skips[0].Function != "helper" || c.Skips[0].Class != "fixture" || c.Skips[0].BuildConstraint != "windows && live" {
		t.Fatalf("field receiver site: %+v", c.Skips[0])
	}
	if c.Skips[1].Class != "platform" || c.Skips[2].Class != "service" {
		t.Fatalf("classes: %+v", c.Skips)
	}
}

func TestCensusNamesMissingAndDynamicReasons(t *testing.T) {
	c := scanSource(t, `package subject
import "testing"
func helper(t testing.TB, reason string) {
 const why = "fixture " + "absent"
 t.Skip(why)
 t.Skip()
 t.Skip("  ")
 t.SkipNow()
 t.Skip(reason)
 t.Skipf("tool absent: %v", reason)
}`)
	var statuses []string
	for _, s := range c.Skips {
		statuses = append(statuses, s.ReasonStatus)
	}
	want := []string{"named", "missing", "missing", "missing", "dynamic", "named"}
	if !reflect.DeepEqual(statuses, want) {
		t.Fatalf("reasons=%v want %v", statuses, want)
	}
	summary := string(summary(c))
	if !strings.Contains(summary, "subject_test.go:8") || !strings.Contains(summary, "t.Skip(reason)") {
		t.Fatal("inspection list loses missing/dynamic sites")
	}
}

func TestSleepBoundsUseConstantsAndTheSleepClock(t *testing.T) {
	c := scanSource(t, `package subject
import (. "time"; "testing"; bubble "testing/synctest")
const tick = 3 * Millisecond
var _ = func() { Sleep(0) }
func TestClock(t *testing.T) {
 Sleep(tick + tick/2)
 Sleep(-Second)
 Sleep((5/2)*Millisecond)
 Sleep(Duration(1<<4)*Microsecond)
 changing := Second
 changing *= 2
 Sleep(changing)
 Sleep(Until(Now()))
 bubble.Test(t, func(t *testing.T) { Sleep(5*Minute) })
}`)
	if len(c.Sleeps) != 8 {
		t.Fatalf("sleep sites=%+v", c.Sleeps)
	}
	want := []int64{0, 4500000, -1000000000, 2000000, 16000}
	for i, ns := range want {
		s := c.Sleeps[i]
		if s.RequestedNS == nil || *s.RequestedNS != ns || s.MinimumNS == nil || *s.MinimumNS != max(int64(0), ns) || s.MaximumNS != nil {
			t.Fatalf("sleep %d: %+v, want %d", i, s, ns)
		}
	}
	for _, s := range c.Sleeps[5:7] {
		if s.Status != "dynamic" || s.RequestedNS != nil || s.MinimumNS != nil {
			t.Fatalf("invented bound: %+v", s)
		}
	}
	if c.Sleeps[7].Clock != "synctest" || *c.Sleeps[7].RequestedNS != 300000000000 || c.Sleeps[1].Clock != "caller-dependent" {
		t.Fatalf("clock domain: %+v", c.Sleeps)
	}
}

func TestCensusBytesDoNotDependOnRootOrInputOrder(t *testing.T) {
	var outputs [][]byte
	for range 2 {
		root := t.TempDir()
		source(t, root, "z.go", "package subject\nimport \"time\"\nfunc z(){time.Sleep(time.Second)}")
		source(t, root, "a_test.go", "package subject\nimport (\"testing\"; \"time\")\nfunc TestA(t *testing.T){t.Skip(\"fixture absent\"); time.Sleep(time.Nanosecond)}")
		for _, paths := range [][]string{{"z.go", "a_test.go"}, {"a_test.go", "z.go"}} {
			c, err := scan(root, paths)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), root) {
				t.Fatal("absolute scratch path leaks into census")
			}
			outputs = append(outputs, append(raw, summary(c)...))
		}
	}
	for _, raw := range outputs[1:] {
		if string(raw) != string(outputs[0]) {
			t.Fatal("census depends on traversal order or checkout path")
		}
	}
}

func TestSourceBoundaryIncludesNewFilesAndAllPlatforms(t *testing.T) {
	root := gittest.SourceRepo(t)
	source(t, root, ".gitignore", "scratch/\n")
	for _, p := range []string{"a_test.go", "other_windows_test.go", "third_party/lib/a_test.go", "vendor/lib/a.go", "testdata/invalid.go", "scratch/local.go", "deleted.go"} {
		source(t, root, p, "package subject\n")
	}
	gittest.Run(t, root, "add", "a_test.go", "deleted.go")
	if err := os.Remove(filepath.Join(root, "deleted.go")); err != nil {
		t.Fatal(err)
	}
	paths, err := sourceFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a_test.go", "other_windows_test.go", "third_party/lib/a_test.go"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("sources=%v want %v", paths, want)
	}
	// I/O and syntax errors fail visibly rather than publishing a partial census.
	source(t, root, "a_test.go", "not Go")
	if _, err := scan(root, paths); err == nil {
		t.Fatal("malformed source silently disappears")
	}
}

func TestRunnerObservationsContainPresenceNeverValues(t *testing.T) {
	c := scanSource(t, `package subject
import ("testing"; "os"; "os/exec")
func helper(t *testing.T) {
 os.Getenv("TEST_CAPS_SECRET")
 os.LookupEnv("TEST_CAPS_ABSENT")
 exec.LookPath("testcaps-missing-tool")
 exec.LookPath("testcaps-tool")
 t.Skip("fixture absent")
 t.Skip("another fixture absent")
}`)
	r := observations(c, func(name string) string {
		if name == "TEST_CAPS_SECRET" {
			return "DO-NOT-PRINT-THIS"
		}
		return ""
	}, func(name string) (string, error) {
		if name == "testcaps-tool" {
			return "/private/operator/bin/tool", nil
		}
		return "", errors.New("absent")
	})
	if len(r.Probes) != 4 {
		t.Fatalf("probes=%+v", r.Probes)
	}
	want := []bool{false, true, false, true}
	for i, p := range r.Probes {
		if p.Available != want[i] {
			t.Fatalf("probe=%+v want available=%v", p, want[i])
		}
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "DO-NOT-PRINT") || strings.Contains(string(raw), "/private/") {
		t.Fatalf("observation discloses a value: %s", raw)
	}
}

func TestRepositorySkipsCarryReasons(t *testing.T) {
	root := "../../.."
	paths, err := sourceFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	c, err := scan(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Skips) == 0 || len(c.Sleeps) == 0 {
		t.Fatal("empty census proves nothing")
	}
	for _, s := range c.Skips {
		if s.ReasonStatus == "missing" {
			t.Errorf("%s:%d %s has no reason", s.File, s.Line, s.Expression)
		}
	}
	for _, file := range []string{"pkg/cli/clean_test.go", "cmd/iterion-desktop/cloud_signout_machine_model_test.go"} {
		found := false
		for _, s := range c.Skips {
			if s.File == file {
				found = true
			}
		}
		if !found {
			t.Errorf("fixture-field skip disappeared: %s", file)
		}
	}
}

func TestCIPublishesTheSameCensusEvenAfterFailure(t *testing.T) {
	raw, err := os.ReadFile("../../../.github/workflows/tests.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name, Run, Uses, If string
				With                map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	generated, published := false, false
	for _, step := range workflow.Jobs["test-unit"].Steps {
		if step.Name == "Test capability census" {
			generated = strings.Contains(step.Run, "go run -mod=vendor ./scripts/ci/testcaps -observe") && strings.Contains(step.Run, "GITHUB_STEP_SUMMARY") && step.If == "always()"
		}
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") && step.With["name"] == "test-capabilities-unit" {
			published = step.If == "always()" && step.With["path"] == ".iterion/test-capabilities/" && step.With["if-no-files-found"] == "error" && step.With["include-hidden-files"] == "true"
		}
	}
	if !generated || !published {
		t.Fatalf("CI census generated=%v published=%v", generated, published)
	}
	task, err := os.ReadFile("../../../Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	var tasks struct {
		Tasks map[string]struct{ Cmds []string }
	}
	// Other tasks have structured commands, so decode only the selected task.
	var tree map[string]any
	if err := yaml.Unmarshal(task, &tree); err != nil {
		t.Fatal(err)
	}
	selected := tree["tasks"].(map[string]any)["test:capabilities"]
	selectedYAML, err := yaml.Marshal(map[string]any{"tasks": map[string]any{"test:capabilities": selected}})
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(selectedYAML, &tasks); err != nil {
		t.Fatal(err)
	}
	if got := tasks.Tasks["test:capabilities"].Cmds; len(got) != 1 || got[0] != "go run -mod=vendor ./scripts/ci/testcaps {{.CLI_ARGS}}" {
		t.Fatalf("task uses different generator: %v", got)
	}
}

func TestCensusPreservesGoMessageAndTypeSemantics(t *testing.T) {
	c := scanSource(t, `package subject
import (. "time"; . "testing")
type OwnTesting T
func (*OwnTesting) Skip(...any) {}
type Alias = T
const tiny uint8 = 1
func TestDot(t *T, own *OwnTesting, alias *Alias) {
 own.Skip()
 t.Skip("", "fixture absent")
 alias.Skip("fixture absent")
 Sleep(Duration(^tiny))
 Sleep(Nanosecond)
}`)
	if len(c.Skips) != 2 || c.Skips[0].Message != "fixture absent" || c.Counts["skip/reason/missing"] != 0 {
		t.Fatalf("Go messages or type identity lost: %+v", c.Skips)
	}
	if len(c.Sleeps) != 2 || c.Sleeps[0].Status != "dynamic" || c.Sleeps[0].RequestedNS != nil || c.Sleeps[1].RequestedNS == nil || *c.Sleeps[1].RequestedNS != 1 {
		t.Fatalf("typed constant or dot import lost: %+v", c.Sleeps)
	}
}

func TestStaticRegenerationRemovesOldRunnerObservations(t *testing.T) {
	root := gittest.SourceRepo(t)
	source(t, root, "subject_test.go", "package subject\nimport \"testing\"\nfunc TestA(t *testing.T){t.Skip(\"fixture absent\")}")
	out := t.TempDir()
	if err := run(root, out, true); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(out, "census.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "runner.json")); err != nil {
		t.Fatal(err)
	}
	if err := run(root, out, false); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(out, "census.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("observation changes the source census")
	}
	if _, err := os.Stat(filepath.Join(out, "runner.json")); !os.IsNotExist(err) {
		t.Fatalf("stale observation remains: %v", err)
	}
}

func TestSleepNeverInventsBoundsFromUntypedNumericCoercion(t *testing.T) {
	c := scanSource(t, `package subject
import "time"
const tick time.Duration = 3.0
func proof() {
 time.Sleep(time.Duration(3.0)/2*2)
 time.Sleep(tick/2*2)
 time.Sleep(time.Second/3.0*3.0)
 time.Sleep(time.Duration(3+0i)/2*2)
 time.Sleep(time.Second/(3+0i)*(3+0i))
}`)
	if len(c.Sleeps) != 5 {
		t.Fatalf("sleep count: %d", len(c.Sleeps))
	}
	for _, s := range c.Sleeps {
		if s.Status != "dynamic" || s.RequestedNS != nil {
			t.Fatalf("untyped numeric coercion needs Go type checking, not a guessed bound: %+v", s)
		}
	}
}
