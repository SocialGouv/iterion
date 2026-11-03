package kubernetes

import (
	"fmt"
	"path"
	"strconv"
	"strings"
)

// MountType discriminates the kinds of `sandbox.mounts` entries the
// kubernetes driver understands (V2-7). Mount strings use the same
// `key=value,key=value,...` shape as the docker driver's
// `--mount` flag, but the type values are k8s-specific.
//
// Bind mounts (the docker default) are explicitly rejected: cloud
// pods don't have host filesystem access, so a bind mount in cloud
// would silently mount nothing. The error message points authors at
// the PVC equivalent.
type MountType string

const (
	// MountTypePVC: source is a PersistentVolumeClaim name in the
	// same namespace as the run pod. The PVC must exist before the
	// pod is admitted; iterion does not provision it. Format:
	//   type=pvc,source=<pvc-name>,target=<container-path>[,readonly]
	MountTypePVC MountType = "pvc"

	// MountTypeConfigMap: source is a ConfigMap name in the same
	// namespace. The whole ConfigMap is mounted as a directory at
	// target. Optional `key=<file>` projects a single key as a file
	// at target (target is then treated as the file path).
	//   type=configmap,source=<cm-name>,target=<container-path>[,key=<key>][,readonly]
	MountTypeConfigMap MountType = "configmap"

	// MountTypeSecret: same shape as ConfigMap but for Secret. Mode
	// is forced to 0400 so the secret bytes never become world-readable
	// inside the pod.
	//   type=secret,source=<sec-name>,target=<container-path>[,key=<key>][,readonly]
	MountTypeSecret MountType = "secret"

	// MountTypeEmptyDir: storage the driver owns — an emptyDir volume at
	// target, no source. This is how a declared read-back path is served
	// on a pod (ADR-143 §2): the container writes a real directory for
	// its whole life and the run drains it at teardown. Optional
	// `size=<quantity>` renders an emptyDir sizeLimit (a Kubernetes
	// quantity, e.g. `256Mi`); unwritten, the volume spends the node's
	// ephemeral storage up to the pod's own limit.
	//   type=emptydir,target=<container-path>[,size=<quantity>]
	MountTypeEmptyDir MountType = "emptydir"

	// MountTypeBind is the docker driver's default. Rejected here.
	MountTypeBind MountType = "bind"
)

// parsedMount is the post-parse intermediate representation of one
// `sandbox.mounts` entry.
type parsedMount struct {
	Type     MountType
	Source   string // PVC / ConfigMap / Secret name (emptydir takes none)
	Target   string // path inside the container
	Key      string // optional ConfigMap/Secret key projection
	Size     string // optional emptyDir sizeLimit (a Kubernetes quantity)
	ReadOnly bool
}

// parseMount parses a single `sandbox.mounts` entry. The format is
// `key=value` pairs separated by commas; whitespace around `=` and
// `,` is tolerated. Unknown keys raise an error so typos don't
// silently degrade to default behaviour.
func parseMount(s string) (parsedMount, error) {
	out := parsedMount{}
	parts := strings.Split(s, ",")
	for _, raw := range parts {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		k, v, ok := strings.Cut(raw, "=")
		if !ok {
			// Bare flag forms — only `readonly` / `ro` supported.
			switch strings.TrimSpace(raw) {
			case "readonly", "ro":
				out.ReadOnly = true
				continue
			}
			return out, fmt.Errorf("mount %q: malformed entry %q (want key=value)", s, raw)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "type":
			out.Type = MountType(v)
		case "source", "src":
			out.Source = v
		case "target", "dst", "destination":
			out.Target = v
		case "key":
			out.Key = v
		case "size":
			out.Size = v
		case "readonly", "ro":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return out, fmt.Errorf("mount %q: invalid readonly=%q (want true/false)", s, v)
			}
			out.ReadOnly = b
		default:
			return out, fmt.Errorf("mount %q: unknown key %q", s, k)
		}
	}
	if out.Type == "" {
		return out, fmt.Errorf("mount %q: type is required (one of pvc, configmap, secret, emptydir)", s)
	}
	// emptydir is the one sourceless type: its storage is the driver's own,
	// never a cluster object a source would name. Anything else written
	// after `source=` on one is an authoring error, not a hint.
	if out.Type == MountTypeEmptyDir {
		if out.Source != "" {
			return out, fmt.Errorf("mount %q: type=emptydir takes no source — its storage is driver-owned (a cluster object is a pvc)", s)
		}
		if out.Key != "" {
			return out, fmt.Errorf("mount %q: type=emptydir takes no key", s)
		}
	} else if out.Source == "" {
		return out, fmt.Errorf("mount %q: source is required", s)
	}
	if out.Size != "" && out.Type != MountTypeEmptyDir {
		return out, fmt.Errorf("mount %q: size= is an emptydir key", s)
	}
	if out.Size != "" && !isKubernetesQuantity(out.Size) {
		return out, fmt.Errorf("mount %q: size %q is not a Kubernetes quantity (want a number with an optional SI or binary suffix — e.g. 128Mi, 1Gi, 500M)", s, out.Size)
	}
	if out.Target == "" {
		return out, fmt.Errorf("mount %q: target is required", s)
	}
	if !strings.HasPrefix(out.Target, "/") {
		return out, fmt.Errorf("mount %q: target %q must be absolute", s, out.Target)
	}
	return out, nil
}

// kubernetesQuantitySuffixes are the SI and binary suffixes a Kubernetes
// quantity carries (the resource.Quantity grammar an emptyDir sizeLimit
// renders): decimal n/u/m/k/M/G/T/P/E, binary Ki/Mi/Gi/Ti/Pi/Ei, or none.
var kubernetesQuantitySuffixes = []string{
	"n", "u", "m", "k", "M", "G", "T", "P", "E",
	"Ki", "Mi", "Gi", "Ti", "Pi", "Ei",
}

// isKubernetesQuantity reports whether s parses as a Kubernetes quantity as
// far as its grammar goes — the admission check keeps a bad `size=` at the
// mount line that named it instead of an API server refusal after creation.
// It accepts a superset of what the API server takes (an exponent may still
// carry a suffix, a leading sign a bare number): a value it lets through
// that the API server still refuses fails there with the same raw error as
// before, while the garbage a quantity never is stops here.
func isKubernetesQuantity(s string) bool {
	rest := s
	if len(rest) > 0 && (rest[0] == '+' || rest[0] == '-') {
		rest = rest[1:]
	}
	i, digits := 0, 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
		digits++
	}
	if i < len(rest) && rest[i] == '.' {
		i++
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return false
	}
	if i < len(rest) && (rest[i] == 'e' || rest[i] == 'E') {
		i++
		if i < len(rest) && (rest[i] == '+' || rest[i] == '-') {
			i++
		}
		exp := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
			exp++
		}
		if exp == 0 {
			return false
		}
	}
	rest = rest[i:]
	if rest == "" {
		return true
	}
	for _, suf := range kubernetesQuantitySuffixes {
		if rest == suf {
			return true
		}
	}
	return false
}

// translateMounts converts the spec's mount strings into the (volumes,
// volumeMounts) pair the pod manifest renderer needs. Bind mounts are
// rejected here — they make sense locally (docker driver) but cannot
// be honoured in cloud since pods have no host filesystem.
func translateMounts(specMounts []string) (volumes []map[string]any, volumeMounts []map[string]any, err error) {
	// One volumeMount per mountPath: two mounts at the same target build a
	// pod the API server refuses ("path is already in use"), and refusing
	// here names the mount lines instead of surfacing a raw API error
	// after creation. Nested targets (a mount inside another's tree) stay
	// the API server's own call.
	targets := make(map[string]bool, len(specMounts))
	for i, raw := range specMounts {
		m, perr := parseMount(raw)
		if perr != nil {
			return nil, nil, perr
		}
		if m.Type == MountTypeBind {
			return nil, nil, fmt.Errorf("mount %q: type=bind is not supported in cloud (pods have no host filesystem); use type=pvc with a PersistentVolumeClaim instead — see docs/sandbox.md § sandbox.mounts (V2-7)", raw)
		}
		clean := path.Clean(m.Target)
		if targets[clean] {
			return nil, nil, fmt.Errorf("mount %q: target %q is already served by another mount — two mounts at one path build a pod the API server refuses", raw, m.Target)
		}
		targets[clean] = true

		// Generate a deterministic volume name. Including the index
		// keeps it unique across multiple entries that point at the
		// same source (e.g. two ConfigMap projections of different
		// keys onto different paths). An emptydir names no source —
		// the index alone names its volume, suffixed so the name stays
		// a valid DNS-1123 label (no trailing dash).
		volName := fmt.Sprintf("mount-%d-%s", i, sanitizeVolumeName(m.Source))
		if m.Type == MountTypeEmptyDir {
			volName = fmt.Sprintf("mount-%d-emptydir", i)
		}

		vm := map[string]any{
			"name":      volName,
			"mountPath": m.Target,
		}
		if m.ReadOnly {
			vm["readOnly"] = true
		}
		// For ConfigMap/Secret with key projection, mountPath is the
		// file path and we use subPath to project a single key.
		if m.Key != "" && (m.Type == MountTypeConfigMap || m.Type == MountTypeSecret) {
			vm["subPath"] = m.Key
		}
		volumeMounts = append(volumeMounts, vm)

		switch m.Type {
		case MountTypeEmptyDir:
			vol := map[string]any{
				"name":     volName,
				"emptyDir": map[string]any{},
			}
			if m.Size != "" {
				vol["emptyDir"].(map[string]any)["sizeLimit"] = m.Size
			}
			volumes = append(volumes, vol)

		case MountTypePVC:
			vol := map[string]any{
				"name": volName,
				"persistentVolumeClaim": map[string]any{
					"claimName": m.Source,
				},
			}
			if m.ReadOnly {
				vol["persistentVolumeClaim"].(map[string]any)["readOnly"] = true
			}
			volumes = append(volumes, vol)

		case MountTypeConfigMap:
			cm := map[string]any{
				"name": m.Source,
			}
			if m.Key != "" {
				cm["items"] = []any{
					map[string]any{"key": m.Key, "path": m.Key},
				}
			}
			volumes = append(volumes, map[string]any{
				"name":      volName,
				"configMap": cm,
			})

		case MountTypeSecret:
			sec := map[string]any{
				"secretName":  m.Source,
				"defaultMode": 0o400,
			}
			if m.Key != "" {
				sec["items"] = []any{
					map[string]any{"key": m.Key, "path": m.Key},
				}
			}
			volumes = append(volumes, map[string]any{
				"name":   volName,
				"secret": sec,
			})

		default:
			return nil, nil, fmt.Errorf("mount %q: unsupported type %q", raw, m.Type)
		}
	}
	return volumes, volumeMounts, nil
}

// sanitizeVolumeName lowercases s and replaces non-DNS-1123-friendly
// chars with dashes so the resulting volume name passes k8s
// validation. Caller still prefixes with "mount-N-" so collisions
// across different sources are impossible.
func sanitizeVolumeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if len(out) > 50 {
		out = out[:50]
	}
	return strings.Trim(out, "-.")
}
