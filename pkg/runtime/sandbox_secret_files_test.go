package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	secretspkg "github.com/SocialGouv/iterion/pkg/secrets"
)

func TestAddSecretFileMounts_GenericSecret(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"kubeconfig": {
			As:        "file",
			MountPath: "/run/iterion/secrets/kubeconfig",
			Env:       "KUBECONFIG",
		},
	}}
	ctx := secretspkg.WithCredentials(context.Background(), secretspkg.Credentials{
		Generic: map[string]string{"kubeconfig": "apiVersion: v1"},
	})
	var spec sandbox.Spec
	if err := addSecretFileMounts(ctx, &spec, wf, nil); err != nil {
		t.Fatalf("addSecretFileMounts: %v", err)
	}
	if len(spec.SecretFiles) != 1 {
		t.Fatalf("SecretFiles = %+v", spec.SecretFiles)
	}
	sf := spec.SecretFiles[0]
	if sf.Name != "kubeconfig" || sf.MountPath != "/run/iterion/secrets/kubeconfig" || string(sf.Value) != "apiVersion: v1" {
		t.Fatalf("secret file mount not populated: %+v", sf)
	}
	if spec.Env["KUBECONFIG"] != "/run/iterion/secrets/kubeconfig" {
		t.Fatalf("KUBECONFIG env not injected: %+v", spec.Env)
	}
}

func TestAddSecretFileMounts_ValueExpressionAndDefaultPath(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"deploy/key": {
			As:    "file",
			Value: "{{vars.secret_payload}}",
		},
	}}
	var spec sandbox.Spec
	err := addSecretFileMounts(context.Background(), &spec, wf, map[string]any{"secret_payload": "payload"})
	if err != nil {
		t.Fatalf("addSecretFileMounts: %v", err)
	}
	sf := spec.SecretFiles[0]
	if sf.MountPath != "/run/iterion/secrets/deploy_key" || string(sf.Value) != "payload" {
		t.Fatalf("default path/value mismatch: %+v", sf)
	}
}

func TestAddSecretFileMounts_MissingValueFails(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"kubeconfig": {As: "file"},
	}}
	var spec sandbox.Spec
	if err := addSecretFileMounts(context.Background(), &spec, wf, nil); err == nil {
		t.Fatal("expected missing file secret value to fail")
	}
}

func TestAddSecretFileMounts_OptionalUnresolvedSkips(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"gitlab_token": {As: "file", Optional: true},
	}}
	var spec sandbox.Spec
	if err := addSecretFileMounts(context.Background(), &spec, wf, nil); err != nil {
		t.Fatalf("optional unresolved file secret should be skipped, not error: %v", err)
	}
	if len(spec.SecretFiles) != 0 {
		t.Fatalf("no mount expected, got %d", len(spec.SecretFiles))
	}
}

func TestAddSecretFileMounts_DuplicatePathFails(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"a": {As: "file", Value: "one", MountPath: "/run/iterion/secrets/shared"},
		"b": {As: "file", Value: "two", MountPath: "/run/iterion/secrets/shared"},
	}}
	var spec sandbox.Spec
	if err := addSecretFileMounts(context.Background(), &spec, wf, nil); err == nil {
		t.Fatal("expected duplicate mount_path to fail")
	}
}

func TestAddSecretFileMounts_DirtyPathFails(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"kubeconfig": {As: "file", Value: "payload", MountPath: "/run/iterion/secrets/../kubeconfig"},
	}}
	var spec sandbox.Spec
	if err := addSecretFileMounts(context.Background(), &spec, wf, nil); err == nil {
		t.Fatal("expected dirty mount_path to fail")
	}
}
