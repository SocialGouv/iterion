// Package authority reads privileged deployment evidence. Reading an
// authority Secret does not verify its contents or enable native admission.
package authority

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const maxSecretResponse = 2 << 20
const maxAuthorityMaterial = 1 << 20

type SecretSource struct {
	Namespace       string
	Name            string
	UID             string
	ResourceVersion string
	// Material contains authorization sources and credential custody data.
	// Never log or persist it outside this privileged server-side adapter.
	material []byte
}

func (s SecretSource) Material() []byte {
	return bytes.Clone(s.material)
}

func (s SecretSource) String() string {
	return "Kubernetes authority Secret [material redacted]"
}

func (s SecretSource) GoString() string { return s.String() }

func dnsLabel(name string) bool {
	if len(name) == 0 || len(name) > 63 || !alphanumeric(name[0]) || !alphanumeric(name[len(name)-1]) {
		return false
	}
	for i := range name {
		if !alphanumeric(name[i]) && name[i] != '-' {
			return false
		}
	}
	return true
}

func dnsSubdomain(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || !alphanumeric(label[0]) || !alphanumeric(label[len(label)-1]) {
			return false
		}
		for i := range label {
			if !alphanumeric(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func alphanumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

// ReadAuthoritySecret uses the existing kubectl subprocess model and reads
// only the named operator-owned Secret. It carries no acceptance assertion:
// the caller must validate the record, permitted writers and live deployment
// evidence before constructing any distributed activation proof.
func ReadAuthoritySecret(ctx context.Context, kubectlBinary, kubeContext, namespace, name string) (*SecretSource, error) {
	if kubectlBinary == "" || !dnsLabel(namespace) || !dnsSubdomain(name) {
		return nil, fmt.Errorf("authority: Kubernetes authority requires a literal namespace and Secret name")
	}
	args := make([]string, 0, 9)
	if kubeContext != "" {
		args = append(args, "--context", kubeContext)
	}
	args = append(args, "--namespace", namespace, "get", "secret", name, "-o", "json")
	output, err := runKubectl(ctx, kubectlBinary, args, maxSecretResponse)
	if err != nil {
		return nil, fmt.Errorf("authority: Kubernetes authority Secret could not be read")
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
		document.Metadata.UID == "" || document.Metadata.ResourceVersion == "" || len(document.Data) != 1 {
		return nil, fmt.Errorf("authority: Kubernetes authority Secret identity or shape is unsupported")
	}
	encoded, ok := document.Data["authority.json"]
	if !ok || len(encoded) > base64.StdEncoding.EncodedLen(maxAuthorityMaterial) {
		return nil, fmt.Errorf("authority: Kubernetes authority Secret is missing bounded material")
	}
	material, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(material) == 0 || len(material) > maxAuthorityMaterial || !json.Valid(material) {
		return nil, fmt.Errorf("authority: Kubernetes authority Secret material is malformed")
	}
	return &SecretSource{Namespace: namespace, Name: name, UID: document.Metadata.UID,
		ResourceVersion: document.Metadata.ResourceVersion, material: material}, nil
}
