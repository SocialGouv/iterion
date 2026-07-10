package runtime

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func workflowHasFileSecrets(wf *ir.Workflow) bool {
	if wf == nil {
		return false
	}
	for _, s := range wf.Secrets {
		if s.IsFile() {
			return true
		}
	}
	return false
}

func addSecretFileMounts(ctx context.Context, spec *sandbox.Spec, wf *ir.Workflow, vars map[string]any) error {
	if spec == nil || wf == nil || len(wf.Secrets) == 0 {
		return nil
	}
	creds, _ := secrets.CredentialsFromContext(ctx)
	names := make([]string, 0, len(wf.Secrets))
	for name := range wf.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	seenMountPaths := map[string]string{}
	for _, name := range names {
		s := wf.Secrets[name]
		if !s.IsFile() {
			continue
		}
		value := ""
		if strings.TrimSpace(s.Value) != "" {
			value = resolveRuntimeSecretValue(s.Value, vars)
		} else {
			value = creds.GenericSecret(name)
		}
		if value == "" {
			if s.Optional {
				// Optional file secret with no resolved value: skip the
				// mount silently so a bot that only needs it on some runs
				// (e.g. a forge token when posting a review) still runs
				// without it. The agent simply won't find the file.
				continue
			}
			return fmt.Errorf("runtime: sandbox: file secret %q has no resolved value; set secrets.%s.value or configure a stored secret named %q (or mark it optional: true)", name, name, name)
		}

		mountPath := secrets.ResolveFileMountPath(name, s.MountPath)
		if !strings.HasPrefix(mountPath, "/") {
			return fmt.Errorf("runtime: sandbox: file secret %q mount_path must be absolute: %q", name, mountPath)
		}
		if strings.ContainsAny(mountPath, "\n\r\x00") {
			return fmt.Errorf("runtime: sandbox: file secret %q mount_path contains a control character", name)
		}
		cleanMountPath := path.Clean(mountPath)
		if cleanMountPath != mountPath || cleanMountPath == "/" {
			return fmt.Errorf("runtime: sandbox: file secret %q mount_path must be a clean absolute file path: %q", name, mountPath)
		}
		if prev := seenMountPaths[cleanMountPath]; prev != "" {
			return fmt.Errorf("runtime: sandbox: file secrets %q and %q resolve to the same mount_path %q", prev, name, cleanMountPath)
		}
		seenMountPaths[cleanMountPath] = name

		if s.Env != "" {
			if spec.Env == nil {
				spec.Env = map[string]string{}
			}
			spec.Env[s.Env] = mountPath
		}
		spec.SecretFiles = append(spec.SecretFiles, sandbox.SecretFileMount{
			Name:      name,
			MountPath: mountPath,
			Env:       s.Env,
			Value:     []byte(value),
		})
	}
	return nil
}

func resolveRuntimeSecretValue(expr string, vars map[string]any) string {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "{{") && strings.HasSuffix(expr, "}}") {
		inner := strings.TrimSpace(expr[2 : len(expr)-2])
		if rest, ok := strings.CutPrefix(inner, "vars."); ok {
			key := strings.TrimSpace(rest)
			if vars == nil {
				return ""
			}
			if v, ok := vars[key]; ok && v != nil {
				return fmt.Sprint(v)
			}
			return ""
		}
	}
	return ir.ExpandEnvWithDefault(expr)
}
