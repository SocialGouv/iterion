package kubernetes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestCapabilitiesAdvertisedFeatures(t *testing.T) {
	d := &Driver{namespace: "test"}
	caps := d.Capabilities()

	if !caps.SupportsImage {
		t.Error("kubernetes driver must advertise SupportsImage")
	}
	if !caps.SupportsPostCreate {
		t.Error("kubernetes driver must advertise SupportsPostCreate")
	}
	if !caps.SupportsRemoteUser {
		t.Error("kubernetes driver must advertise SupportsRemoteUser")
	}
	if !caps.SupportsNetworkPolicy {
		t.Error("V2-5 must advertise SupportsNetworkPolicy (per-run policy synthesis is wired)")
	}
	// Still deferred.
	if caps.SupportsBuild {
		t.Error("Phase 5 V1 must NOT advertise SupportsBuild (BuildKit lands in V2-6)")
	}
	if caps.SupportsMounts {
		t.Error("Phase 5 V1 must NOT advertise SupportsMounts (PVCs land in V2-7)")
	}
}

func TestPrepareRejectsBuild(t *testing.T) {
	d := &Driver{namespace: "test"}
	_, err := d.Prepare(context.Background(), sandbox.Spec{
		Mode:  sandbox.ModeInline,
		Build: &sandbox.Build{Dockerfile: "Dockerfile"},
	})
	if err == nil {
		t.Fatal("expected rejection of sandbox.Build")
	}
}

func TestPrepareRejectsMounts(t *testing.T) {
	d := &Driver{namespace: "test"}
	_, err := d.Prepare(context.Background(), sandbox.Spec{
		Mode:   sandbox.ModeInline,
		Image:  "alpine:3",
		Mounts: []string{"type=bind,source=/a,target=/b"},
	})
	if err == nil {
		t.Fatal("expected rejection of sandbox.Mounts (V1 deferred)")
	}
}

func TestPrepareRejectsMissingImage(t *testing.T) {
	d := &Driver{namespace: "test"}
	_, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeInline})
	if err == nil {
		t.Fatal("expected rejection of inline spec without image")
	}
}

func TestPrepareRequiresUser(t *testing.T) {
	d := &Driver{namespace: "test"}
	_, err := d.Prepare(context.Background(), sandbox.Spec{
		Mode:  sandbox.ModeInline,
		Image: "alpine:3.20",
	})
	if err == nil {
		t.Fatal("expected rejection of spec with empty User (runAsNonRoot would fail at kubelet)")
	}
	if !strings.Contains(err.Error(), "sandbox.user") {
		t.Errorf("error should mention sandbox.user, got: %v", err)
	}
}

func TestPrepareRejectsNonNumericUser(t *testing.T) {
	d := &Driver{namespace: "test"}
	_, err := d.Prepare(context.Background(), sandbox.Spec{
		Mode:  sandbox.ModeInline,
		Image: "alpine:3.20",
		User:  "root",
	})
	if err == nil {
		t.Fatal("expected rejection of non-numeric user")
	}
	if !strings.Contains(err.Error(), "must be numeric") {
		t.Errorf("error should mention numeric requirement, got: %v", err)
	}
}

func TestPrepareAcceptsValidUser(t *testing.T) {
	d := &Driver{namespace: "test"}
	for _, user := range []string{"1000", "1000:1000", "10001:10001"} {
		_, err := d.Prepare(context.Background(), sandbox.Spec{
			Mode:  sandbox.ModeInline,
			Image: "alpine:3.20",
			User:  user,
		})
		if err != nil {
			t.Errorf("Prepare with user=%q failed: %v", user, err)
		}
	}
}

func TestProxyConfigRequiresPodIP(t *testing.T) {
	t.Setenv(PodIPEnvVar, "")
	d := &Driver{namespace: "test"}
	_, _, err := d.ProxyConfig()
	if err == nil {
		t.Fatal("expected error when ITERION_POD_IP is unset")
	}
	if !strings.Contains(err.Error(), PodIPEnvVar) {
		t.Errorf("error should mention %s, got: %v", PodIPEnvVar, err)
	}
}

func TestProxyConfigReturnsBindAndPodIP(t *testing.T) {
	t.Setenv(PodIPEnvVar, "10.42.0.7")
	d := &Driver{namespace: "test"}
	bind, advertise, err := d.ProxyConfig()
	if err != nil {
		t.Fatalf("ProxyConfig: %v", err)
	}
	if bind != "0.0.0.0:0" {
		t.Errorf("bind = %q, want 0.0.0.0:0 (sibling pods reach the proxy across the cluster network)", bind)
	}
	if advertise != "10.42.0.7" {
		t.Errorf("advertise = %q, want 10.42.0.7 (read from %s downward API)", advertise, PodIPEnvVar)
	}
}

func TestBuildNetworkPolicyValidates(t *testing.T) {
	cases := []struct {
		name string
		in   NetworkPolicyInput
	}{
		{"missing namespace", NetworkPolicyInput{Name: "x", RunID: "r1", RunnerPodIP: "10.0.0.1"}},
		{"missing name", NetworkPolicyInput{Namespace: "ns", RunID: "r1", RunnerPodIP: "10.0.0.1"}},
		{"missing runID", NetworkPolicyInput{Namespace: "ns", Name: "x", RunnerPodIP: "10.0.0.1"}},
		{"missing runnerIP", NetworkPolicyInput{Namespace: "ns", Name: "x", RunID: "r1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := BuildNetworkPolicy(c.in); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestBuildNetworkPolicyShape(t *testing.T) {
	out, err := BuildNetworkPolicy(NetworkPolicyInput{
		Namespace:    "iterion",
		Name:         "iterion-run-r1",
		RunID:        "run_1",
		FriendlyName: "happy-axe-1234",
		RunnerPodIP:  "10.42.0.7",
	})
	if err != nil {
		t.Fatalf("BuildNetworkPolicy: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"apiVersion": "networking.k8s.io/v1"`,
		`"kind": "NetworkPolicy"`,
		`"iterion.io/run-id": "run_1"`,
		`"cidr": "10.42.0.7/32"`,
		`"k8s-app": "kube-dns"`,
		`"port": 53`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("manifest missing %q\nfull:\n%s", want, s)
		}
	}
}

func TestPodNameDeterministic(t *testing.T) {
	a := podNameFor("run_123")
	b := podNameFor("run_123")
	if a != b {
		t.Errorf("podNameFor not deterministic: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "iterion-run-") {
		t.Errorf("name = %q, want prefix iterion-run-", a)
	}
	if strings.Contains(a, "_") {
		t.Errorf("name = %q must not contain underscores (DNS-1123 violation)", a)
	}
}

func TestPodNameFitsDNS1123Limit(t *testing.T) {
	long := "run_" + strings.Repeat("x", 200)
	got := podNameFor(long)
	if len(got) > 63 {
		t.Errorf("name length = %d, exceeds DNS-1123 label limit of 63", len(got))
	}
}

func TestPodManifestStructure(t *testing.T) {
	manifest, err := BuildPodManifest(PodManifestInput{
		Namespace:    "iterion",
		Name:         "iterion-run-test",
		RunID:        "test-1",
		FriendlyName: "swift-cedar",
		Spec: sandbox.Spec{
			Mode:  sandbox.ModeInline,
			Image: "alpine:3",
			Env:   map[string]string{"FOO": "bar"},
		},
		ProxyEndpoint: "http://t:tok@host:8080",
	})
	if err != nil {
		t.Fatalf("BuildPodManifest: %v", err)
	}

	var pod map[string]any
	if err := json.Unmarshal(manifest, &pod); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	meta := pod["metadata"].(map[string]any)
	if meta["name"] != "iterion-run-test" {
		t.Errorf("metadata.name = %v", meta["name"])
	}
	if meta["namespace"] != "iterion" {
		t.Errorf("metadata.namespace = %v", meta["namespace"])
	}
	labels := meta["labels"].(map[string]any)
	if labels[LabelManaged] != "true" {
		t.Errorf("missing managed label: %v", labels)
	}
	if labels[LabelRunID] != "test-1" {
		t.Errorf("missing run-id label: %v", labels)
	}
	if labels[LabelRunName] != "swift-cedar" {
		t.Errorf("missing run-name label: %v", labels)
	}

	spec := pod["spec"].(map[string]any)
	if spec["restartPolicy"] != "Never" {
		t.Errorf("restartPolicy = %v, want Never", spec["restartPolicy"])
	}
	if v, _ := spec["automountServiceAccountToken"].(bool); v {
		t.Error("automountServiceAccountToken must be false (sibling pods don't need cluster API access)")
	}
	psc := spec["securityContext"].(map[string]any)
	if v, _ := psc["runAsNonRoot"].(bool); !v {
		t.Error("pod securityContext.runAsNonRoot must be true")
	}
	containers := spec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("containers len = %d, want 1", len(containers))
	}
	c0 := containers[0].(map[string]any)
	if c0["image"] != "alpine:3" {
		t.Errorf("container image = %v", c0["image"])
	}
	cmd := c0["command"].([]any)
	if len(cmd) != 2 || cmd[0] != "sleep" || cmd[1] != "infinity" {
		t.Errorf("command = %v, want [sleep infinity]", cmd)
	}
	csc := c0["securityContext"].(map[string]any)
	if v, _ := csc["allowPrivilegeEscalation"].(bool); v {
		t.Error("container allowPrivilegeEscalation must be false")
	}
	caps := csc["capabilities"].(map[string]any)
	dropped := caps["drop"].([]any)
	if len(dropped) != 1 || dropped[0] != "ALL" {
		t.Errorf("capabilities.drop = %v, want [ALL]", dropped)
	}

	envSlice := c0["env"].([]any)
	gotEnv := map[string]string{}
	for _, e := range envSlice {
		m := e.(map[string]any)
		gotEnv[m["name"].(string)] = m["value"].(string)
	}
	if gotEnv["FOO"] != "bar" {
		t.Errorf("env FOO = %q", gotEnv["FOO"])
	}
	if gotEnv["HTTPS_PROXY"] != "http://t:tok@host:8080" {
		t.Errorf("HTTPS_PROXY = %q", gotEnv["HTTPS_PROXY"])
	}
	if gotEnv["NO_PROXY"] == "" {
		t.Error("NO_PROXY must be set when proxy endpoint is provided")
	}
}

func TestPodManifestRequiresImage(t *testing.T) {
	_, err := BuildPodManifest(PodManifestInput{
		Namespace: "ns",
		Name:      "n",
		Spec:      sandbox.Spec{Mode: sandbox.ModeInline},
	})
	if err == nil {
		t.Fatal("expected error for missing image")
	}
}

func TestParseUserSpec(t *testing.T) {
	cases := []struct {
		in       string
		uid, gid int64
		ok       bool
	}{
		{"1000", 1000, 0, true},
		{"1000:2000", 1000, 2000, true},
		{"node", 0, 0, false},       // names not supported by securityContext
		{"", 0, 0, false},           // empty
		{"-1", 0, 0, false},         // negative refused
		{"1000:foo", 1000, 0, true}, // gid skipped, uid kept
	}
	for _, c := range cases {
		uid, gid, ok := parseUserSpec(c.in)
		if ok != c.ok {
			t.Errorf("parseUserSpec(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !c.ok {
			continue
		}
		if uid != c.uid {
			t.Errorf("parseUserSpec(%q) uid = %d, want %d", c.in, uid, c.uid)
		}
		if gid != c.gid {
			t.Errorf("parseUserSpec(%q) gid = %d, want %d", c.in, gid, c.gid)
		}
	}
}

func TestShellSingleQuote(t *testing.T) {
	cases := map[string]string{
		"":            "''",
		"hello":       "hello", // no special chars → no quoting
		"hello world": "'hello world'",
		"it's":        "'it'\\''s'",
		"a$b":         "'a$b'",
		"/workspace":  "/workspace", // no special chars
	}
	for in, want := range cases {
		if got := shellSingleQuote(in); got != want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUpsertEnvReplacesExisting(t *testing.T) {
	env := []any{
		map[string]any{"name": "FOO", "value": "1"},
		map[string]any{"name": "BAR", "value": "2"},
	}
	got := upsertEnv(env, "FOO", "99")
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 (replace, not append)", len(got))
	}
	first := got[0].(map[string]any)
	if first["value"] != "99" {
		t.Errorf("FOO = %v, want 99", first["value"])
	}
}

func TestUpsertEnvAppendsNew(t *testing.T) {
	env := []any{map[string]any{"name": "FOO", "value": "1"}}
	got := upsertEnv(env, "BAR", "2")
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}
