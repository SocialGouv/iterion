package secrets

import (
	"path"
	"regexp"
	"strings"
)

const SecretFilesMountDir = "/run/iterion/secrets"

var secretFileNameSanitizer = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// SanitizeFileName reduces a secret name to a safe basename for a secret
// file (letters, digits, `_`, `.`, `-`). It is the shared rule behind
// DefaultFileMountPath (sandbox mount) and the host-side materialisation
// used by non-sandbox runs, so the same secret lands under the same
// filename on either path.
func SanitizeFileName(name string) string {
	clean := secretFileNameSanitizer.ReplaceAllString(name, "_")
	clean = strings.Trim(clean, "._-")
	if clean == "" {
		clean = "secret"
	}
	return clean
}

// DefaultFileMountPath returns the stable in-sandbox file path for a
// workflow secret mounted as a file. The path is deterministic so prompts
// can reference it before the sandbox container is started.
func DefaultFileMountPath(name string) string {
	return SecretFilesMountDir + "/" + SanitizeFileName(name)
}

func ResolveFileMountPath(name, override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	return DefaultFileMountPath(name)
}

// RelativeToSecretFilesMountDir returns mountPath relative to the default
// file-secret directory when mountPath lives directly under it. The caller is
// expected to validate mountPath as a clean absolute file path first.
func RelativeToSecretFilesMountDir(mountPath string) (string, bool) {
	clean := path.Clean(mountPath)
	if clean != mountPath {
		return "", false
	}
	prefix := SecretFilesMountDir + "/"
	if !strings.HasPrefix(clean, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(clean, prefix)
	if rel == "" || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
		return "", false
	}
	return rel, true
}
