package authority

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const maxCredentialURLBytes = 8192

// CredentialSecretKey is a named Kubernetes source for one NATS URL. It is
// source evidence only; reading it does not prove exclusive custody or that
// the broker currently accepts this credential.
type CredentialSecretKey struct {
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	Key             string `json:"key"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resource_version"`
	value           []byte
}

func (CredentialSecretKey) String() string     { return "Kubernetes NATS credential [value redacted]" }
func (c CredentialSecretKey) GoString() string { return c.String() }
func (c CredentialSecretKey) Value() []byte    { return bytes.Clone(c.value) }

func (c CredentialSecretKey) Identity() (string, error) {
	if len(c.value) == 0 || len(c.value) > maxCredentialURLBytes ||
		strings.TrimSpace(string(c.value)) != string(c.value) || strings.Contains(string(c.value), ",") {
		return "", fmt.Errorf("Kubernetes NATS credential URL is malformed")
	}
	parsed, err := url.Parse(string(c.value))
	if err != nil || parsed == nil || parsed.Scheme != "nats" && parsed.Scheme != "tls" ||
		parsed.Hostname() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.User == nil || parsed.User.Username() == "" {
		return "", fmt.Errorf("Kubernetes NATS credential URL has unsupported authentication")
	}
	password, ok := parsed.User.Password()
	if !ok || password == "" {
		return "", fmt.Errorf("Kubernetes NATS credential URL requires named userinfo authentication")
	}
	return parsed.User.Username(), nil
}

func ReadCredentialSecretKey(ctx context.Context, kubectlBinary, kubeContext, namespace, name, key string) (*CredentialSecretKey, error) {
	if kubectlBinary == "" || !dnsLabel(namespace) || !dnsSubdomain(name) || !validSecretDataKey(key) {
		return nil, fmt.Errorf("Kubernetes NATS credential requires a literal namespace, Secret name and key")
	}
	args := make([]string, 0, 9)
	if kubeContext != "" {
		args = append(args, "--context", kubeContext)
	}
	args = append(args, "--namespace", namespace, "get", "secret", name, "-o", "json")
	output, err := runKubectl(ctx, kubectlBinary, args, maxSecretResponse)
	if err != nil {
		return nil, fmt.Errorf("Kubernetes NATS credential Secret could not be read")
	}
	var document struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Type       string `json:"type"`
		Metadata   struct {
			Namespace       string `json:"namespace"`
			Name            string `json:"name"`
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if len(output) == 0 || json.Unmarshal(output, &document) != nil ||
		document.APIVersion != "v1" || document.Kind != "Secret" || document.Type != "Opaque" ||
		document.Metadata.Namespace != namespace || document.Metadata.Name != name ||
		document.Metadata.UID == "" || document.Metadata.ResourceVersion == "" {
		return nil, fmt.Errorf("Kubernetes NATS credential Secret identity or shape is unsupported")
	}
	encoded, ok := document.Data[key]
	if !ok || len(encoded) > base64.StdEncoding.EncodedLen(maxCredentialURLBytes) {
		return nil, fmt.Errorf("Kubernetes NATS credential Secret is missing a bounded key")
	}
	value, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(value) == 0 || len(value) > maxCredentialURLBytes {
		return nil, fmt.Errorf("Kubernetes NATS credential Secret has malformed key data")
	}
	credential := &CredentialSecretKey{Namespace: namespace, Name: name, Key: key,
		UID: document.Metadata.UID, ResourceVersion: document.Metadata.ResourceVersion, value: value}
	if _, err := credential.Identity(); err != nil {
		return nil, err
	}
	return credential, nil
}

func validSecretDataKey(key string) bool {
	if len(key) == 0 || len(key) > 253 {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
