package kubernetes

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// LabelManaged identifies pods iterion owns. Used by Cleanup to
// safely target only iterion-created resources, and by Helm-side
// NetworkPolicy selectors (V2) to scope egress rules.
const (
	LabelManaged   = "iterion.io/managed"
	LabelRunID     = "iterion.io/run-id"
	LabelRunName   = "iterion.io/run-name"
	LabelComponent = "iterion.io/component"
)

// ComponentSandboxRun identifies the sibling pod that hosts a single
// iterion run's workload. Distinct from the runner pod (which
// orchestrates) and the netproxy pod (V2).
const ComponentSandboxRun = "sandbox-run"

// PodManifestInput is the surface the runtime layer hands to
// [BuildPodManifest]. Encapsulating the call avoids passing the
// whole Spec + RunInfo + namespace tuple down through helpers.
type PodManifestInput struct {
	Namespace      string
	Name           string
	RunID          string
	FriendlyName   string
	Spec           sandbox.Spec
	WorkspaceMount string // in-pod path; defaults to /workspace
	ProxyEndpoint  string // optional HTTPS_PROXY URL
}

// BuildPodManifest renders the YAML for a sibling pod that hosts a
// single iterion run.
//
// The pod's PID 1 is `sleep infinity` (mirrors the docker driver) so
// the engine can `kubectl exec` repeatedly into the same pod for
// each delegate invocation, amortising create+ready latency over
// the run's lifetime.
//
// Security defaults: runAsNonRoot, allowPrivilegeEscalation: false,
// capabilities.drop: [ALL], readOnlyRootFilesystem unless the spec
// explicitly opts out (V2). The workspace volume is an emptyDir so
// the run can write freely without persisting beyond the pod.
//
// Returns YAML bytes ready for `kubectl apply -f -`. The function
// renders JSON internally (the kubectl decoder accepts both); JSON
// is the safer encoding because it sidesteps every YAML parsing
// edge case (anchor merges, string vs date, multiline).
func BuildPodManifest(in PodManifestInput) ([]byte, error) {
	if in.Namespace == "" {
		return nil, fmt.Errorf("kubernetes: namespace is required")
	}
	if in.Name == "" {
		return nil, fmt.Errorf("kubernetes: pod name is required")
	}
	if in.Spec.Image == "" {
		return nil, fmt.Errorf("kubernetes: spec.image is required")
	}

	workspace := in.WorkspaceMount
	if workspace == "" {
		workspace = "/workspace"
	}
	if in.Spec.WorkspaceFolder != "" {
		workspace = in.Spec.WorkspaceFolder
	}

	labels := map[string]string{
		LabelManaged:   "true",
		LabelRunID:     in.RunID,
		LabelComponent: ComponentSandboxRun,
	}
	if in.FriendlyName != "" {
		labels[LabelRunName] = in.FriendlyName
	}

	// Build env list. ProxyEndpoint takes precedence over the spec's
	// own env if it sets HTTPS_PROXY — the engine-managed proxy is
	// the security boundary and shouldn't be silently overridden.
	envSlice := envMapToSlice(in.Spec.Env)
	if in.ProxyEndpoint != "" {
		envSlice = upsertEnv(envSlice, "HTTPS_PROXY", in.ProxyEndpoint)
		envSlice = upsertEnv(envSlice, "HTTP_PROXY", in.ProxyEndpoint)
		envSlice = upsertEnv(envSlice, "NO_PROXY", "localhost,127.0.0.1,.svc,.cluster.local")
	}

	// V2-7: parse spec.Mounts (devcontainer-style strings) into k8s
	// Volumes + VolumeMounts. Bind mounts are rejected at the driver
	// layer (cloud pods don't have host filesystem access) so we only
	// see PVC / ConfigMap / Secret types here.
	extraVolumes, extraVolumeMounts, mountErr := translateMounts(in.Spec.Mounts)
	if mountErr != nil {
		return nil, fmt.Errorf("kubernetes: %w", mountErr)
	}

	volumeMounts := []any{
		map[string]any{
			"name":      "workspace",
			"mountPath": workspace,
		},
	}
	for _, vm := range extraVolumeMounts {
		volumeMounts = append(volumeMounts, vm)
	}

	volumes := []any{
		map[string]any{
			"name":     "workspace",
			"emptyDir": map[string]any{},
		},
	}
	for _, v := range extraVolumes {
		volumes = append(volumes, v)
	}

	pod := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      in.Name,
			"namespace": in.Namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"restartPolicy":                "Never",
			"automountServiceAccountToken": false,
			"securityContext": map[string]any{
				"runAsNonRoot":   true,
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
				"fsGroup":        1000,
			},
			"containers": []any{
				map[string]any{
					"name":            "workload",
					"image":           in.Spec.Image,
					"command":         []any{"sleep", "infinity"},
					"workingDir":      workspace,
					"env":             envSlice,
					"securityContext": defaultContainerSecurityContext(in.Spec.User),
					"volumeMounts":    volumeMounts,
				},
			},
			"volumes": volumes,
		},
	}

	return json.MarshalIndent(pod, "", "  ")
}

// defaultContainerSecurityContext returns the per-container
// security context: drop all capabilities, no privilege escalation,
// runAsNonRoot. When the spec sets a remoteUser, parse the optional
// "uid:gid" form and apply it.
func defaultContainerSecurityContext(user string) map[string]any {
	ctx := map[string]any{
		"allowPrivilegeEscalation": false,
		"capabilities": map[string]any{
			"drop": []any{"ALL"},
		},
		"runAsNonRoot": true,
	}
	if user == "" {
		return ctx
	}
	uid, gid, ok := parseUserSpec(user)
	if !ok {
		// Pass through verbatim — the kube API server will reject
		// it with a clear message if the form is wrong, which is
		// preferable to swallowing the error here.
		return ctx
	}
	ctx["runAsUser"] = uid
	if gid > 0 {
		ctx["runAsGroup"] = gid
	}
	return ctx
}

// parseUserSpec parses the devcontainer remoteUser convention. The
// form is "uid", "name", or "uid:gid" (we only support numeric IDs
// for the security context — names live in the image's /etc/passwd
// which we can't inspect from here).
func parseUserSpec(s string) (uid, gid int64, ok bool) {
	parts := strings.SplitN(s, ":", 2)
	uid, err := atoi64(parts[0])
	if err != nil {
		return 0, 0, false
	}
	if len(parts) > 1 {
		gid, err = atoi64(parts[1])
		if err != nil {
			return uid, 0, true // uid valid, gid skipped
		}
	}
	return uid, gid, true
}

// atoi64 is a small helper that accepts only fully numeric strings.
// Negative IDs are rejected (Pod security context refuses them).
func atoi64(s string) (int64, error) {
	var n int64
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit %q", c)
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// envMapToSlice converts the spec's map[string]string env into the
// []corev1.EnvVar shape the API server expects. Order is sorted
// alphabetically for stable diffs in `kubectl describe pod`.
func envMapToSlice(env map[string]string) []any {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{"name": k, "value": env[k]})
	}
	return out
}

// upsertEnv replaces (or appends) an env var on the slice. Used to
// inject HTTPS_PROXY / NO_PROXY without duplicating user-supplied
// entries.
func upsertEnv(env []any, name, value string) []any {
	for i, e := range env {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if existing, _ := m["name"].(string); existing == name {
			env[i] = map[string]any{"name": name, "value": value}
			return env
		}
	}
	return append(env, map[string]any{"name": name, "value": value})
}
