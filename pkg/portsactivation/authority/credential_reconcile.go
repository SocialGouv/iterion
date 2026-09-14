package authority

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

type CredentialSourceBinding struct {
	HolderID        string `json:"holder_id"`
	Account         string `json:"account"`
	Identity        string `json:"identity"`
	Namespace       string `json:"namespace"`
	SecretName      string `json:"secret_name"`
	SecretKey       string `json:"secret_key"`
	SecretUID       string `json:"secret_uid"`
	ResourceVersion string `json:"resource_version"`
}

// CredentialReconciliation names exact Secret revisions and unresolved
// external holders. It cannot prove exclusive possession or broker acceptance.
type CredentialReconciliation struct {
	Bindings          []CredentialSourceBinding `json:"bindings"`
	ExternalHolderIDs []string                  `json:"external_holder_ids"`
}

type credentialKey struct{ namespace, name, key string }
type secretVersion struct{ uid, resourceVersion string }

func ReconcileCredentialSecrets(ctx context.Context, record *Record, kubectlBinary, kubeContext string) (*CredentialReconciliation, error) {
	read := func(ctx context.Context, namespace, name, key string) (*CredentialSecretKey, error) {
		return ReadCredentialSecretKey(ctx, kubectlBinary, kubeContext, namespace, name, key)
	}
	return reconcileCredentialSecretsWithReader(ctx, record, read)
}

func reconcileCredentialSecretsWithReader(ctx context.Context, record *Record,
	read func(context.Context, string, string, string) (*CredentialSecretKey, error)) (*CredentialReconciliation, error) {
	if record == nil || record.validate() != nil || read == nil {
		return nil, fmt.Errorf("authority: Kubernetes NATS credential reconciliation requires a valid operator record")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	principals := make(map[string][]CredentialCustody)
	for _, credential := range record.Credentials {
		for _, holderID := range credential.HolderIDs {
			principals[holderID] = append(principals[holderID], credential)
		}
	}
	readKeys := make(map[credentialKey]*CredentialSecretKey)
	secretVersions := make(map[[2]string]secretVersion)
	result := &CredentialReconciliation{}
	for _, holder := range record.Holders {
		if holder.Kind != "kubernetes" {
			result.ExternalHolderIDs = append(result.ExternalHolderIDs, holder.ID)
			continue
		}
		entries := principals[holder.ID]
		if len(entries) != 1 {
			return nil, fmt.Errorf("authority: Kubernetes NATS holder must map to exactly one configured principal")
		}
		ref := strings.Split(holder.CredentialRef, "/")
		nameKey := strings.Split(ref[1], ":")
		key := credentialKey{ref[0], nameKey[0], nameKey[1]}
		if key.namespace != holder.Namespace {
			return nil, fmt.Errorf("authority: Kubernetes NATS holder references a foreign namespace")
		}
		secret := readKeys[key]
		if secret == nil {
			var err error
			secret, err = read(ctx, key.namespace, key.name, key.key)
			if err != nil || secret == nil {
				return nil, fmt.Errorf("authority: Kubernetes NATS credential source is unavailable")
			}
			if secret.Namespace != key.namespace || secret.Name != key.name || secret.Key != key.key ||
				secret.UID == "" || secret.ResourceVersion == "" {
				return nil, fmt.Errorf("authority: Kubernetes NATS credential source identity changed")
			}
			readKeys[key] = secret
		}
		versionKey := [2]string{key.namespace, key.name}
		version := secretVersion{secret.UID, secret.ResourceVersion}
		if previous, exists := secretVersions[versionKey]; exists && previous != version {
			return nil, fmt.Errorf("authority: Kubernetes NATS Secret changed during credential inventory")
		}
		secretVersions[versionKey] = version
		identity, err := secret.Identity()
		if err != nil || identity != entries[0].Identity {
			return nil, fmt.Errorf("authority: Kubernetes NATS Secret user differs from configured principal custody")
		}
		result.Bindings = append(result.Bindings, CredentialSourceBinding{
			HolderID: holder.ID, Account: entries[0].Account, Identity: identity,
			Namespace: key.namespace, SecretName: key.name, SecretKey: key.key,
			SecretUID: secret.UID, ResourceVersion: secret.ResourceVersion,
		})
	}
	slices.Sort(result.ExternalHolderIDs)
	slices.SortFunc(result.Bindings, func(a, b CredentialSourceBinding) int {
		return strings.Compare(a.HolderID, b.HolderID)
	})
	return result, nil
}
