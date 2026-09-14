package authority

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
)

const RecordVersion = 1

// Record is an operator assertion with concrete source and custody entries.
// Parsing it does not verify its completeness, the live broker configuration,
// credential possession, workload state or permission boundaries.
type Record struct {
	Version            int                      `json:"version"`
	DeploymentRevision string                   `json:"deployment_revision"`
	Epoch              uint64                   `json:"epoch"`
	Assertions         CompletenessAssertions   `json:"assertions"`
	Namespaces         []string                 `json:"namespaces"`
	Queue              natsconfig.QueueTopology `json:"queue"`
	Brokers            []Broker                 `json:"brokers"`
	Credentials        []CredentialCustody      `json:"credentials"`
	Holders            []CredentialHolder       `json:"holders"`
	Issuers            []CredentialIssuer       `json:"issuers"`
	PermittedWriters   []OperatorWriter         `json:"permitted_writers"`
}

// These are explicit assertions by trusted deployment operators, not results
// of this parser. The authority adapter must reconcile the concrete entries
// with NATS system observations, Kubernetes and permitted-writer evidence.
type CompletenessAssertions struct {
	BrokerScope       bool `json:"broker_scope"`
	Configuration     bool `json:"configuration"`
	WorkloadScope     bool `json:"workload_scope"`
	CredentialCustody bool `json:"credential_custody"`
	IssuerScope       bool `json:"issuer_scope"`
	WriterScope       bool `json:"writer_scope"`
}

type Broker struct {
	ServerID    string `json:"server_id"`
	ServerName  string `json:"server_name"`
	Namespace   string `json:"namespace"`
	PodName     string `json:"pod_name"`
	Container   string `json:"container"`
	ImageDigest string `json:"image_digest"`
	ConfigPath  string `json:"config_path"`
	sources     natsconfig.Sources
}

func (Broker) String() string     { return "NATS authority broker [sources redacted]" }
func (b Broker) GoString() string { return b.String() }

func (b *Broker) UnmarshalJSON(data []byte) error {
	var wire struct {
		ServerID    string             `json:"server_id"`
		ServerName  string             `json:"server_name"`
		Namespace   string             `json:"namespace"`
		PodName     string             `json:"pod_name"`
		Container   string             `json:"container"`
		ImageDigest string             `json:"image_digest"`
		ConfigPath  string             `json:"config_path"`
		Sources     natsconfig.Sources `json:"sources"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("NATS authority broker has an unsupported shape")
	}
	*b = Broker{ServerID: wire.ServerID, ServerName: wire.ServerName, Namespace: wire.Namespace,
		PodName: wire.PodName, Container: wire.Container, ImageDigest: wire.ImageDigest,
		ConfigPath: wire.ConfigPath, sources: wire.Sources}
	return nil
}

func (b Broker) Sources() natsconfig.Sources {
	files := make(map[string]string, len(b.sources.Files))
	for name, body := range b.sources.Files {
		files[name] = body
	}
	return natsconfig.Sources{Entry: b.sources.Entry, Files: files}
}

// A credential entry is required for every principal in the parsed NATS
// configuration. Holder and issuer IDs are reconciled with their own lists;
// even a disconnected holder remains part of this inventory.
type CredentialCustody struct {
	Account   string   `json:"account"`
	Identity  string   `json:"identity"`
	HolderIDs []string `json:"holder_ids"`
	IssuerIDs []string `json:"issuer_ids"`
}

type CredentialHolder struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"` // kubernetes, cli or automation
	Namespace      string `json:"namespace,omitempty"`
	WorkloadKind   string `json:"workload_kind,omitempty"`
	WorkloadName   string `json:"workload_name,omitempty"`
	Container      string `json:"container,omitempty"`
	ServiceAccount string `json:"service_account,omitempty"`
	ImageDigest    string `json:"image_digest"`
	BuildDigest    string `json:"build_digest"`
	AccessScope    string `json:"access_scope"` // server, runner, cli, automation or authority
	CredentialRef  string `json:"credential_ref"`
}

type CredentialIssuer struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"` // kubernetes or external
	Namespace      string `json:"namespace,omitempty"`
	ServiceAccount string `json:"service_account,omitempty"`
	Identity       string `json:"identity,omitempty"` // named external credential issuer
}

// These are the operator's named permitted writers for the authority Secret.
// Actual RoleBinding/ClusterRoleBinding and permission evidence must agree.
type OperatorWriter struct {
	Kind      string `json:"kind"` // user, group or service_account
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

func (Record) String() string     { return "Kubernetes authority record [sources redacted]" }
func (r Record) GoString() string { return r.String() }

// ParseRecord accepts a closed and bounded wire shape. This is intentionally
// only source validation; callers must not turn a parsed Record into a proof.
func ParseRecord(source SecretSource) (*Record, error) {
	if len(source.material) == 0 || len(source.material) > maxAuthorityMaterial {
		return nil, fmt.Errorf("Kubernetes authority record is missing or oversized")
	}
	if err := rejectDuplicateKeys(source.material); err != nil {
		return nil, err
	}
	var record Record
	decoder := json.NewDecoder(bytes.NewReader(source.material))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("Kubernetes authority record has an unsupported shape")
	}
	if err := record.validate(); err != nil {
		return nil, err
	}
	return &record, nil
}

func (r *Record) validate() error {
	if r == nil || r.Version != RecordVersion || !boundedID(r.DeploymentRevision) || r.Epoch == 0 ||
		!r.Assertions.BrokerScope || !r.Assertions.Configuration || !r.Assertions.WorkloadScope ||
		!r.Assertions.CredentialCustody || !r.Assertions.IssuerScope || !r.Assertions.WriterScope ||
		len(r.Namespaces) == 0 || len(r.Namespaces) > 16 || len(r.Brokers) == 0 || len(r.Brokers) > 16 ||
		len(r.Credentials) == 0 || len(r.Credentials) > 128 || len(r.Holders) == 0 || len(r.Holders) > 256 ||
		len(r.Issuers) == 0 || len(r.Issuers) > 64 || len(r.PermittedWriters) == 0 || len(r.PermittedWriters) > 32 ||
		r.Queue.Validate() != nil {
		return fmt.Errorf("Kubernetes authority record is incomplete or outside the supported profile")
	}
	namespaces := make(map[string]bool, len(r.Namespaces))
	for _, namespace := range r.Namespaces {
		if !dnsLabel(namespace) || namespaces[namespace] {
			return fmt.Errorf("Kubernetes authority record has an invalid namespace inventory")
		}
		namespaces[namespace] = true
	}
	brokers := make(map[string]bool, len(r.Brokers))
	brokerNames := make(map[string]bool, len(r.Brokers))
	brokerPods := make(map[[3]string]bool, len(r.Brokers))
	for _, broker := range r.Brokers {
		pod := [3]string{broker.Namespace, broker.PodName, broker.Container}
		if !boundedID(broker.ServerID) || !boundedID(broker.ServerName) ||
			brokers[broker.ServerID] || brokerNames[broker.ServerName] || brokerPods[pod] ||
			!namespaces[broker.Namespace] || !dnsSubdomain(broker.PodName) || !dnsLabel(broker.Container) ||
			!imageDigest(broker.ImageDigest) || !absoluteConfigPath(broker.ConfigPath) || broker.sources.Validate() != nil {
			return fmt.Errorf("Kubernetes authority record has an invalid broker inventory")
		}
		brokers[broker.ServerID] = true
		brokerNames[broker.ServerName] = true
		brokerPods[pod] = true
	}
	holders := make(map[string]bool, len(r.Holders))
	for _, holder := range r.Holders {
		if !boundedID(holder.ID) || holders[holder.ID] || !imageDigest(holder.ImageDigest) ||
			!sha256Hex(holder.BuildDigest) || !oneOf(holder.AccessScope, "server", "runner", "cli", "automation", "authority") ||
			!boundedID(holder.CredentialRef) {
			return fmt.Errorf("Kubernetes authority record has an invalid holder inventory")
		}
		switch holder.Kind {
		case "kubernetes":
			if !namespaces[holder.Namespace] || !oneOf(holder.WorkloadKind,
				"Pod", "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet", "Job", "CronJob", "ReplicationController") ||
				!dnsSubdomain(holder.WorkloadName) || !dnsLabel(holder.Container) || !dnsSubdomain(holder.ServiceAccount) {
				return fmt.Errorf("Kubernetes authority record has an incomplete workload holder")
			}
			refNamespace, valid := kubeSecretRefNamespace(holder.CredentialRef)
			if !valid || refNamespace != holder.Namespace {
				return fmt.Errorf("Kubernetes authority workload credential reference is unsupported")
			}
		case "cli", "automation":
			if holder.Namespace != "" || holder.WorkloadKind != "" || holder.WorkloadName != "" ||
				holder.Container != "" || holder.ServiceAccount != "" {
				return fmt.Errorf("Kubernetes authority record mixes external and workload holders")
			}
		default:
			return fmt.Errorf("Kubernetes authority record has an unsupported holder kind")
		}
		holders[holder.ID] = true
	}
	issuers := make(map[string]bool, len(r.Issuers))
	for _, issuer := range r.Issuers {
		if !boundedID(issuer.ID) || issuers[issuer.ID] {
			return fmt.Errorf("Kubernetes authority record has an invalid issuer inventory")
		}
		switch issuer.Kind {
		case "kubernetes":
			if !namespaces[issuer.Namespace] || !dnsSubdomain(issuer.ServiceAccount) || issuer.Identity != "" {
				return fmt.Errorf("Kubernetes authority record has an incomplete Kubernetes issuer")
			}
		case "external":
			if issuer.Namespace != "" || issuer.ServiceAccount != "" || !boundedID(issuer.Identity) {
				return fmt.Errorf("Kubernetes authority record mixes external and Kubernetes issuers")
			}
		default:
			return fmt.Errorf("Kubernetes authority record has an unsupported issuer kind")
		}
		issuers[issuer.ID] = true
	}
	writers := make(map[[3]string]bool, len(r.PermittedWriters))
	for _, writer := range r.PermittedWriters {
		key := [3]string{writer.Kind, writer.Namespace, writer.Name}
		if writers[key] || !boundedID(writer.Name) {
			return fmt.Errorf("Kubernetes authority record has invalid permitted writers")
		}
		switch writer.Kind {
		case "service_account":
			if !namespaces[writer.Namespace] || !dnsSubdomain(writer.Name) {
				return fmt.Errorf("Kubernetes authority record has unsupported writer identity")
			}
		case "user", "group":
			if writer.Namespace != "" {
				return fmt.Errorf("Kubernetes authority record has unsupported writer identity")
			}
		default:
			return fmt.Errorf("Kubernetes authority record has unsupported writer kind")
		}
		writers[key] = true
	}
	credentials := make(map[[2]string]bool, len(r.Credentials))
	usedHolders := make(map[string]bool)
	usedIssuers := make(map[string]bool)
	for _, credential := range r.Credentials {
		key := [2]string{credential.Account, credential.Identity}
		if !boundedID(credential.Account) || !boundedID(credential.Identity) || credentials[key] ||
			len(credential.HolderIDs) == 0 || len(credential.HolderIDs) > 64 ||
			len(credential.IssuerIDs) == 0 || len(credential.IssuerIDs) > 32 {
			return fmt.Errorf("Kubernetes authority record has an invalid credential inventory")
		}
		credentials[key] = true
		seenHolders := make(map[string]bool, len(credential.HolderIDs))
		for _, id := range credential.HolderIDs {
			if !holders[id] || seenHolders[id] {
				return fmt.Errorf("Kubernetes authority record has ambiguous or unaccounted holders")
			}
			seenHolders[id] = true
			usedHolders[id] = true
		}
		seenIssuers := make(map[string]bool, len(credential.IssuerIDs))
		for _, id := range credential.IssuerIDs {
			if !issuers[id] || seenIssuers[id] {
				return fmt.Errorf("Kubernetes authority record has ambiguous or unaccounted issuers")
			}
			seenIssuers[id] = true
			usedIssuers[id] = true
		}
	}
	if len(usedHolders) != len(holders) || len(usedIssuers) != len(issuers) {
		return fmt.Errorf("Kubernetes authority record leaves holder or issuer entries unaccounted")
	}
	return nil
}

func boundedID(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '/' || c == ':') {
			return false
		}
	}
	return true
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func kubeSecretRefNamespace(value string) (string, bool) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !dnsLabel(parts[0]) {
		return "", false
	}
	secretAndKey := strings.Split(parts[1], ":")
	if len(secretAndKey) != 2 || !dnsSubdomain(secretAndKey[0]) ||
		len(secretAndKey[1]) == 0 || len(secretAndKey[1]) > 253 {
		return "", false
	}
	for _, c := range secretAndKey[1] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return "", false
		}
	}
	return parts[0], true
}

func sha256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	return strings.Trim(value, "0123456789abcdef") == ""
}

func imageDigest(value string) bool {
	parts := strings.Split(value, "@sha256:")
	return len(parts) == 2 && boundedID(parts[0]) && sha256Hex(parts[1])
}

func absoluteConfigPath(value string) bool {
	return len(value) > 1 && len(value) <= 256 && value[0] == '/' &&
		!strings.Contains(value, "..") && !strings.ContainsAny(value, "\x00\n\r")
}

// Duplicate JSON keys are ambiguous authorization input, including within
// nested source maps. DisallowUnknownFields alone does not reject them.
func rejectDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 64 {
			return fmt.Errorf("Kubernetes authority record exceeds supported depth")
		}
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("Kubernetes authority record contains invalid JSON")
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				// encoding/json also matches struct fields case-insensitively.
				// Reject a second spelling that could overwrite an earlier
				// authority assertion during typed decoding.
				folded := strings.ToLower(name)
				if err != nil || !ok || keys[folded] {
					return fmt.Errorf("Kubernetes authority record has duplicate or invalid keys")
				}
				keys[folded] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("Kubernetes authority record contains invalid delimiters")
		}
		if _, err := decoder.Token(); err != nil {
			return fmt.Errorf("Kubernetes authority record contains invalid JSON")
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("Kubernetes authority record contains trailing content")
	}
	return nil
}
