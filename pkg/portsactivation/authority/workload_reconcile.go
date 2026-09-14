package authority

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const ordinaryNATSEnv = "ITERION_NATS_URL"
const systemNATSEnv = "ITERION_CONTRACTS_DISTRIBUTED_SYSTEM_NATS_URL"

type WorkloadBinding struct {
	WorkloadUID string `json:"workload_uid"`
	HolderID    string `json:"holder_id"`
}

// WorkloadReconciliation covers the supported, explicit SecretKeyRef supply
// path. It is not an exhaustive credential-custody or RBAC proof: arbitrary
// code, external holders and undisclosed Secret/ConfigMap contents remain an
// operator trust boundary and need further live verification.
type WorkloadReconciliation struct {
	Bindings []WorkloadBinding `json:"bindings"`
}

type workloadKey struct{ namespace, kind, name string }

type podCredentialSpec struct {
	ServiceAccountName  string                `json:"serviceAccountName"`
	Containers          []credentialContainer `json:"containers"`
	InitContainers      []credentialContainer `json:"initContainers"`
	EphemeralContainers []credentialContainer `json:"ephemeralContainers"`
	Volumes             []struct {
		Secret struct {
			SecretName string `json:"secretName"`
		} `json:"secret"`
		Projected struct {
			Sources []struct {
				Secret struct {
					Name string `json:"name"`
				} `json:"secret"`
			} `json:"sources"`
		} `json:"projected"`
	} `json:"volumes"`
}

type credentialContainer struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	Command []string `json:"command"`
	Args    []string `json:"args"`
	Env     []struct {
		Name      string `json:"name"`
		Value     string `json:"value"`
		ValueFrom struct {
			SecretKeyRef *struct {
				Name     string `json:"name"`
				Key      string `json:"key"`
				Optional bool   `json:"optional"`
			} `json:"secretKeyRef"`
		} `json:"valueFrom"`
	} `json:"env"`
	EnvFrom []struct {
		SecretRef *struct {
			Name string `json:"name"`
		} `json:"secretRef"`
	} `json:"envFrom"`
}

// ReconcileWorkloads checks every declared Kubernetes holder against actual
// listed templates and pods. Pod -> ReplicaSet -> Deployment and Pod -> Job ->
// CronJob chains use UID-bound controlling owner references; an old dormant
// template with a different image or Secret reference cannot inherit a new
// controller's compatible holder declaration.
func ReconcileWorkloads(record *Record, snapshot *WorkloadSnapshot) (*WorkloadReconciliation, error) {
	if record == nil || record.validate() != nil || snapshot == nil ||
		!sameStrings(record.Namespaces, snapshot.Namespaces) || len(snapshot.Workloads) > maxWorkloads {
		return nil, fmt.Errorf("authority: Kubernetes workload reconciliation requires matching bounded authority scope")
	}
	workloads := make(map[workloadKey]Workload, len(snapshot.Workloads))
	for _, workload := range snapshot.Workloads {
		key := workloadKey{workload.Namespace, workload.Kind, workload.Name}
		if !slices.Contains(record.Namespaces, workload.Namespace) || workloads[key].UID != "" ||
			workload.UID == "" || !supportedWorkloadVersion(workload.Kind, workload.APIVersion) ||
			len(workload.podSpec) == 0 {
			return nil, fmt.Errorf("authority: Kubernetes workload reconciliation has an invalid object inventory")
		}
		workloads[key] = workload
	}
	holders := make(map[workloadKey][]CredentialHolder)
	knownSecrets := make(map[string]bool)
	for _, holder := range record.Holders {
		if holder.Kind != "kubernetes" {
			continue
		}
		key := workloadKey{holder.Namespace, holder.WorkloadKind, holder.WorkloadName}
		if workloads[key].UID == "" {
			return nil, fmt.Errorf("authority: Kubernetes authority holder has no matching declared or running workload")
		}
		holders[key] = append(holders[key], holder)
		secretName := strings.Split(strings.Split(holder.CredentialRef, ":")[0], "/")
		knownSecrets[holder.Namespace+"/"+secretName[1]] = true
	}
	result := &WorkloadReconciliation{}
	matched := make(map[[2]string]bool)
	for _, workload := range snapshot.Workloads {
		var spec podCredentialSpec
		if json.Unmarshal(workload.podSpec, &spec) != nil || len(spec.Containers) == 0 {
			return nil, fmt.Errorf("authority: Kubernetes workload reconciliation has an unreadable pod specification")
		}
		if spec.ServiceAccountName == "" {
			spec.ServiceAccountName = "default"
		}
		ancestors, err := workloadAncestors(workload, workloads)
		if err != nil {
			return nil, err
		}
		candidates := make([]CredentialHolder, 0)
		for _, ancestor := range ancestors {
			candidates = append(candidates, holders[workloadKey{ancestor.Namespace, ancestor.Kind, ancestor.Name}]...)
		}
		for _, volume := range spec.Volumes {
			if knownSecrets[workload.Namespace+"/"+volume.Secret.SecretName] && volume.Secret.SecretName != "" {
				return nil, fmt.Errorf("authority: Kubernetes NATS credential is mounted outside the supported explicit environment path")
			}
			for _, source := range volume.Projected.Sources {
				if knownSecrets[workload.Namespace+"/"+source.Secret.Name] && source.Secret.Name != "" {
					return nil, fmt.Errorf("authority: Kubernetes NATS credential is projected outside the supported explicit environment path")
				}
			}
		}
		containers := append(append(slices.Clone(spec.Containers), spec.InitContainers...), spec.EphemeralContainers...)
		for _, container := range containers {
			for _, arg := range append(slices.Clone(container.Command), container.Args...) {
				if strings.Contains(arg, "nats://") || strings.Contains(arg, "tls://") {
					return nil, fmt.Errorf("authority: Kubernetes NATS credential appears in literal launch arguments")
				}
			}
			for _, envFrom := range container.EnvFrom {
				if envFrom.SecretRef != nil && knownSecrets[workload.Namespace+"/"+envFrom.SecretRef.Name] {
					return nil, fmt.Errorf("authority: Kubernetes NATS credential is imported through unsupported envFrom")
				}
			}
			seenEnv := make(map[string]bool)
			for _, env := range container.Env {
				monitored := env.Name == ordinaryNATSEnv || env.Name == systemNATSEnv
				if monitored && seenEnv[env.Name] {
					return nil, fmt.Errorf("authority: Kubernetes NATS environment contains duplicate credential variables")
				}
				seenEnv[env.Name] = true
				ref := env.ValueFrom.SecretKeyRef
				if !monitored {
					if ref != nil && knownSecrets[workload.Namespace+"/"+ref.Name] {
						return nil, fmt.Errorf("authority: Kubernetes NATS credential is supplied under an unreviewed variable")
					}
					continue
				}
				if env.Value != "" || ref == nil || ref.Optional || !dnsSubdomain(ref.Name) || ref.Key == "" {
					return nil, fmt.Errorf("authority: Kubernetes NATS URL requires a mandatory named SecretKeyRef")
				}
				credentialRef := workload.Namespace + "/" + ref.Name + ":" + ref.Key
				var selected *CredentialHolder
				for i := range candidates {
					holder := &candidates[i]
					expectedEnv := ordinaryNATSEnv
					if holder.AccessScope == "authority" {
						expectedEnv = systemNATSEnv
					}
					if expectedEnv == env.Name && holder.CredentialRef == credentialRef &&
						holder.Container == container.Name && holder.ImageDigest == container.Image &&
						holder.ServiceAccount == spec.ServiceAccountName {
						if selected != nil {
							return nil, fmt.Errorf("authority: Kubernetes NATS credential matches ambiguous holder declarations")
						}
						selected = holder
					}
				}
				if selected == nil {
					return nil, fmt.Errorf("authority: Kubernetes NATS credential has an unknown or incompatible workload holder")
				}
				key := [2]string{workload.UID, selected.ID}
				matched[key] = true
				result.Bindings = append(result.Bindings, WorkloadBinding{WorkloadUID: workload.UID, HolderID: selected.ID})
			}
		}
		if len(spec.EphemeralContainers) != 0 && len(candidates) != 0 {
			return nil, fmt.Errorf("authority: Kubernetes authority workload has an unreviewed ephemeral container")
		}
		for _, holder := range holders[workloadKey{workload.Namespace, workload.Kind, workload.Name}] {
			if !matched[[2]string{workload.UID, holder.ID}] {
				return nil, fmt.Errorf("authority: Kubernetes declared credential holder lacks its explicit SecretKeyRef")
			}
		}
	}
	slices.SortFunc(result.Bindings, func(a, b WorkloadBinding) int {
		if c := strings.Compare(a.WorkloadUID, b.WorkloadUID); c != 0 {
			return c
		}
		return strings.Compare(a.HolderID, b.HolderID)
	})
	return result, nil
}

func workloadAncestors(workload Workload, index map[workloadKey]Workload) ([]Workload, error) {
	ancestors := make([]Workload, 0, 8)
	seen := make(map[string]bool)
	for depth := 0; depth < 8; depth++ {
		if seen[workload.UID] {
			return nil, fmt.Errorf("authority: Kubernetes workload ownership contains a cycle")
		}
		seen[workload.UID] = true
		ancestors = append(ancestors, workload)
		var owner *OwnerReference
		for i := range workload.Owners {
			if workload.Owners[i].Controller {
				if owner != nil {
					return nil, fmt.Errorf("authority: Kubernetes workload has ambiguous controlling owners")
				}
				owner = &workload.Owners[i]
			}
		}
		if owner == nil {
			return ancestors, nil
		}
		parent := index[workloadKey{workload.Namespace, owner.Kind, owner.Name}]
		if parent.UID == "" || parent.UID != owner.UID || parent.APIVersion != owner.APIVersion {
			return nil, fmt.Errorf("authority: Kubernetes workload controlling owner is missing or changed")
		}
		workload = parent
	}
	return nil, fmt.Errorf("authority: Kubernetes workload ownership exceeds supported depth")
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
