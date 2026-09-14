package authority

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
)

// BrokerConfigSecret contains private configuration material from one named
// Kubernetes Secret. Only the binding metadata is returned to callers.
type BrokerConfigSecret struct {
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resource_version"`
	data            map[string][]byte
}

func (BrokerConfigSecret) String() string     { return "NATS broker configuration Secret [data redacted]" }
func (s BrokerConfigSecret) GoString() string { return s.String() }

type BrokerConfigBinding struct {
	ServerID       string `json:"server_id"`
	Namespace      string `json:"namespace"`
	SecretName     string `json:"secret_name"`
	SecretUID      string `json:"secret_uid"`
	SecretRevision string `json:"secret_revision"`
	PodUID         string `json:"pod_uid"`
	PodRevision    string `json:"pod_revision"`
}

// ReadBrokerConfigSecret reads one exact Secret and keeps its data private.
// Kubernetes RBAC, writer scope and future changes require separate checks.
func ReadBrokerConfigSecret(ctx context.Context, kubectlBinary, kubeContext, namespace, name string) (*BrokerConfigSecret, error) {
	if kubectlBinary == "" || !dnsLabel(namespace) || !dnsSubdomain(name) {
		return nil, fmt.Errorf("NATS broker configuration requires a named Kubernetes Secret")
	}
	args := make([]string, 0, 9)
	if kubeContext != "" {
		args = append(args, "--context", kubeContext)
	}
	args = append(args, "--namespace", namespace, "get", "secret", name, "-o", "json")
	output, err := runKubectl(ctx, kubectlBinary, args, maxSecretResponse)
	if err != nil {
		return nil, fmt.Errorf("NATS broker configuration Secret could not be read")
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
		document.Metadata.UID == "" || document.Metadata.ResourceVersion == "" ||
		len(document.Data) == 0 || len(document.Data) > natsconfig.MaxSourceFiles {
		return nil, fmt.Errorf("NATS broker configuration Secret has an unsupported identity or shape")
	}
	secret := &BrokerConfigSecret{Namespace: namespace, Name: name,
		UID: document.Metadata.UID, ResourceVersion: document.Metadata.ResourceVersion,
		data: make(map[string][]byte, len(document.Data))}
	size := 0
	for key, encoded := range document.Data {
		if !validSecretDataKey(key) || len(encoded) > base64.StdEncoding.EncodedLen(natsconfig.MaxSourceBytes) {
			return nil, fmt.Errorf("NATS broker configuration Secret contains an unsupported key")
		}
		value, err := base64.StdEncoding.DecodeString(encoded)
		size += len(value)
		if err != nil || size > natsconfig.MaxSourceBytes {
			return nil, fmt.Errorf("NATS broker configuration Secret exceeds the supported source profile")
		}
		secret.data[key] = value
	}
	return secret, nil
}

type brokerVolumeMount struct {
	Name        string `json:"name"`
	MountPath   string `json:"mountPath"`
	ReadOnly    bool   `json:"readOnly"`
	SubPath     string `json:"subPath"`
	SubPathExpr string `json:"subPathExpr"`
}

type brokerSecretVolume struct {
	SecretName string `json:"secretName"`
	Optional   *bool  `json:"optional"`
	Items      []struct {
		Key  string `json:"key"`
		Path string `json:"path"`
	} `json:"items"`
}

// ReconcileBrokerConfigSecrets checks the Pod's declared Secret volume projection
// against every byte of the declared source set. It permits one read-only mount
// at the source root and refuses subPath, shadow mounts and optional sources.
// It does not read kubelet-mounted bytes. Pod status, live loaded digest and
// authority freshness are separate evidence.
func ReconcileBrokerConfigSecrets(ctx context.Context, record *Record, snapshot *WorkloadSnapshot,
	kubectlBinary, kubeContext string) ([]BrokerConfigBinding, error) {
	read := func(ctx context.Context, namespace, name string) (*BrokerConfigSecret, error) {
		return ReadBrokerConfigSecret(ctx, kubectlBinary, kubeContext, namespace, name)
	}
	return reconcileBrokerConfigSecretsWithReader(ctx, record, snapshot, read)
}

func reconcileBrokerConfigSecretsWithReader(ctx context.Context, record *Record, snapshot *WorkloadSnapshot,
	read func(context.Context, string, string) (*BrokerConfigSecret, error)) ([]BrokerConfigBinding, error) {
	if record == nil || record.validate() != nil || snapshot == nil || read == nil ||
		!sameStrings(record.Namespaces, snapshot.Namespaces) {
		return nil, fmt.Errorf("NATS broker configuration requires matching Kubernetes scope")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pods := make(map[[2]string]Workload)
	for _, workload := range snapshot.Workloads {
		if workload.Kind == "Pod" {
			key := [2]string{workload.Namespace, workload.Name}
			if pods[key].UID != "" || workload.UID == "" || workload.ResourceVersion == "" {
				return nil, fmt.Errorf("NATS broker configuration Pod inventory is ambiguous")
			}
			pods[key] = workload
		}
	}
	result := make([]BrokerConfigBinding, 0, len(record.Brokers))
	versions := make(map[[2]string]secretVersion)
	for _, broker := range record.Brokers {
		pod := pods[[2]string{broker.Namespace, broker.PodName}]
		if pod.UID == "" || pod.APIVersion != "v1" {
			return nil, fmt.Errorf("NATS broker configuration has no matching Pod")
		}
		root, okay := brokerSourceRoot(broker.ConfigPath, broker.sources.Entry)
		if !okay {
			return nil, fmt.Errorf("NATS broker configuration entry differs from Pod command path")
		}
		var spec struct {
			Containers []struct {
				Name         string              `json:"name"`
				VolumeMounts []brokerVolumeMount `json:"volumeMounts"`
			} `json:"containers"`
			Volumes []struct {
				Name   string              `json:"name"`
				Secret *brokerSecretVolume `json:"secret"`
			} `json:"volumes"`
		}
		if json.Unmarshal(pod.podSpec, &spec) != nil {
			return nil, fmt.Errorf("NATS broker configuration Pod has an unsupported template")
		}
		volumeName := ""
		containerCount := 0
		for _, container := range spec.Containers {
			if container.Name != broker.Container {
				continue
			}
			containerCount++
			for _, mount := range container.VolumeMounts {
				if mount.MountPath == "" || mount.MountPath != path.Clean(mount.MountPath) {
					return nil, fmt.Errorf("NATS broker configuration has a noncanonical mount path")
				}
				if mount.MountPath == root {
					if volumeName != "" || !mount.ReadOnly || mount.SubPath != "" || mount.SubPathExpr != "" {
						return nil, fmt.Errorf("NATS broker configuration has an unsupported source mount")
					}
					volumeName = mount.Name
				} else {
					// Kubernetes Secret files resolve through ..data symlinks. A
					// second mount anywhere below the source root can shadow a
					// symlink target even when no declared filename is covered.
					if root == "/" || strings.HasPrefix(mount.MountPath, root+"/") {
						return nil, fmt.Errorf("NATS broker configuration projection is shadowed")
					}
					for name := range broker.sources.Files {
						filePath := path.Join(root, name)
						if mount.MountPath == filePath ||
							strings.HasPrefix(filePath, strings.TrimSuffix(mount.MountPath, "/")+"/") {
							return nil, fmt.Errorf("NATS broker configuration path is shadowed by another mount")
						}
					}
				}
			}
		}
		if containerCount != 1 || volumeName == "" {
			return nil, fmt.Errorf("NATS broker configuration has no unique source mount")
		}
		var volume *brokerSecretVolume
		for _, candidate := range spec.Volumes {
			if candidate.Name == volumeName {
				if volume != nil || candidate.Secret == nil {
					return nil, fmt.Errorf("NATS broker configuration source is not one Secret")
				}
				volume = candidate.Secret
			}
		}
		if volume == nil || !dnsSubdomain(volume.SecretName) || volume.Optional != nil && *volume.Optional {
			return nil, fmt.Errorf("NATS broker configuration Secret reference is incomplete")
		}
		secret, err := read(ctx, broker.Namespace, volume.SecretName)
		if err != nil || secret == nil || secret.Namespace != broker.Namespace ||
			secret.Name != volume.SecretName || secret.UID == "" || secret.ResourceVersion == "" {
			return nil, fmt.Errorf("NATS broker configuration Secret is unavailable or changed")
		}
		versionKey := [2]string{secret.Namespace, secret.Name}
		version := secretVersion{secret.UID, secret.ResourceVersion}
		if previous, found := versions[versionKey]; found && previous != version {
			return nil, fmt.Errorf("NATS broker configuration Secret changed during inventory")
		}
		versions[versionKey] = version
		projected := make(map[string][]byte)
		if len(volume.Items) == 0 {
			for key, value := range secret.data {
				projected[key] = value
			}
		} else {
			for _, item := range volume.Items {
				value, found := secret.data[item.Key]
				_, duplicate := projected[item.Path]
				if !found || !fs.ValidPath(item.Path) || duplicate {
					return nil, fmt.Errorf("NATS broker configuration Secret projection is incomplete")
				}
				projected[item.Path] = value
			}
		}
		if len(projected) != len(broker.sources.Files) {
			return nil, fmt.Errorf("NATS broker configuration Secret does not match declared files")
		}
		for name, body := range broker.sources.Files {
			value, found := projected[name]
			if !found || !bytes.Equal(value, []byte(body)) {
				return nil, fmt.Errorf("NATS broker configuration Secret differs from declared sources")
			}
		}
		result = append(result, BrokerConfigBinding{ServerID: broker.ServerID, Namespace: broker.Namespace,
			SecretName: secret.Name, SecretUID: secret.UID, SecretRevision: secret.ResourceVersion,
			PodUID: pod.UID, PodRevision: pod.ResourceVersion})
	}
	slices.SortFunc(result, func(a, b BrokerConfigBinding) int { return strings.Compare(a.ServerID, b.ServerID) })
	return result, nil
}

func brokerSourceRoot(configPath, entry string) (string, bool) {
	if !absoluteConfigPath(configPath) || !fs.ValidPath(entry) ||
		!strings.HasSuffix(configPath, "/"+entry) {
		return "", false
	}
	root := strings.TrimSuffix(configPath, "/"+entry)
	if root == "" {
		root = "/"
	}
	return root, path.Join(root, entry) == configPath
}
