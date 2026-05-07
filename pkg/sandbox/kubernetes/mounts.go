package kubernetes

import (
	"fmt"
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

	// MountTypeBind is the docker driver's default. Rejected here.
	MountTypeBind MountType = "bind"
)

// parsedMount is the post-parse intermediate representation of one
// `sandbox.mounts` entry.
type parsedMount struct {
	Type     MountType
	Source   string // PVC / ConfigMap / Secret name
	Target   string // path inside the container
	Key      string // optional ConfigMap/Secret key projection
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
		return out, fmt.Errorf("mount %q: type is required (one of pvc, configmap, secret)", s)
	}
	if out.Source == "" {
		return out, fmt.Errorf("mount %q: source is required", s)
	}
	if out.Target == "" {
		return out, fmt.Errorf("mount %q: target is required", s)
	}
	if !strings.HasPrefix(out.Target, "/") {
		return out, fmt.Errorf("mount %q: target %q must be absolute", s, out.Target)
	}
	return out, nil
}

// translateMounts converts the spec's mount strings into the (volumes,
// volumeMounts) pair the pod manifest renderer needs. Bind mounts are
// rejected here — they make sense locally (docker driver) but cannot
// be honoured in cloud since pods have no host filesystem.
func translateMounts(specMounts []string) (volumes []map[string]any, volumeMounts []map[string]any, err error) {
	for i, raw := range specMounts {
		m, perr := parseMount(raw)
		if perr != nil {
			return nil, nil, perr
		}
		if m.Type == MountTypeBind {
			return nil, nil, fmt.Errorf("mount %q: type=bind is not supported in cloud (pods have no host filesystem); use type=pvc with a PersistentVolumeClaim instead — see docs/sandbox.md § sandbox.mounts (V2-7)", raw)
		}

		// Generate a deterministic volume name. Including the index
		// keeps it unique across multiple entries that point at the
		// same source (e.g. two ConfigMap projections of different
		// keys onto different paths).
		volName := fmt.Sprintf("mount-%d-%s", i, sanitizeVolumeName(m.Source))

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
