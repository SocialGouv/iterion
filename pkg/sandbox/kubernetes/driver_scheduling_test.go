package kubernetes

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// Unset means the manifest of a driver without the knobs: no resources and
// no spread — the scheduler's own policy, as before.
func TestSchedulingFromEnvDefaultsToNothing(t *testing.T) {
	s, err := schedulingFromEnv(envOf(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.resources != (PodResources{}) {
		t.Fatalf("resources must be unset by default, got %+v", s.resources)
	}
	if s.spreadKey != "" {
		t.Fatalf("spread must be off by default, got %q", s.spreadKey)
	}
	if got := s.String(); got != "no resources, no spread" {
		t.Fatalf("summary = %q", got)
	}
}

func TestSchedulingFromEnvParsesQuantities(t *testing.T) {
	s, err := schedulingFromEnv(envOf(map[string]string{
		RequestsCPUEnvVar:    " 2 ",
		RequestsMemoryEnvVar: "4Gi",
		LimitsCPUEnvVar:      "2.5",
		LimitsMemoryEnvVar:   "8Gi",
		SpreadEnvVar:         "hostname",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := PodResources{
		Requests: ResourceList{CPU: "2", Memory: "4Gi"},
		Limits:   ResourceList{CPU: "2.5", Memory: "8Gi"},
	}
	if s.resources != want {
		t.Fatalf("resources = %+v, want %+v", s.resources, want)
	}
	if got := s.String(); got != "requests cpu=2 memory=4Gi, limits cpu=2.5 memory=8Gi, spread=kubernetes.io/hostname" {
		t.Fatalf("summary = %q", got)
	}
	// The accepted quantity subset: <digits>, <digits>.<digits>, <digits>., .<digits>,
	// an optional sign, an exponent, a CPU (m) or byte (k…Ei) suffix.
	for _, v := range []string{"500m", "3", "0.25", ".5", "1.", "+1", "1e3", "1E3"} {
		if _, err := schedulingFromEnv(envOf(map[string]string{RequestsCPUEnvVar: v})); err != nil {
			t.Errorf("%q must be accepted as a CPU quantity: %v", v, err)
		}
	}
	for _, v := range []string{"128Mi", "2Ti", "100k", "0.5Gi", "1e9", "4G"} {
		if _, err := schedulingFromEnv(envOf(map[string]string{RequestsMemoryEnvVar: v})); err != nil {
			t.Errorf("%q must be accepted as a memory quantity: %v", v, err)
		}
	}
}

// A typo must fail with the variable and the value named, never be rendered
// for the API server to reject on every pod.
func TestSchedulingFromEnvRejectsMalformedQuantities(t *testing.T) {
	// `1e400` matches the grammar and overflows float64: refused, not rendered
	// for the API server to reject on every pod.
	for _, v := range []string{"2 cores", "4GB", "-1", "1,5", "Gi", "two", "2ki", "1e", ".", "1u", "1n", "1e400"} {
		_, err := schedulingFromEnv(envOf(map[string]string{RequestsMemoryEnvVar: v}))
		if err == nil {
			t.Errorf("%q must be refused", v)
			continue
		}
		if !strings.Contains(err.Error(), RequestsMemoryEnvVar) || !strings.Contains(err.Error(), v) {
			t.Errorf("error for %q must name the variable and the value, got %q", v, err)
		}
	}
}

// A zero quantity renders a resources block that schedules exactly like no
// resources block — the one lie the policy exists to prevent. `1e-400` is a
// zero too: it underflows float64, and ParseFloat reports no error for it.
func TestSchedulingFromEnvRejectsZeroQuantities(t *testing.T) {
	for _, v := range []string{"0", "0.0", "0Gi", "0m", "+0", "0e3", ".0", "1e-400", "0.0e0"} {
		for _, env := range []string{RequestsCPUEnvVar, RequestsMemoryEnvVar} {
			_, err := schedulingFromEnv(envOf(map[string]string{env: v}))
			if err == nil || !strings.Contains(err.Error(), "zero") || !strings.Contains(err.Error(), env) {
				t.Errorf("%s=%q must be refused as a zero quantity naming the variable, got %v", env, v, err)
			}
		}
	}
}

// Below Kubernetes' precision (1m of CPU, 1 byte of memory) the API server
// rounds up at admission: such a "floor" is a zero wearing a number. The
// precision itself is accepted.
func TestSchedulingFromEnvRejectsQuantitiesBelowPrecision(t *testing.T) {
	for _, v := range []string{"0.0001", "5e-324", "1e-323", "0.5m"} {
		_, err := schedulingFromEnv(envOf(map[string]string{RequestsCPUEnvVar: v}))
		if err == nil || !strings.Contains(err.Error(), "precision") || !strings.Contains(err.Error(), v) {
			t.Errorf("cpu=%q must be refused as below precision naming the value, got %v", v, err)
		}
	}
	for _, v := range []string{"0.5", "5e-324", "0.9"} {
		_, err := schedulingFromEnv(envOf(map[string]string{RequestsMemoryEnvVar: v}))
		if err == nil || !strings.Contains(err.Error(), "precision") || !strings.Contains(err.Error(), v) {
			t.Errorf("memory=%q must be refused as below precision naming the value, got %v", v, err)
		}
	}
	if _, err := schedulingFromEnv(envOf(map[string]string{RequestsCPUEnvVar: "1m", RequestsMemoryEnvVar: "1"})); err != nil {
		t.Fatalf("the precision itself must be accepted, got %v", err)
	}
}

// A mantissa that fits float64 can still overflow once its suffix is applied;
// out of range means out of range whichever step produced it.
func TestSchedulingFromEnvRejectsOverflowAfterScaling(t *testing.T) {
	v := strings.Repeat("9", 301) + "Ei"
	_, err := schedulingFromEnv(envOf(map[string]string{RequestsMemoryEnvVar: v}))
	if err == nil || !strings.Contains(err.Error(), "out of range") || !strings.Contains(err.Error(), RequestsMemoryEnvVar) {
		t.Fatalf("a scaled overflow must be refused as out of range naming the variable, got %v", err)
	}
	if got, err := quantityValue(v); err == nil {
		t.Fatalf("quantityValue(%q) = %v, want an out-of-range error", v[:8]+"…Ei", got)
	}
}

// The suffix must fit the resource: `m` on memory is milli-bytes (a CPU
// suffix on the wrong variable), a byte suffix on CPU is never a core count.
func TestSchedulingFromEnvRejectsSuffixOnWrongResource(t *testing.T) {
	if _, err := schedulingFromEnv(envOf(map[string]string{RequestsMemoryEnvVar: "400m"})); err == nil || !strings.Contains(err.Error(), "milli-byte") {
		t.Fatalf("memory=400m must be refused as milli-bytes, got %v", err)
	}
	if _, err := schedulingFromEnv(envOf(map[string]string{LimitsCPUEnvVar: "2Gi"})); err == nil || !strings.Contains(err.Error(), "byte suffix") {
		t.Fatalf("cpu=2Gi must be refused as a byte suffix on CPU, got %v", err)
	}
	// `Ei` must be read as a suffix, not as the start of an exponent.
	if _, err := schedulingFromEnv(envOf(map[string]string{RequestsCPUEnvVar: "1Ei"})); err == nil || !strings.Contains(err.Error(), "byte suffix") {
		t.Fatalf("cpu=1Ei must be refused as a byte suffix on CPU, got %v", err)
	}
}

// A limit without its request becomes the request at admission (the API
// server copies it), and a limit below its request is rejected there too —
// both are refused here, where the variable can be named.
func TestSchedulingFromEnvRequiresRequestUnderLimit(t *testing.T) {
	_, err := schedulingFromEnv(envOf(map[string]string{LimitsMemoryEnvVar: "8Gi"}))
	if err == nil || !strings.Contains(err.Error(), LimitsMemoryEnvVar) || !strings.Contains(err.Error(), RequestsMemoryEnvVar) {
		t.Fatalf("a limit without its request must be refused naming both variables, got %v", err)
	}
	_, err = schedulingFromEnv(envOf(map[string]string{RequestsCPUEnvVar: "2", LimitsCPUEnvVar: "500m"}))
	if err == nil || !strings.Contains(err.Error(), "below") {
		t.Fatalf("a limit below its request must be refused, got %v", err)
	}
	_, err = schedulingFromEnv(envOf(map[string]string{RequestsMemoryEnvVar: "4Gi", LimitsMemoryEnvVar: "4096Mi"}))
	if err != nil {
		t.Fatalf("an equal limit in another unit must be accepted, got %v", err)
	}
	_, err = schedulingFromEnv(envOf(map[string]string{RequestsMemoryEnvVar: "4Gi", LimitsMemoryEnvVar: "4G"}))
	if err == nil {
		t.Fatalf("4G (decimal) is below 4Gi (binary) and must be refused")
	}
	// The exbibyte suffix evaluates like every other one: above a gibibyte
	// as a limit, below a mebibyte never.
	_, err = schedulingFromEnv(envOf(map[string]string{RequestsMemoryEnvVar: "1Gi", LimitsMemoryEnvVar: "10Ei"}))
	if err != nil {
		t.Fatalf("a 10Ei limit over a 1Gi request must be accepted, got %v", err)
	}
	_, err = schedulingFromEnv(envOf(map[string]string{RequestsMemoryEnvVar: "1Ei", LimitsMemoryEnvVar: "1Mi"}))
	if err == nil || !strings.Contains(err.Error(), "below") {
		t.Fatalf("a 1Mi limit under a 1Ei request must be refused, got %v", err)
	}
}

func TestQuantityValue(t *testing.T) {
	cases := map[string]float64{
		"2": 2, "500m": 0.5, "4Gi": 4 * (1 << 30), "4G": 4e9, "1e3": 1000, ".5": 0.5, "1.": 1, "+3": 3, "128Mi": 128 * (1 << 20),
		// `E` and `Ei` start like an exponent and are suffixes: an exponent
		// needs digits or a sign after the `e`.
		"2E": 2e18, "1Ei": 1 << 60, "10Ei": 10 * (1 << 60), "1E3": 1000, "1e+3": 1000, "1e-3": 0.001, "1.e2": 100,
	}
	for v, want := range cases {
		got, err := quantityValue(v)
		if err != nil || got != want {
			t.Errorf("quantityValue(%q) = %v, %v; want %v", v, got, err, want)
		}
	}
	// What cannot be evaluated is an error, never a silent zero.
	for _, v := range []string{"", "x", "1x", "1Ei3", "1e"} {
		if got, err := quantityValue(v); err == nil {
			t.Errorf("quantityValue(%q) = %v, want an error", v, got)
		}
	}
}

func TestSplitQuantity(t *testing.T) {
	cases := map[string][2]string{
		"2": {"2", ""}, "500m": {"500", "m"}, "4Gi": {"4", "Gi"}, "1e3": {"1e3", ""}, "1E3": {"1E3", ""},
		"1e+3": {"1e+3", ""}, "1e-3": {"1e-3", ""}, "2E": {"2", "E"}, "1Ei": {"1", "Ei"}, "10Ei": {"10", "Ei"},
		"1.E": {"1.", "E"}, ".5Ei": {".5", "Ei"}, "+1Ei": {"1", "Ei"},
	}
	for v, want := range cases {
		number, suffix := splitQuantity(v)
		if number != want[0] || suffix != want[1] {
			t.Errorf("splitQuantity(%q) = (%q, %q), want (%q, %q)", v, number, suffix, want[0], want[1])
		}
	}
}

// The keywords fold case; anything else must be a prefixed label key — a bare
// word is a typo the API server would accept as a label no node carries,
// which turns the spread into a silent no-op.
func TestSchedulingFromEnvSpreadSelector(t *testing.T) {
	cases := map[string]string{
		"":                            "",
		"none":                        "",
		"NONE":                        "",
		"off":                         "",
		"Off":                         "",
		"hostname":                    hostnameTopologyKey,
		"Hostname":                    hostnameTopologyKey,
		"topology.kubernetes.io/zone": "topology.kubernetes.io/zone",
		"example.com/Rack_1":          "example.com/Rack_1",
	}
	for v, want := range cases {
		s, err := schedulingFromEnv(envOf(map[string]string{SpreadEnvVar: v}))
		if err != nil {
			t.Errorf("%q: unexpected error %v", v, err)
			continue
		}
		if s.spreadKey != want {
			t.Errorf("%q: spread key = %q, want %q", v, s.spreadKey, want)
		}
	}
	for _, v := range []string{"zone", "node", "hostnmae", "true", "1", "disabled", "not a key", "/zone", "Example.com/zone", "example.com/", "example.com/-zone", "bad/key/again"} {
		_, err := schedulingFromEnv(envOf(map[string]string{SpreadEnvVar: v}))
		if err == nil || !strings.Contains(err.Error(), SpreadEnvVar) || !strings.Contains(err.Error(), v) {
			t.Errorf("%q must be refused naming the variable and the value, got %v", v, err)
		}
	}
}

func TestSchedulingPolicyReportsSummaryOrError(t *testing.T) {
	sched, _ := schedulingFromEnv(envOf(map[string]string{RequestsCPUEnvVar: "2", SpreadEnvVar: "hostname"}))
	d := &Driver{namespace: "test", sched: sched}
	if got, err := d.SchedulingPolicy(); err != nil || got != "requests cpu=2, spread=kubernetes.io/hostname" {
		t.Fatalf("SchedulingPolicy = %q, %v", got, err)
	}
	if d.SpreadTopologyKey() != hostnameTopologyKey {
		t.Fatalf("SpreadTopologyKey = %q", d.SpreadTopologyKey())
	}
	_, schedErr := schedulingFromEnv(envOf(map[string]string{RequestsCPUEnvVar: "two"}))
	bad := &Driver{namespace: "test", schedErr: schedErr}
	if _, err := bad.SchedulingPolicy(); err == nil || !strings.Contains(err.Error(), RequestsCPUEnvVar) {
		t.Fatalf("SchedulingPolicy must return the parse error naming the variable, got %v", err)
	}
	var _ sandbox.SchedulingPolicyReporter = bad
}

// ValidateSchedulingEnv is what the runner calls at bootstrap; it must read
// the real environment and return the same error Start would.
func TestValidateSchedulingEnvReadsTheProcessEnvironment(t *testing.T) {
	t.Setenv(RequestsCPUEnvVar, "2 cores")
	if err := ValidateSchedulingEnv(); err == nil || !strings.Contains(err.Error(), RequestsCPUEnvVar) {
		t.Fatalf("expected the malformed value to be refused naming the variable, got %v", err)
	}
	t.Setenv(RequestsCPUEnvVar, "2")
	if err := ValidateSchedulingEnv(); err != nil {
		t.Fatalf("a valid value must pass, got %v", err)
	}
}

// The factory's preference walk skips any constructor error and lands on the
// noop driver, so a malformed value must fail each run at Start instead —
// loudly, with the variable named, before any kubectl call.
func TestStartRefusesWhenSchedulingIsMalformed(t *testing.T) {
	_, schedErr := schedulingFromEnv(envOf(map[string]string{RequestsCPUEnvVar: "two"}))
	if schedErr == nil {
		t.Fatal("precondition: malformed quantity must produce an error")
	}
	d := &Driver{namespace: "test", kubectl: "/nonexistent/kubectl", schedErr: schedErr}
	_, err := d.Start(context.Background(), &Prepared{spec: sandbox.Spec{Image: "img", User: "1000:1000"}, workspace: "/workspace"}, sandbox.RunInfo{RunID: "r"})
	if err == nil || !strings.Contains(err.Error(), RequestsCPUEnvVar) {
		t.Fatalf("Start must surface the scheduling error naming %s, got %v", RequestsCPUEnvVar, err)
	}
}
