package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestResolveDoctorTarget(t *testing.T) {
	cases := []struct {
		optTarget, driver, want string
	}{
		{"cloud", "docker", "kubernetes"},
		{"k8s", "noop", "kubernetes"},
		{"local", "kubernetes", "docker"},
		{"auto", "docker", "docker"},
		{"", "podman", "docker"},
		{"", "kubernetes", "kubernetes"},
		{"", "noop", ""},
		{"", "<none>", ""},
	}
	for _, tc := range cases {
		if got := resolveDoctorTarget(tc.optTarget, tc.driver); got != tc.want {
			t.Errorf("resolveDoctorTarget(%q, %q) = %q, want %q", tc.optTarget, tc.driver, got, tc.want)
		}
	}
}

func TestCrossHostDoctorValidation(t *testing.T) {
	t.Run("isExplicitDoctorTarget", func(t *testing.T) {
		cases := []struct {
			target string
			want   bool
		}{
			{"cloud", true},
			{"kubernetes", true},
			{"k8s", true},
			{"local", true},
			{"docker", true},
			{"podman", true},
			{" Cloud ", true}, // case + whitespace insensitive
			{"", false},
			{"auto", false},
			{"bogus", false},
		}
		for _, tc := range cases {
			if got := isExplicitDoctorTarget(tc.target); got != tc.want {
				t.Errorf("isExplicitDoctorTarget(%q) = %v, want %v", tc.target, got, tc.want)
			}
		}
	})

	t.Run("crossHostDoctorValidation", func(t *testing.T) {
		cases := []struct {
			target, driver string
			want           bool
		}{
			// Runtime-less host + explicit target → cross-host (downgrade).
			{"cloud", "<none>", true},
			{"local", "<none>", true},
			{"cloud", "", true},
			// Runtime-less host, no/auto target → genuine local failure.
			{"", "<none>", false},
			{"auto", "<none>", false},
			// Driver selected, forced battery differs from natural one.
			{"cloud", "docker", true},     // kubernetes != docker
			{"local", "kubernetes", true}, // docker != kubernetes
			// Driver selected, forced battery matches natural one.
			{"cloud", "kubernetes", false}, // kubernetes == kubernetes
			{"local", "docker", false},     // docker == docker
			{"docker", "podman", false},    // both → docker battery
			// No explicit target never downgrades.
			{"", "docker", false},
			{"auto", "kubernetes", false},
		}
		for _, tc := range cases {
			if got := crossHostDoctorValidation(tc.target, tc.driver); got != tc.want {
				t.Errorf("crossHostDoctorValidation(%q, %q) = %v, want %v", tc.target, tc.driver, got, tc.want)
			}
		}
	})
}

func TestStaticBinaryWarning(t *testing.T) {
	t.Run("linkage String", func(t *testing.T) {
		cases := map[binaryLinkage]string{
			linkStatic:  "static",
			linkDynamic: "dynamic",
			linkUnknown: "unknown",
		}
		for link, want := range cases {
			if got := link.String(); got != want {
				t.Errorf("binaryLinkage(%d).String() = %q, want %q", link, got, want)
			}
		}
	})

	t.Run("warning only on dynamic", func(t *testing.T) {
		if w := staticBinaryWarning("/usr/bin/iterion", linkStatic); w != "" {
			t.Errorf("staticBinaryWarning(static) = %q, want empty", w)
		}
		if w := staticBinaryWarning("/usr/bin/iterion", linkUnknown); w != "" {
			t.Errorf("staticBinaryWarning(unknown) = %q, want empty", w)
		}
		w := staticBinaryWarning("/usr/bin/iterion", linkDynamic)
		if w == "" {
			t.Fatal("staticBinaryWarning(dynamic) = empty, want a warning")
		}
		for _, want := range []string{"/usr/bin/iterion", "CGO_ENABLED=0", "__claw-runner", "WARNING"} {
			if !strings.Contains(w, want) {
				t.Errorf("staticBinaryWarning(dynamic) missing %q in:\n%s", want, w)
			}
		}
	})

	t.Run("detectBinaryLinkage unprobeable", func(t *testing.T) {
		if got := detectBinaryLinkage(""); got != linkUnknown {
			t.Errorf("detectBinaryLinkage(\"\") = %v, want linkUnknown", got)
		}
		if got := detectBinaryLinkage("/nonexistent/iterion/binary"); got != linkUnknown {
			t.Errorf("detectBinaryLinkage(nonexistent) = %v, want linkUnknown", got)
		}
	})
}

func TestRunNetworkStrictChecks(t *testing.T) {
	t.Run("good preset + rules", func(t *testing.T) {
		r := &SandboxStrictReport{}
		runNetworkStrictChecks(r, &sandbox.Spec{Network: &sandbox.Network{
			Mode:   sandbox.NetworkModeAllowlist,
			Preset: "iterion-default",
			Rules:  []string{"**.example.com", "!evil.example.com"},
		}})
		if r.Failed() {
			t.Fatalf("expected no failures, got %+v", r.Checks)
		}
	})

	t.Run("unknown preset", func(t *testing.T) {
		r := &SandboxStrictReport{}
		runNetworkStrictChecks(r, &sandbox.Spec{Network: &sandbox.Network{
			Mode:   sandbox.NetworkModeAllowlist,
			Preset: "nope-not-a-preset",
		}})
		if !r.Failed() {
			t.Fatal("expected a failure for unknown preset")
		}
		if !hasFailNamed(r, "network preset") {
			t.Errorf("expected a failing 'network preset' check, got %+v", r.Checks)
		}
	})

	t.Run("bad rule syntax", func(t *testing.T) {
		r := &SandboxStrictReport{}
		runNetworkStrictChecks(r, &sandbox.Spec{Network: &sandbox.Network{
			Mode:  sandbox.NetworkModeAllowlist,
			Rules: []string{"a*b.com"}, // embedded wildcard — rejected by compileRule
		}})
		if !hasFailNamed(r, "network allowlist syntax") {
			t.Errorf("expected a failing 'network allowlist syntax' check, got %+v", r.Checks)
		}
	})
}

// fakeDriver is a minimal sandbox.Driver with controllable capabilities
// for the capability-mismatch check.
type fakeDriver struct {
	name string
	caps sandbox.Capabilities
}

func (f fakeDriver) Name() string                       { return f.name }
func (f fakeDriver) Capabilities() sandbox.Capabilities { return f.caps }
func (f fakeDriver) Prepare(context.Context, sandbox.Spec) (sandbox.PreparedSpec, error) {
	return nil, nil
}
func (f fakeDriver) Start(context.Context, sandbox.PreparedSpec, sandbox.RunInfo) (sandbox.Run, error) {
	return nil, nil
}

// fakePolicyDriver is a fakeDriver that also reports a scheduling policy,
// the way the kubernetes driver does.
type fakePolicyDriver struct {
	fakeDriver
	policy string
	err    error
}

func (f fakePolicyDriver) SchedulingPolicy() (string, error) { return f.policy, f.err }

// A malformed policy fails every run at sandbox start, never the
// constructor — the doctor must surface it as a FAIL naming the cause.
func TestRunSchedulingPolicyStrictCheck(t *testing.T) {
	t.Run("driver without a policy adds nothing", func(t *testing.T) {
		r := &SandboxStrictReport{}
		runSchedulingPolicyStrictCheck(r, fakeDriver{name: "fake"})
		if len(r.Checks) != 0 {
			t.Fatalf("expected no check, got %+v", r.Checks)
		}
	})
	t.Run("malformed policy fails", func(t *testing.T) {
		r := &SandboxStrictReport{}
		runSchedulingPolicyStrictCheck(r, fakePolicyDriver{err: errors.New("kubernetes: ITERION_SANDBOX_K8S_REQUESTS_CPU=\"two\" is not a Kubernetes quantity")})
		if !hasFailNamed(r, "sandbox scheduling policy") {
			t.Fatalf("expected a scheduling policy failure, got %+v", r.Checks)
		}
		if !strings.Contains(r.Checks[0].Detail, "ITERION_SANDBOX_K8S_REQUESTS_CPU") {
			t.Fatalf("the failure must name the variable, got %+v", r.Checks[0])
		}
	})
	t.Run("valid policy passes with its summary", func(t *testing.T) {
		r := &SandboxStrictReport{}
		runSchedulingPolicyStrictCheck(r, fakePolicyDriver{policy: "requests cpu=2 memory=4Gi, spread=kubernetes.io/hostname"})
		if r.Failed() || len(r.Checks) != 1 || r.Checks[0].Detail != "requests cpu=2 memory=4Gi, spread=kubernetes.io/hostname" {
			t.Fatalf("expected one passing check carrying the summary, got %+v", r.Checks)
		}
	})
}

// kubectlNodesShim puts a fake `kubectl` first on PATH whose `get nodes -o json`
// answers with the given node labels.
func kubectlNodesShim(t *testing.T, nodesJSON string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in \"get nodes \"*) printf '%s' '" + nodesJSON + "'; exit 0 ;; esac\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A spread over a label the nodes do not carry excludes them from scheduling
// — soft or not — so the doctor must say which nodes carry it.
func TestRunSpreadCoverageStrictCheck(t *testing.T) {
	threeNodes := `{"items":[{"metadata":{"labels":{"topology.kubernetes.io/zone":"a"}}},{"metadata":{"labels":{}}},{"metadata":{"labels":{"topology.kubernetes.io/zone":"b"}}}]}`

	t.Run("hostname needs no check", func(t *testing.T) {
		r := &SandboxStrictReport{}
		runSpreadCoverageStrictCheck(context.Background(), r, "kubernetes.io/hostname")
		if len(r.Checks) != 0 {
			t.Fatalf("expected no check, got %+v", r.Checks)
		}
	})
	t.Run("no node carries the key fails", func(t *testing.T) {
		kubectlNodesShim(t, threeNodes)
		r := &SandboxStrictReport{}
		runSpreadCoverageStrictCheck(context.Background(), r, "example.com/rack")
		if !hasFailNamed(r, "k8s spread key coverage") {
			t.Fatalf("expected a coverage failure, got %+v", r.Checks)
		}
	})
	t.Run("partial coverage warns", func(t *testing.T) {
		kubectlNodesShim(t, threeNodes)
		r := &SandboxStrictReport{}
		runSpreadCoverageStrictCheck(context.Background(), r, "topology.kubernetes.io/zone")
		if r.Failed() || len(r.Checks) != 1 || r.Checks[0].Status != CheckWarn || !strings.Contains(r.Checks[0].Detail, "2 of 3") {
			t.Fatalf("expected a 2 of 3 warning, got %+v", r.Checks)
		}
	})
}

func TestRunCapabilityStrictChecks(t *testing.T) {
	noCaps := fakeDriver{name: "fake", caps: sandbox.Capabilities{}}

	t.Run("build unsupported", func(t *testing.T) {
		r := &SandboxStrictReport{}
		runCapabilityStrictChecks(r, &sandbox.Spec{Build: &sandbox.Build{Dockerfile: "Dockerfile"}}, noCaps)
		if !hasFailNamed(r, "driver capabilities") {
			t.Errorf("expected capability failure, got %+v", r.Checks)
		}
	})

	t.Run("all supported", func(t *testing.T) {
		allCaps := fakeDriver{name: "fake", caps: sandbox.Capabilities{
			SupportsBuild: true, SupportsMounts: true, SupportsRemoteUser: true, SupportsPostCreate: true,
		}}
		r := &SandboxStrictReport{}
		runCapabilityStrictChecks(r, &sandbox.Spec{
			Build: &sandbox.Build{Dockerfile: "Dockerfile"}, User: "1000", PostCreate: "x", Mounts: []string{"m"},
		}, allCaps)
		if r.Failed() {
			t.Errorf("expected no capability failure with full caps, got %+v", r.Checks)
		}
	})
}

func TestReportFailedAndRenderExit(t *testing.T) {
	r := &SandboxStrictReport{Mode: "auto", Host: "local"}
	r.add("ok", CheckPass, "", "")
	r.add("warn", CheckWarn, "advisory", "do x")
	if r.Failed() {
		t.Fatal("warn-only report must not be Failed()")
	}

	// renderStrict returns nil when no fail.
	p := &Printer{W: &bytes.Buffer{}, Format: OutputHuman}
	if err := renderStrict(p, r); err != nil {
		t.Errorf("renderStrict on warn-only report = %v, want nil", err)
	}

	r.add("boom", CheckFail, "broken", "fix it")
	if !r.Failed() {
		t.Fatal("report with a fail must be Failed()")
	}
	var buf bytes.Buffer
	p = &Printer{W: &buf, Format: OutputHuman}
	if err := renderStrict(p, r); !errors.Is(err, errStrictSandboxChecks) {
		t.Errorf("renderStrict on failing report = %v, want errStrictSandboxChecks", err)
	}
	out := buf.String()
	if !strings.Contains(out, "[FAIL] boom") || !strings.Contains(out, "hint: fix it") {
		t.Errorf("rendered output missing fail line / hint:\n%s", out)
	}
}

func TestRunSandboxDoctorStrictNoSandbox(t *testing.T) {
	// No workflow file + Sandbox="none" → spec resolves inactive → warn,
	// nil error, no driver selection or shelling out.
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputHuman}
	err := RunSandboxDoctor(context.Background(), p, SandboxDoctorOptions{Strict: true, Sandbox: "none"})
	if err != nil {
		t.Fatalf("strict doctor with sandbox=none = %v, want nil", err)
	}
	if !strings.Contains(buf.String(), "sandbox configured") {
		t.Errorf("expected 'sandbox configured' warn, got:\n%s", buf.String())
	}
}

func TestRunSandboxDoctorStrictResolutionFailure(t *testing.T) {
	// An invalid mode override fails spec resolution → fail check → exit
	// error. Deterministic (no driver/daemon contact).
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputJSON}
	err := RunSandboxDoctor(context.Background(), p, SandboxDoctorOptions{Strict: true, Sandbox: "bogus-mode"})
	if !errors.Is(err, errStrictSandboxChecks) {
		t.Fatalf("strict doctor with bogus mode = %v, want errStrictSandboxChecks", err)
	}
	if !strings.Contains(buf.String(), "spec resolution") {
		t.Errorf("expected a 'spec resolution' fail in JSON, got:\n%s", buf.String())
	}
}

func TestBasicDoctorUnchanged(t *testing.T) {
	// Non-strict path still works and never errors.
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputHuman}
	if err := RunSandboxDoctor(context.Background(), p, SandboxDoctorOptions{}); err != nil {
		t.Fatalf("basic doctor = %v, want nil", err)
	}
	if !strings.Contains(buf.String(), "doctor report") {
		t.Errorf("basic doctor output changed unexpectedly:\n%s", buf.String())
	}
}

func hasFailNamed(r *SandboxStrictReport, name string) bool {
	for _, c := range r.Checks {
		if c.Name == name && c.Status == CheckFail {
			return true
		}
	}
	return false
}
