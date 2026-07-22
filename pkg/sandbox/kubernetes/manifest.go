package kubernetes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
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

// OwnerReference identifies the k8s object that owns a per-run resource
// (sandbox pod, CA/file-secrets Secret, NetworkPolicy). Setting it makes
// the cluster garbage-collect the resource when the owner is deleted
// (cascade GC), independent of iterion's own Cleanup path.
//
// iterion sets the owner to the RUNNER pod (read from the downward-API
// env vars in the driver): when a runner pod is removed by a deployment
// rollout, node drain or scale-down, k8s takes all of its sandbox pods,
// Secrets and NetworkPolicies with it — closing the
// plaintext-credential-at-rest window without waiting for the label
// reaper. It is best-effort: when the downward API isn't wired the owner
// is nil and the run relies on activeDeadlineSeconds + the label reaper
// (defence in depth). See ADR-070.
type OwnerReference struct {
	APIVersion string
	Kind       string
	Name       string
	UID        string
}

// valid reports whether the owner carries the two fields k8s requires to
// establish an ownerReference (name + uid). A nil or partial owner is
// treated as "no owner" so callers can pass it unconditionally.
func (o *OwnerReference) valid() bool {
	return o != nil && o.Name != "" && o.UID != ""
}

// ownerReferencesJSON renders the metadata.ownerReferences list for a
// valid owner, or nil to omit the field entirely. Deliberately a plain
// (non-controller) reference: cascade GC deletes dependents on owner
// deletion regardless of the controller flag, and omitting
// controller/blockOwnerDeletion avoids needing extra RBAC on the owner.
func ownerReferencesJSON(o *OwnerReference) []any {
	if !o.valid() {
		return nil
	}
	apiVersion := o.APIVersion
	if apiVersion == "" {
		apiVersion = "v1"
	}
	kind := o.Kind
	if kind == "" {
		kind = "Pod"
	}
	return []any{
		map[string]any{
			"apiVersion": apiVersion,
			"kind":       kind,
			"name":       o.Name,
			"uid":        o.UID,
		},
	}
}

// withOwner stamps ownerReferences onto a metadata map when the owner is
// valid. No-op otherwise, so builders call it unconditionally.
func withOwner(meta map[string]any, o *OwnerReference) {
	if refs := ownerReferencesJSON(o); refs != nil {
		meta["ownerReferences"] = refs
	}
}

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
	// CASecretName, when non-empty, is the name of a Secret (created by
	// the driver before the pod) holding the egress TLS-inspection CA
	// under key caSecretKey. The manifest mounts it and points the
	// CA-bundle env vars at it so in-pod clients trust the proxy's
	// minted leaves (Layer 2 secret egress substitution).
	CASecretName string
	// SecretFilesSecretName, when non-empty, is the name of a per-run
	// Secret holding runtime-materialised workflow file secrets. The pod
	// mounts each Secret key read-only at its requested in-sandbox path.
	SecretFilesSecretName string

	// Owner, when valid, is stamped as the pod's metadata.ownerReferences
	// so the cluster cascade-GCs the sandbox pod when the owner (the
	// runner pod) is deleted. Best-effort — nil when the downward API
	// isn't wired. See ADR-070.
	Owner *OwnerReference

	// ActiveDeadlineSeconds, when > 0, bounds the sandbox pod's wall-clock
	// lifetime (spec.activeDeadlineSeconds): kubelet fails the pod once it
	// exceeds this, so a leaked pod (runner SIGKILLed/OOM-killed mid-run,
	// Cleanup never fired) stops consuming compute deterministically
	// rather than idling as `sleep infinity` forever. Derived from the
	// run's max_duration + a margin (see driver.go). See ADR-070.
	ActiveDeadlineSeconds int64
}

// caSecretKey is the Secret data key + projected filename for the egress
// inspection CA. caMountDir is where the Secret volume is mounted in the
// pod; caContainerPath is the resulting in-pod CA file path.
const (
	caSecretKey     = "egress-ca.pem"
	caMountDir      = "/run/iterion-ca"
	caContainerPath = caMountDir + "/" + caSecretKey
)

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
// pullPolicyForImage chooses the pod's imagePullPolicy from the image ref. A
// pinned digest (…@sha256:…) is immutable, so IfNotPresent avoids a needless
// re-pull. Any mutable tag (:edge, :latest, a plain :tag, or no tag) → Always,
// so a freshly rebuilt image (e.g. iterion-sandbox-full:edge after a CI bake) is
// picked up instead of a stale node-cached layer.
func pullPolicyForImage(image string) string {
	if strings.Contains(image, "@sha256:") {
		return "IfNotPresent"
	}
	return "Always"
}

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

	podMeta := map[string]any{
		"name":      in.Name,
		"namespace": in.Namespace,
		"labels":    labels,
	}
	withOwner(podMeta, in.Owner)

	// Build env list. ProxyEndpoint takes precedence over the spec's
	// own env if it sets HTTPS_PROXY — the engine-managed proxy is
	// the security boundary and shouldn't be silently overridden.
	envSlice := envMapToSlice(in.Spec.Env)
	if in.ProxyEndpoint != "" {
		envSlice = upsertEnv(envSlice, "HTTPS_PROXY", in.ProxyEndpoint)
		envSlice = upsertEnv(envSlice, "HTTP_PROXY", in.ProxyEndpoint)
		// NO_PROXY=localhost,127.0.0.1 only. The previous default
		// added .svc and .cluster.local — those let the sandboxed
		// workload bypass the iterion proxy and reach in-cluster
		// services (kube API at kubernetes.default.svc.cluster.local,
		// internal Prometheus / Vault / Mongo, ...) WITHOUT going
		// through the allowlist. The synthesised NetworkPolicy (see
		// driver.go) locks egress to runner pod + kube-dns and blocks
		// in-cluster bypass on a NetworkPolicy-aware CNI (Calico,
		// Cilium). On CNIs that don't enforce NetworkPolicy
		// (kindnet, weave-net without policy controller) the
		// cluster-suffix carve-outs were a real proxy bypass. Drop
		// them so the network posture matches the documented
		// "allowlist via iterion proxy" promise.
		envSlice = upsertEnv(envSlice, "NO_PROXY", "localhost,127.0.0.1")
	}

	// Git safe.directory for the workspace. The emptyDir mountpoint is
	// created root-owned by kubelet (fsGroup only sets the group), while
	// every `kubectl exec` — the post-populate git fixup, the agent's own
	// git, tool nodes — runs as the non-root workload user, so git ≥2.35.2
	// rejects the repo at the workspace root as dubiously owned; `git -C
	// <ws> config …` then fails discovery with the notoriously opaque
	// "fatal: not in a git directory" (exit 128 — observed live on run
	// 019f8a50, breaking sandbox start at fixupWorkspaceGit). safe.directory
	// is only honoured from protected configuration; the GIT_CONFIG_*
	// environment ("command" scope, git ≥2.38) is the one protected channel
	// a pod env can carry, and every exec inherits it.
	envSlice, gitEnvErr := appendGitSafeDirectoryEnv(envSlice, workspace)
	if gitEnvErr != nil {
		return nil, fmt.Errorf("kubernetes: %w", gitEnvErr)
	}

	// Egress TLS-inspection CA (Layer 2): mount the per-run CA Secret and
	// point every common client's CA-bundle env var at it. In inspection
	// mode the proxy terminates all egress TLS, so the only certificate
	// an in-pod client ever sees is our leaf — our-CA-only is correct.
	caVolumes, caVolumeMounts := caInjection(in.CASecretName, &envSlice)
	secretFileVolumes, secretFileVolumeMounts := secretFilesInjection(in.SecretFilesSecretName, in.Spec.SecretFiles)

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
	volumeMounts = append(volumeMounts, secretFileVolumeMounts...)
	volumeMounts = append(volumeMounts, caVolumeMounts...)

	volumes := []any{
		map[string]any{
			"name":     "workspace",
			"emptyDir": map[string]any{},
		},
	}
	for _, v := range extraVolumes {
		volumes = append(volumes, v)
	}
	volumes = append(volumes, secretFileVolumes...)
	volumes = append(volumes, caVolumes...)

	podSpec := map[string]any{
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
				"imagePullPolicy": pullPolicyForImage(in.Spec.Image),
				"command":         []any{"sleep", "infinity"},
				"workingDir":      workspace,
				"env":             envSlice,
				"securityContext": defaultContainerSecurityContext(in.Spec.User),
				"volumeMounts":    volumeMounts,
			},
		},
		"volumes": volumes,
	}
	// Bound the pod's lifetime so a leaked pod self-fails instead of
	// idling forever (runner killed mid-run before Cleanup fired).
	if in.ActiveDeadlineSeconds > 0 {
		podSpec["activeDeadlineSeconds"] = in.ActiveDeadlineSeconds
	}

	pod := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   podMeta,
		"spec":       podSpec,
	}

	return json.MarshalIndent(pod, "", "  ")
}

const secretFilesVolumeName = "iterion-secret-files"

func secretFilesInjection(secretName string, files []sandbox.SecretFileMount) (volumes, volumeMounts []any) {
	if secretName == "" || len(files) == 0 {
		return nil, nil
	}
	defaultItems := make([]any, 0, len(files))
	customItems := make([]any, 0, len(files))
	customMounts := make([]any, 0, len(files))
	for i, sf := range files {
		key := secretFileKey(i, sf.Name)
		if rel, ok := secrets.RelativeToSecretFilesMountDir(sf.MountPath); ok {
			defaultItems = append(defaultItems, map[string]any{"key": key, "path": rel})
			continue
		}
		customItems = append(customItems, map[string]any{"key": key, "path": key})
		customMounts = append(customMounts, map[string]any{
			"mountPath": sf.MountPath,
			"subPath":   key,
			"readOnly":  true,
		})
	}

	if len(defaultItems) > 0 {
		name := secretFilesVolumeName
		if len(customItems) > 0 {
			name = secretFilesVolumeName + "-dir"
		}
		volumes = append(volumes, secretFileVolume(name, secretName, defaultItems))
		volumeMounts = append(volumeMounts, map[string]any{
			"name":      name,
			"mountPath": secrets.SecretFilesMountDir,
			"readOnly":  true,
		})
	}
	if len(customItems) > 0 {
		name := secretFilesVolumeName
		if len(defaultItems) > 0 {
			name = secretFilesVolumeName + "-custom"
		}
		volumes = append(volumes, secretFileVolume(name, secretName, customItems))
		for _, mount := range customMounts {
			if m, ok := mount.(map[string]any); ok {
				m["name"] = name
			}
			volumeMounts = append(volumeMounts, mount)
		}
	}
	return volumes, volumeMounts
}

func secretFileVolume(volumeName, secretName string, items []any) map[string]any {
	return map[string]any{
		"name": volumeName,
		"secret": map[string]any{
			"secretName":  secretName,
			"items":       items,
			"defaultMode": 0o440,
		},
	}
}

// caInjection returns the Secret volume + mount for the egress
// inspection CA and appends the CA-bundle env vars to envSlice. Returns
// nils when no CA secret is configured.
func caInjection(secretName string, envSlice *[]any) (volumes, volumeMounts []any) {
	if secretName == "" {
		return nil, nil
	}
	for _, name := range []string{
		"NODE_EXTRA_CA_CERTS", // Node / Claude Code (additive)
		"SSL_CERT_FILE",       // OpenSSL: python ssl, git-over-openssl, …
		"CURL_CA_BUNDLE",      // curl
		"GIT_SSL_CAINFO",      // git
		"REQUESTS_CA_BUNDLE",  // python requests
	} {
		*envSlice = upsertEnv(*envSlice, name, caContainerPath)
	}
	volumes = []any{
		map[string]any{
			"name": "iterion-egress-ca",
			"secret": map[string]any{
				"secretName": secretName,
				"items": []any{
					map[string]any{"key": caSecretKey, "path": caSecretKey},
				},
			},
		},
	}
	volumeMounts = []any{
		map[string]any{
			"name":      "iterion-egress-ca",
			"mountPath": caMountDir,
			"readOnly":  true,
		},
	}
	return volumes, volumeMounts
}

// BuildCASecret renders the per-run Secret holding the egress
// TLS-inspection CA (the public CA cert only — the private key never
// leaves the runner). Applied by the driver before the pod so the pod
// can mount it. The data is the PEM, base64-encoded under caSecretKey.
func BuildCASecret(namespace, name, runID, friendlyName string, caPEM []byte, owner *OwnerReference) ([]byte, error) {
	if namespace == "" || name == "" {
		return nil, fmt.Errorf("kubernetes: CA secret namespace and name are required")
	}
	labels := map[string]string{
		LabelManaged:   "true",
		LabelRunID:     runID,
		LabelComponent: ComponentSandboxRun,
	}
	if friendlyName != "" {
		labels[LabelRunName] = friendlyName
	}
	meta := map[string]any{
		"name":      name,
		"namespace": namespace,
		"labels":    labels,
	}
	withOwner(meta, owner)
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   meta,
		"type":       "Opaque",
		"data": map[string]any{
			caSecretKey: base64.StdEncoding.EncodeToString(caPEM),
		},
	}
	return json.MarshalIndent(secret, "", "  ")
}

// BuildSecretFilesSecret renders the per-run Secret that holds
// runtime-materialised file secrets. Values are base64-encoded under
// deterministic keys shared with BuildPodManifest; callers must never log
// the returned manifest because it contains plaintext-equivalent data.
func BuildSecretFilesSecret(namespace, name, runID, friendlyName string, files []sandbox.SecretFileMount, owner *OwnerReference) ([]byte, error) {
	if namespace == "" || name == "" {
		return nil, fmt.Errorf("kubernetes: file secret namespace and name are required")
	}
	labels := map[string]string{
		LabelManaged:   "true",
		LabelRunID:     runID,
		LabelComponent: ComponentSandboxRun,
	}
	if friendlyName != "" {
		labels[LabelRunName] = friendlyName
	}
	data := make(map[string]any, len(files))
	for i, sf := range files {
		if len(sf.Value) == 0 {
			return nil, fmt.Errorf("kubernetes: file secret %s has empty payload", sf.Name)
		}
		data[secretFileKey(i, sf.Name)] = base64.StdEncoding.EncodeToString(sf.Value)
	}
	meta := map[string]any{
		"name":      name,
		"namespace": namespace,
		"labels":    labels,
	}
	withOwner(meta, owner)
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   meta,
		"type":       "Opaque",
		"data":       data,
	}
	return json.MarshalIndent(secret, "", "  ")
}

func secretFileKey(i int, name string) string {
	clean := sanitizeVolumeName(name)
	if clean == "" {
		clean = path.Base(name)
		clean = sanitizeVolumeName(clean)
	}
	if clean == "" {
		clean = "secret"
	}
	return fmt.Sprintf("secret-%d-%s", i, clean)
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
// alphabetically for stable diffs in `kubectl describe pod`. Values
// are validated for control characters — mirrors the docker driver's
// discipline (F-SB-5).
func envMapToSlice(env map[string]string) []any {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		// Skip silently rather than fail the whole manifest build —
		// the upstream caller (BuildPodManifest) doesn't return an
		// error for individual env validation. The downstream `env`
		// command in appendEnvPrefix DOES error, so a malformed value
		// is caught there. This sift is defense-in-depth.
		if validateEnvVar(k, env[k]) != nil {
			continue
		}
		out = append(out, map[string]any{"name": k, "value": env[k]})
	}
	return out
}

// appendGitSafeDirectoryEnv appends `safe.directory=<workspace>` to the
// pod's GIT_CONFIG_{COUNT,KEY_n,VALUE_n} environment, preserving any
// entries the spec already carries (their count is read and our entry
// appended after them). A malformed pre-existing GIT_CONFIG_COUNT is a
// hard error — silently renumbering (or skipping) would either clobber
// the workflow's git config or reintroduce the dubious-ownership 128
// this exists to fix.
func appendGitSafeDirectoryEnv(envSlice []any, workspace string) ([]any, error) {
	next := 0
	for _, e := range envSlice {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := m["name"].(string); name == "GIT_CONFIG_COUNT" {
			raw, _ := m["value"].(string)
			n, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("spec env GIT_CONFIG_COUNT=%q is not a non-negative integer; fix it so the workspace safe.directory entry can be appended", raw)
			}
			next = n
			break
		}
	}
	envSlice = upsertEnv(envSlice, fmt.Sprintf("GIT_CONFIG_KEY_%d", next), "safe.directory")
	envSlice = upsertEnv(envSlice, fmt.Sprintf("GIT_CONFIG_VALUE_%d", next), workspace)
	envSlice = upsertEnv(envSlice, "GIT_CONFIG_COUNT", strconv.Itoa(next+1))
	return envSlice, nil
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
