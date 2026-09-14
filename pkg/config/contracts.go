package config

import (
	"fmt"
	"net/url"
	"strings"
)

type ContractsConfig struct {
	Distributed DistributedContractsConfig `yaml:"distributed"`
}

// DistributedContractsConfig is server-only privileged authority wiring.
// Empty fields leave distributed native admission disabled; a complete
// configuration is necessary but never sufficient to enable it.
type DistributedContractsConfig struct {
	SystemNATSURL        string   `yaml:"system_nats_url"`
	AuthorityRef         string   `yaml:"authority_ref"`
	KubernetesContext    string   `yaml:"kubernetes_context"`
	KubernetesNamespaces []string `yaml:"kubernetes_namespaces"`
}

func (d DistributedContractsConfig) configured() bool {
	return d.SystemNATSURL != "" || d.AuthorityRef != "" || d.KubernetesContext != "" || len(d.KubernetesNamespaces) != 0
}

func (d DistributedContractsConfig) validate(mode Mode) error {
	if !d.configured() {
		return nil
	}
	if mode != ModeCloud || d.SystemNATSURL == "" || d.AuthorityRef == "" || len(d.KubernetesNamespaces) == 0 {
		return fmt.Errorf("distributed contracts authority requires cloud mode, system NATS URL, authority Secret reference and Kubernetes namespaces")
	}
	parsed, err := url.Parse(d.SystemNATSURL)
	if err != nil || parsed == nil || len(d.SystemNATSURL) > 2048 || strings.Contains(d.SystemNATSURL, ",") ||
		parsed.Scheme != "nats" && parsed.Scheme != "tls" ||
		parsed.Hostname() == "" || parsed.User == nil || parsed.User.Username() == "" ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("distributed contracts system NATS URL requires supported URL userinfo authentication")
	}
	password, hasPassword := parsed.User.Password()
	if !hasPassword || password == "" {
		return fmt.Errorf("distributed contracts system NATS URL requires a userinfo password")
	}
	parts := strings.Split(d.AuthorityRef, "/")
	if len(parts) != 2 || !kubernetesDNSLabel(parts[0]) || !kubernetesDNSSubdomain(parts[1]) {
		return fmt.Errorf("distributed contracts authority Secret reference must be namespace/name")
	}
	seen := make(map[string]bool, len(d.KubernetesNamespaces))
	for _, name := range d.KubernetesNamespaces {
		if !kubernetesDNSLabel(name) || seen[name] {
			return fmt.Errorf("distributed contracts Kubernetes namespaces must be distinct literal names")
		}
		seen[name] = true
	}
	if !seen[parts[0]] {
		return fmt.Errorf("distributed contracts authority Secret namespace is outside the declared scope")
	}
	return nil
}

func kubernetesDNSLabel(name string) bool {
	if len(name) == 0 || len(name) > 63 || !kubernetesAlphaNumeric(rune(name[0])) ||
		!kubernetesAlphaNumeric(rune(name[len(name)-1])) {
		return false
	}
	for _, c := range name {
		if !kubernetesAlphaNumeric(c) && c != '-' {
			return false
		}
	}
	return true
}

func kubernetesAlphaNumeric(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

func kubernetesDNSSubdomain(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || !kubernetesAlphaNumeric(rune(label[0])) ||
			!kubernetesAlphaNumeric(rune(label[len(label)-1])) {
			return false
		}
		for _, c := range label {
			if !kubernetesAlphaNumeric(c) && c != '-' {
				return false
			}
		}
	}
	return true
}
