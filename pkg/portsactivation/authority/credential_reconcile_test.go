package authority

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func credentialReaderFixture(namespace, name, key string) *CredentialSecretKey {
	identity := "worker-user"
	if namespace == "trusted" {
		identity = "sys"
	}
	return &CredentialSecretKey{Namespace: namespace, Name: name, Key: key,
		UID: "uid-" + name, ResourceVersion: "19",
		value: []byte("nats://" + identity + ":private-password@nats.example:4222")}
}

func TestKubernetesCredentialReconciliationBindsSecretRevisionsToPrincipals(t *testing.T) {
	record := staticFixture(t)
	result, err := reconcileCredentialSecretsWithReader(t.Context(), record,
		func(_ context.Context, namespace, name, key string) (*CredentialSecretKey, error) {
			return credentialReaderFixture(namespace, name, key), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Bindings) != 2 || len(result.ExternalHolderIDs) != 0 ||
		result.Bindings[0].HolderID != "authority-1" || result.Bindings[0].Identity != "sys" ||
		result.Bindings[1].HolderID != "worker-1" || result.Bindings[1].Identity != "worker-user" {
		t.Fatalf("named Kubernetes Secret keys were not bound to principal custody: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), "private-password") {
		t.Fatal("credential reconciliation leaked a Secret value")
	}
}

func TestKubernetesCredentialReconciliationRefusesWrongIdentityAndChangingSecret(t *testing.T) {
	t.Run("different principal", func(t *testing.T) {
		record := staticFixture(t)
		_, err := reconcileCredentialSecretsWithReader(t.Context(), record,
			func(_ context.Context, namespace, name, key string) (*CredentialSecretKey, error) {
				secret := credentialReaderFixture(namespace, name, key)
				if namespace == "worker" {
					secret.value = []byte("nats://old-user:private-password@nats.example:4222")
				}
				return secret, nil
			})
		if err == nil || strings.Contains(err.Error(), "private-password") {
			t.Fatalf("wrong Kubernetes credential identity was accepted or leaked: %v", err)
		}
	})
	t.Run("changed Secret across keys", func(t *testing.T) {
		record := staticFixture(t)
		extra := record.Holders[1]
		extra.ID = "authority-2"
		extra.CredentialRef = "trusted/nats-system:SECOND_URL"
		record.Holders = append(record.Holders, extra)
		for i := range record.Credentials {
			if record.Credentials[i].Identity == "sys" {
				record.Credentials[i].HolderIDs = append(record.Credentials[i].HolderIDs, extra.ID)
			}
		}
		_, err := reconcileCredentialSecretsWithReader(t.Context(), record,
			func(_ context.Context, namespace, name, key string) (*CredentialSecretKey, error) {
				secret := credentialReaderFixture(namespace, name, key)
				if key == "SECOND_URL" {
					secret.ResourceVersion = "20"
				}
				return secret, nil
			})
		if err == nil {
			t.Fatal("credential inventory combined two revisions of one Kubernetes Secret")
		}
	})
	t.Run("read error remains private", func(t *testing.T) {
		record := staticFixture(t)
		_, err := reconcileCredentialSecretsWithReader(t.Context(), record,
			func(context.Context, string, string, string) (*CredentialSecretKey, error) {
				return nil, errors.New("private-password")
			})
		if err == nil || strings.Contains(err.Error(), "private-password") {
			t.Fatalf("Kubernetes Secret reader diagnostic leaked: %v", err)
		}
	})
}

func TestKubernetesCredentialReconciliationKeepsExternalHoldersUnresolved(t *testing.T) {
	record := staticFixture(t)
	external := record.Holders[0]
	external.ID = "offline-cli"
	external.Kind = "cli"
	external.Namespace, external.WorkloadKind, external.WorkloadName = "", "", ""
	external.Container, external.ServiceAccount = "", ""
	external.AccessScope = "cli"
	external.CredentialRef = "external/offline-cli"
	record.Holders = append(record.Holders, external)
	for i := range record.Credentials {
		if record.Credentials[i].Identity == "worker-user" {
			record.Credentials[i].HolderIDs = append(record.Credentials[i].HolderIDs, external.ID)
		}
	}
	result, err := reconcileCredentialSecretsWithReader(t.Context(), record,
		func(_ context.Context, namespace, name, key string) (*CredentialSecretKey, error) {
			return credentialReaderFixture(namespace, name, key), nil
		})
	if err != nil || len(result.ExternalHolderIDs) != 1 || result.ExternalHolderIDs[0] != external.ID {
		t.Fatalf("external holder was silently treated as Kubernetes-proven: %+v %v", result, err)
	}
}
